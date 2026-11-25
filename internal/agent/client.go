package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"

	"github.com/nxckdx/longhorntx/internal/longhorn"
)

// Info describes a running agent.
type Info struct {
	Holder  string    // pod name
	Renewed time.Time // last heartbeat
}

// Detect looks for a live agent: its lease in the Longhorn namespace,
// renewed within the last minute. One small GET; no agent (or no right to
// read the lease) = nil, and lhtx works on its own.
func Detect(ctx context.Context, c *longhorn.Client) *Info {
	u, err := c.Dynamic().Resource(gvrLease).Namespace(c.Namespace).Get(ctx, LeaseName, metav1.GetOptions{})
	if err != nil {
		return nil
	}
	holder, _, _ := unstructured.NestedString(u.Object, "spec", "holderIdentity")
	renew, _, _ := unstructured.NestedString(u.Object, "spec", "renewTime")
	t, err := time.Parse(time.RFC3339Nano, renew)
	if holder == "" || err != nil || time.Since(t) > time.Minute {
		return nil
	}
	return &Info{Holder: holder, Renewed: t}
}

// Request asks the agent to freeze PVCs: it creates a Freeze with the
// caller's own rights. The name is the UTC time, like local freezes.
func Request(ctx context.Context, c *longhorn.Client, spec Spec, user string) (string, error) {
	name := time.Now().UTC().Format("20060102-150405")
	pvcs := make([]any, 0, len(spec.PVCs))
	for _, p := range slices.Sorted(slices.Values(spec.PVCs)) {
		pvcs = append(pvcs, p)
	}
	sp := map[string]any{"pvcs": pvcs}
	for k, v := range map[string]string{"reason": spec.Reason, "autoUnfreezeAfter": spec.AutoUnfreezeAfter, "wait": spec.Wait} {
		if v != "" {
			sp[k] = v
		}
	}
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "lhtx.io/v1alpha1", "kind": "Freeze",
		"metadata": map[string]any{"name": name, "annotations": map[string]any{AnnRequestedBy: user}},
		"spec":     sp,
	}}
	if _, err := c.Dynamic().Resource(GVRFreeze).Create(ctx, u, metav1.CreateOptions{FieldManager: "lhtx"}); err != nil {
		return "", fmt.Errorf("create Freeze %s: %w", name, err)
	}
	return name, nil
}

// End asks the agent to end a freeze: deleting the Freeze; the agent's
// finalizer keeps it until the apps are back.
func End(ctx context.Context, c *longhorn.Client, name string) error {
	err := c.Dynamic().Resource(GVRFreeze).Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// Forget removes a Freeze without the agent (it is gone): after lhtx
// unfroze locally, the finalizer is dropped so the resource can go.
func Forget(ctx context.Context, c *longhorn.Client, name string) error {
	dyn := c.Dynamic().Resource(GVRFreeze)
	if err := dyn.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	u, err := dyn.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	fins := slices.DeleteFunc(u.GetFinalizers(), func(s string) bool { return s == Finalizer })
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"finalizers": fins}})
	_, err = dyn.Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{FieldManager: "lhtx"})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// List returns all Freeze resources (nil if the CRD is not installed).
func List(ctx context.Context, c *longhorn.Client) ([]*Freeze, error) {
	l, err := c.Dynamic().Resource(GVRFreeze).List(ctx, metav1.ListOptions{})
	if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*Freeze
	for i := range l.Items {
		if f, err := fromU(&l.Items[i]); err == nil {
			out = append(out, f)
		}
	}
	return out, nil
}

// Follow prints the agent's log lines for a Freeze until done says so or
// the Freeze is gone (gone = true); with history the lines already there
// first. It watches just this one object. Ending the follow (ctx) does not
// stop the agent.
func Follow(ctx context.Context, c *longhorn.Client, name string, history bool, logf func(string, ...any), done func(*Freeze) bool) (last *Freeze, gone bool, err error) {
	seen := -1
	show := func(f *Freeze) {
		first := f.Status.LogCount - len(f.Status.Log)
		if seen < 0 {
			seen = f.Status.LogCount
			if history {
				seen = first
			}
		}
		for i, l := range f.Status.Log {
			if first+i >= seen {
				logf("%s", strings.TrimSpace(l))
			}
		}
		if f.Status.LogCount > seen {
			seen = f.Status.LogCount
		}
	}
	res := c.Dynamic().Resource(GVRFreeze)
	for {
		u, err := res.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return last, true, nil
		}
		if err != nil {
			return last, false, err
		}
		if last, err = fromU(u); err != nil {
			return nil, false, err
		}
		show(last)
		if done(last) {
			return last, false, nil
		}
		w, err := res.Watch(ctx, metav1.ListOptions{FieldSelector: fields.OneTermEqualSelector("metadata.name", name).String(),
			ResourceVersion: u.GetResourceVersion()})
		if err != nil {
			return last, false, err
		}
		for ev := range w.ResultChan() {
			if ev.Type == watch.Deleted {
				w.Stop()
				return last, true, nil
			}
			u, ok := ev.Object.(*unstructured.Unstructured)
			if !ok || u.GetName() != name {
				continue
			}
			if f, err := fromU(u); err == nil {
				last = f
				show(f)
				if done(f) {
					w.Stop()
					return last, false, nil
				}
			}
		}
		if ctx.Err() != nil {
			return last, false, ctx.Err()
		}
		// watch closed by the server: GET again and continue
	}
}

// Settled reports whether a freeze request has finished freezing.
func Settled(f *Freeze) bool {
	return f.Status.Phase == PhaseFrozen || f.Status.Phase == PhaseFailed
}
