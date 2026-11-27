package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"

	"github.com/nxckdx/longhorntx/internal/longhorn"
)

// RWX: the share managers (NFS servers of RWX volumes) and their pods go
// into the volume's history – an error state, a restart of the NFS server
// without the volume being detached, and the pod's container restarts (each
// one means client pods saw I/O errors or stale handles). Two small
// watches: ShareManagers and the share-manager pods (label-selected).

// shareEvents: notable ShareManager state changes. Normal lifecycle
// (stopped → starting → running → stopping → stopped) is not recorded.
func shareEvents(old, new *longhorn.ShareManager, now time.Time) []longhorn.HistoryEvent {
	o, n := old.Status.State, new.Status.State
	if o == n {
		return nil
	}
	if n == "error" || o == "error" || (o == "running" && n == "starting") {
		return []longhorn.HistoryEvent{{At: now.UTC(), Kind: longhorn.HistShareManager, Text: fmt.Sprintf("share manager %s → %s", o, n)}}
	}
	return nil
}

// sharePodEvents: container restarts of a share-manager pod.
func sharePodEvents(old, new *corev1.Pod, now time.Time) []longhorn.HistoryEvent {
	var oc, nc int32
	for _, cs := range old.Status.ContainerStatuses {
		oc += cs.RestartCount
	}
	why := ""
	for _, cs := range new.Status.ContainerStatuses {
		nc += cs.RestartCount
		if t := cs.LastTerminationState.Terminated; t != nil {
			why = fmt.Sprintf(" (%s, exit %d)", t.Reason, t.ExitCode)
		}
	}
	if nc <= oc {
		return nil
	}
	return []longhorn.HistoryEvent{{At: now.UTC(), Kind: longhorn.HistShareManager,
		Text: fmt.Sprintf("share-manager pod %s restarted on %s%s – clients may have seen I/O errors", new.Name, new.Spec.NodeName, why)}}
}

// watchShares adds the ShareManager and share-manager pod watches to the
// history. Without the rights (older RBAC) it logs and does nothing.
func (a *Agent) watchShares(ctx context.Context, r *recorder, volume func(string) *longhorn.Volume, ready func() bool) {
	dyn := a.c.Dynamic()
	if _, err := dyn.Resource(longhorn.GVRShareManager).Namespace(a.c.Namespace).List(ctx, metav1.ListOptions{Limit: 1}); err != nil {
		a.Logf("share managers not readable (%v) – no RWX history", err)
		return
	}
	f := dynamicinformer.NewFilteredDynamicSharedInformerFactory(dyn, 0, a.c.Namespace, nil)
	sinf := f.ForResource(longhorn.GVRShareManager).Informer()
	a.metrics.shares = sinf.GetStore()
	_, _ = sinf.AddEventHandler(cache.ResourceEventHandlerFuncs{UpdateFunc: func(o, n any) {
		if !ready() {
			return
		}
		os, err1 := convertU[longhorn.ShareManager](o)
		ns, err2 := convertU[longhorn.ShareManager](n)
		if err1 == nil && err2 == nil {
			r.record(volume(ns.Name), shareEvents(os, ns, time.Now()))
		}
	}})
	pf := dynamicinformer.NewFilteredDynamicSharedInformerFactory(dyn, 0, a.c.Namespace, func(o *metav1.ListOptions) {
		o.LabelSelector = longhorn.LabelShareManager
	})
	pinf := pf.ForResource(longhorn.GVRPod).Informer()
	_, _ = pinf.AddEventHandler(cache.ResourceEventHandlerFuncs{UpdateFunc: func(o, n any) {
		if !ready() {
			return
		}
		op, err1 := convertU[corev1.Pod](o)
		np, err2 := convertU[corev1.Pod](n)
		if err1 == nil && err2 == nil {
			r.record(volume(np.Labels[longhorn.LabelShareManager]), sharePodEvents(op, np, time.Now()))
		}
	}})
	f.Start(ctx.Done())
	pf.Start(ctx.Done())
}

func convertU[T any](o any) (*T, error) {
	u, ok := o.(*unstructured.Unstructured)
	if !ok {
		return nil, fmt.Errorf("not unstructured: %T", o)
	}
	out := new(T)
	return out, runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, out)
}

// shareStates counts share managers by state (metric).
func shareStates(s cache.Store) map[string]int {
	m := map[string]int{}
	for _, o := range s.List() {
		u := o.(*unstructured.Unstructured)
		st, _, _ := unstructured.NestedString(u.Object, "status", "state")
		m[orDash(strings.TrimSpace(st))]++
	}
	return m
}
