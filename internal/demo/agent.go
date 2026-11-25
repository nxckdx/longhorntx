package demo

import (
	"context"
	"encoding/json"
	"os"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"

	"github.com/nxckdx/longhorntx/internal/agent"
	"github.com/nxckdx/longhorntx/internal/longhorn"
)

// The lhtx agent in the demo (LHTX_DEMO_AGENT=1): it runs in-process with
// a lease that lhtx finds, and Freeze resources behave like on an API
// server – a delete with finalizers only marks them, and they go when the
// last finalizer is removed.

var gvrLease = schema.GroupVersionResource{Group: "coordination.k8s.io", Version: "v1", Resource: "leases"}

func (s *sim) freezeReactors() {
	freezes := s.dyn.Tracker()
	s.dyn.PrependReactor("delete", "freezes", func(act k8stesting.Action) (bool, runtime.Object, error) {
		if isDryRun(act) {
			return false, nil, nil
		}
		name := act.(k8stesting.DeleteActionImpl).GetName()
		o, err := freezes.Get(agent.GVRFreeze, "", name)
		if err != nil {
			return false, nil, nil
		}
		u := o.(*unstructured.Unstructured).DeepCopy()
		if len(u.GetFinalizers()) == 0 {
			return false, nil, nil
		}
		if u.GetDeletionTimestamp() == nil {
			now := metav1.Now()
			u.SetDeletionTimestamp(&now)
			u.SetResourceVersion("")
			if err := freezes.Update(agent.GVRFreeze, u, ""); err != nil {
				return true, nil, err
			}
		}
		return true, u, nil
	})
	patch := k8stesting.ObjectReaction(freezes)
	s.dyn.PrependReactor("patch", "freezes", func(act k8stesting.Action) (bool, runtime.Object, error) {
		ok, obj, err := patch(act)
		if err != nil || isDryRun(act) {
			return ok, obj, err
		}
		if u, isU := obj.(*unstructured.Unstructured); isU && u.GetDeletionTimestamp() != nil && len(u.GetFinalizers()) == 0 {
			_ = freezes.Delete(agent.GVRFreeze, "", u.GetName())
		}
		return ok, obj, err
	})
}

// startAgent runs the agent and keeps its lease fresh.
func startAgent(ctx context.Context, c *longhorn.Client) {
	if os.Getenv("LHTX_DEMO_AGENT") == "" {
		return
	}
	a := agent.New(c, "lhtx-agent-7c9f6d5b8-x2k4q")
	a.Logf = func(string, ...any) {}
	a.Poll = time.Second
	renew := func(create bool) {
		l := u(map[string]any{"apiVersion": "coordination.k8s.io/v1", "kind": "Lease",
			"metadata": map[string]any{"name": agent.LeaseName, "namespace": ns},
			"spec": map[string]any{"holderIdentity": "lhtx-agent-7c9f6d5b8-x2k4q",
				"renewTime": time.Now().UTC().Format(time.RFC3339Nano), "leaseDurationSeconds": int64(30)}})
		if create {
			_, _ = c.Dynamic().Resource(gvrLease).Namespace(ns).Create(ctx, l, metav1.CreateOptions{})
			return
		}
		b, _ := json.Marshal(map[string]any{"spec": l.Object["spec"]})
		_, _ = c.Dynamic().Resource(gvrLease).Namespace(ns).Patch(ctx, agent.LeaseName, "application/merge-patch+json", b, metav1.PatchOptions{})
	}
	renew(true)
	// installed with the agent: vouches for Switch requesters
	_, _ = c.Dynamic().Resource(gvrVAP).Create(ctx, u(map[string]any{"apiVersion": "admissionregistration.k8s.io/v1",
		"kind": "ValidatingAdmissionPolicy", "metadata": map[string]any{"name": agent.RequesterPolicy}}), metav1.CreateOptions{})
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				renew(false)
			}
		}
	}()
	go func() { _ = a.Run(ctx) }()
}
