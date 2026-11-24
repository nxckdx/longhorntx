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

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	dynfake "k8s.io/client-go/dynamic/fake"
	corefake "k8s.io/client-go/kubernetes/typed/core/v1/fake"
	metafake "k8s.io/client-go/metadata/fake"
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
		longhorn.GVRVolumeAttachment, longhorn.GVRBackupVolume,
	} {
		kinds[g] = kindOf(g) + "List"
	}
	kinds[longhorn.GVRPVC] = "PersistentVolumeClaimList"
	kinds[longhorn.GVRStorageClass] = "StorageClassList"
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

	s := &sim{d: d, dyn: dyn}
	// Expanding a PVC: grow the volume a moment later, like the CSI resizer.
	dyn.PrependReactor("patch", "persistentvolumeclaims", func(a k8stesting.Action) (bool, runtime.Object, error) {
		pa := a.(k8stesting.PatchActionImpl)
		if isDryRun(a) {
			return false, nil, nil
		}
		var p struct {
			Spec struct {
				Resources struct {
					Requests map[string]string `json:"requests"`
				} `json:"resources"`
			} `json:"spec"`
		}
		if json.Unmarshal(pa.GetPatch(), &p) == nil {
			if q, err := resource.ParseQuantity(p.Spec.Resources.Requests["storage"]); err == nil {
				go s.resize(ctx, pa.GetNamespace()+"/"+pa.GetName(), q.Value())
			}
		}
		return false, nil, nil
	})

	// Restore: a volume created from a backup becomes a detached volume;
	// a PVC bound to it shows up in its kubernetesStatus.
	dyn.PrependReactor("create", "volumes", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if !isDryRun(a) {
			if u, ok := a.(k8stesting.CreateActionImpl).GetObject().(*unstructured.Unstructured); ok {
				go s.restored(ctx, u.GetName(), nil)
			}
		}
		return false, nil, nil
	})
	dyn.PrependReactor("create", "persistentvolumeclaims", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if !isDryRun(a) {
			if u, ok := a.(k8stesting.CreateActionImpl).GetObject().(*unstructured.Unstructured); ok {
				if vol, _, _ := unstructured.NestedString(u.Object, "spec", "volumeName"); vol != "" {
					go s.restored(ctx, vol, map[string]any{"namespace": u.GetNamespace(), "pvcName": u.GetName(),
						"pvName": vol, "pvStatus": "Bound"})
				}
			}
		}
		return false, nil, nil
	})

	// Attach/detach: follow the attachment tickets like Longhorn does.
	dyn.PrependReactor("patch", "volumeattachments", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if !isDryRun(a) {
			go s.reconcileAttachment(ctx, a.(k8stesting.PatchActionImpl).GetName())
		}
		return false, nil, nil
	})

	cl := &kube.Cluster{Context: Context, Config: &rest.Config{Host: "demo.invalid"}, Traffic: &kube.Counter{}}
	c := longhorn.NewClientFrom(cl, ns, dyn, &corefake.FakeCoreV1{Fake: &k8stesting.Fake{}})
	// secret names only (backup target credential picker)
	var secrets []runtime.Object
	for _, n := range []string{"backup-s3", "azure-blob", "cifs-nas", "longhorn-webhook-tls"} {
		secrets = append(secrets, &metav1.PartialObjectMetadata{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
			ObjectMeta: metav1.ObjectMeta{Name: n, Namespace: ns}})
	}
	sch := metafake.NewTestScheme()
	sch.AddKnownTypeWithName(schema.GroupVersionKind{Version: "v1", Kind: "Secret"}, &metav1.PartialObjectMetadata{})
	sch.AddKnownTypeWithName(schema.GroupVersionKind{Version: "v1", Kind: "SecretList"}, &metav1.PartialObjectMetadataList{})
	c.SetMetadataClient(metafake.NewSimpleMetadataClient(sch, secrets...))
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
		"engineimages": "EngineImage", "orphans": "Orphan", "volumeattachments": "VolumeAttachment",
		"backupvolumes": "BackupVolume",
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
	dyn *dynfake.FakeDynamicClient
}

func (s *sim) reconcileAttachment(ctx context.Context, name string) {
	time.Sleep(1500 * time.Millisecond)
	u, err := s.dyn.Resource(longhorn.GVRVolumeAttachment).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return
	}
	node := ""
	tickets, _, _ := unstructured.NestedMap(u.Object, "spec", "attachmentTickets")
	for _, t := range tickets {
		if m, ok := t.(map[string]any); ok {
			node, _ = m["nodeID"].(string)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range s.d.volumes {
		if v.Name != name {
			continue
		}
		e := s.d.engines[v.Name]
		state := "stopped"
		if node != "" {
			v.Status.State, v.Status.Robustness, v.Status.CurrentNodeID, v.Spec.NodeID = "attached", "healthy", node, node
			state = "running"
		} else {
			v.Status.State, v.Status.Robustness, v.Status.CurrentNodeID, v.Spec.NodeID = "detached", "unknown", "", ""
		}
		e.Status.CurrentState, e.Spec.NodeID = state, node
		e.Status.CurrentReplicaAddressMap = map[string]string{}
		e.Status.ReplicaModeMap = map[string]string{}
		for _, r := range s.d.replicas[v.Name] {
			r.Status.CurrentState, r.Spec.DesireState = state, state
			s.update(ctx, longhorn.GVRReplica, r)
			if node != "" {
				e.Status.CurrentReplicaAddressMap[r.Name] = fmt.Sprintf("%s:%d", r.Status.StorageIP, r.Status.Port)
				e.Status.ReplicaModeMap[r.Name] = "RW"
			}
		}
		s.update(ctx, longhorn.GVREngine, e)
		s.update(ctx, longhorn.GVRVolume, v)
	}
}

func (s *sim) resize(ctx context.Context, pvc string, size int64) {
	time.Sleep(time.Second)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range s.d.volumes {
		if v.PVC() == pvc && size > int64(v.Spec.Size) {
			v.Spec.Size = longhorn.Quantity(size)
			s.update(ctx, longhorn.GVRVolume, v)
			e := s.d.engines[v.Name]
			e.Spec.VolumeSize = v.Spec.Size
			s.update(ctx, longhorn.GVREngine, e)
		}
	}
}

// restored fills the status of a volume created from a backup (and its
// PVC once one is bound).
func (s *sim) restored(ctx context.Context, name string, ks map[string]any) {
	time.Sleep(600 * time.Millisecond)
	res := s.dyn.Resource(longhorn.GVRVolume).Namespace(ns)
	u, err := res.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return
	}
	size, _, _ := unstructured.NestedString(u.Object, "spec", "size")
	_ = unstructured.SetNestedField(u.Object, "detached", "status", "state")
	_ = unstructured.SetNestedField(u.Object, "unknown", "status", "robustness")
	_ = unstructured.SetNestedField(u.Object, size, "status", "actualSize")
	if ks != nil {
		_ = unstructured.SetNestedMap(u.Object, ks, "status", "kubernetesStatus")
	}
	u.SetResourceVersion(strconv.FormatInt(rv.Add(1)+48211, 10))
	_, _ = res.Update(ctx, u, metav1.UpdateOptions{})
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
