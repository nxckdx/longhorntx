// Package demo provides an in-memory Longhorn cluster with made-up volumes,
// nodes and a rebuild that keeps running, for `lhtx --demo` (screenshots,
// trying lhtx without a cluster). Nothing here talks to a network.
package demo

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	dynfake "k8s.io/client-go/dynamic/fake"
	corefake "k8s.io/client-go/kubernetes/typed/core/v1/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"

	"github.com/nxckdx/longhorntx/internal/kube"
	"github.com/nxckdx/longhorntx/internal/longhorn"
)

// Context is the kubeconfig context name shown in demo mode.
const Context = "demo"

const ns = longhorn.DefaultNamespace

const gi = int64(1) << 30

// NewClient returns a Longhorn client backed by a fake API server filled
// with demo data. A simulator keeps a replica rebuild running until ctx ends.
func NewClient(ctx context.Context) *longhorn.Client {
	d := build(time.Now())
	kinds := map[schema.GroupVersionResource]string{}
	for _, g := range []schema.GroupVersionResource{
		longhorn.GVRVolume, longhorn.GVRReplica, longhorn.GVREngine, longhorn.GVRNode,
		longhorn.GVRSetting, longhorn.GVRSnapshot, longhorn.GVRBackup, longhorn.GVRBackupTarget,
		longhorn.GVRRecurringJob, longhorn.GVREngineImage, longhorn.GVROrphan,
	} {
		kinds[g] = kindOf(g) + "List"
	}
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), kinds, d.objects()...)
	// The fake client ignores label selectors on watches; lhtx relies on
	// them for the per-volume engine watches.
	dyn.PrependWatchReactor("*", func(a k8stesting.Action) (bool, watch.Interface, error) {
		wa := a.(k8stesting.WatchActionImpl)
		w, err := dyn.Tracker().Watch(wa.GetResource(), wa.GetNamespace(), wa.ListOptions)
		if err != nil || wa.WatchRestrictions.Labels == nil || wa.WatchRestrictions.Labels.Empty() {
			return true, w, err
		}
		sel := wa.WatchRestrictions.Labels
		return true, watch.Filter(w, func(e watch.Event) (watch.Event, bool) {
			if m, ok := e.Object.(metav1.Object); ok {
				return e, sel.Matches(labels.Set(m.GetLabels()))
			}
			return e, true
		}), nil
	})
	// Dry runs must not change the demo data either.
	for _, verb := range []string{"create", "patch", "delete"} {
		dyn.PrependReactor(verb, "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
			if isDryRun(a) {
				return true, &unstructured.Unstructured{Object: map[string]any{}}, nil
			}
			return false, nil, nil
		})
	}

	cl := &kube.Cluster{Context: Context, Config: &rest.Config{Host: "demo.invalid"}, Traffic: &kube.Counter{}}
	c := longhorn.NewClientFrom(cl, ns, dyn, &corefake.FakeCoreV1{Fake: &k8stesting.Fake{}})
	s := &sim{d: d, c: c, dyn: dyn}
	go s.run(ctx)
	return c
}

func isDryRun(a k8stesting.Action) bool {
	switch a := a.(type) {
	case k8stesting.CreateActionImpl:
		return len(a.CreateOptions.DryRun) > 0
	case k8stesting.PatchActionImpl:
		return len(a.PatchOptions.DryRun) > 0
	case k8stesting.DeleteActionImpl:
		return len(a.DeleteOptions.DryRun) > 0
	}
	return false
}

func kindOf(g schema.GroupVersionResource) string {
	return map[string]string{
		"volumes": "Volume", "replicas": "Replica", "engines": "Engine", "nodes": "Node",
		"settings": "Setting", "snapshots": "Snapshot", "backups": "Backup",
		"backuptargets": "BackupTarget", "recurringjobs": "RecurringJob",
		"engineimages": "EngineImage", "orphans": "Orphan",
	}[g.Resource]
}

var rv atomic.Int64

// toU converts one of the slim structs into an unstructured object.
func toU(g schema.GroupVersionResource, obj any) *unstructured.Unstructured {
	b, err := json.Marshal(obj)
	if err != nil {
		panic(err)
	}
	u := &unstructured.Unstructured{}
	if err := json.Unmarshal(b, &u.Object); err != nil {
		panic(err)
	}
	u.SetAPIVersion(longhorn.Group + "/" + longhorn.Version)
	u.SetKind(kindOf(g))
	u.SetNamespace(ns)
	u.SetResourceVersion(strconv.FormatInt(rv.Add(1)+48211, 10))
	delete(u.Object["metadata"].(map[string]any), "creationTimestamp")
	return u
}

// hex8 derives stable pseudo-random hex from a name.
func hex8(s string) string {
	h := fnv.New32a()
	h.Write([]byte(s))
	return fmt.Sprintf("%08x", h.Sum32())
}

func uuid(s string) string {
	a, b, c, d := hex8(s), hex8(s+"1"), hex8(s+"2"), hex8(s+"3")
	return fmt.Sprintf("%s-%s-%s-%s-%s%s", a, b[:4], b[4:], c[:4], c[4:], d)
}

// sim mutates the demo data: it keeps one replica rebuild running.
type sim struct {
	mu  sync.Mutex
	d   *data
	c   *longhorn.Client
	dyn *dynfake.FakeDynamicClient
}

func (s *sim) update(ctx context.Context, g schema.GroupVersionResource, obj any) {
	_, _ = s.dyn.Resource(g).Namespace(ns).Update(ctx, toU(g, obj), metav1.UpdateOptions{})
}

func (s *sim) run(ctx context.Context) {
	t := time.NewTicker(400 * time.Millisecond)
	defer t.Stop()
	pause := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.mu.Lock()
		switch r := s.d.rebuild; {
		case r != nil && r.progress < 100:
			r.progress += 1 + int(time.Now().UnixNano()%2)
			if r.progress >= 100 {
				s.finishRebuild(ctx)
				pause = 20
			} else {
				s.d.setRebuild(r)
				s.update(ctx, longhorn.GVREngine, r.engine)
			}
		case pause > 0:
			pause--
		default:
			s.startRebuild(ctx)
		}
		s.mu.Unlock()
	}
}

func (s *sim) finishRebuild(ctx context.Context) {
	r := s.d.rebuild
	now := time.Now().UTC().Format(time.RFC3339)
	r.replica.Spec.HealthyAt = now
	r.replica.Spec.LastFailedAt = ""
	delete(r.engine.Status.RebuildStatus, "tcp://"+r.engine.Status.CurrentReplicaAddressMap[r.replica.Name])
	r.engine.Status.ReplicaModeMap[r.replica.Name] = "RW"
	r.engine.Status.ReplicaTransitionTimeMap[r.replica.Name] = now
	r.vol.Status.Robustness = "healthy"
	s.update(ctx, longhorn.GVRReplica, r.replica)
	s.update(ctx, longhorn.GVREngine, r.engine)
	s.update(ctx, longhorn.GVRVolume, r.vol)
	s.d.rebuild = nil
}

// startRebuild replaces a replica of the next volume in the rotation with a
// fresh one, like Longhorn does after a replica failed.
func (s *sim) startRebuild(ctx context.Context) {
	vol := s.d.nextRebuildVolume()
	if vol == nil {
		return
	}
	old, repl := s.d.replaceReplica(vol)
	_ = s.dyn.Resource(longhorn.GVRReplica).Namespace(ns).Delete(ctx, old.Name, metav1.DeleteOptions{})
	_, _ = s.dyn.Resource(longhorn.GVRReplica).Namespace(ns).Create(ctx, toU(longhorn.GVRReplica, repl), metav1.CreateOptions{})
	s.update(ctx, longhorn.GVREngine, s.d.rebuild.engine)
	s.update(ctx, longhorn.GVRVolume, vol)
}
