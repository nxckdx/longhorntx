package switchover

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"github.com/nxckdx/longhorntx/internal/longhorn"
)

// Options control a run.
type Options struct {
	// Log receives one line per step and progress update.
	Log func(format string, a ...any)
	// Journal is a file the plan and every finished step are written to
	// ("" = none), so a crash leaves a record of what to undo.
	Journal string
	// Wait bounds every wait for pods/PVCs (default 10 minutes); waiting
	// for a restore is unbounded (cancel with ctx).
	Wait time.Duration
	// Poll is the interval of the status checks (default 3 s).
	Poll time.Duration
}

// journal is what the journal file holds.
type journal struct {
	Plan  *Plan     `json:"plan"`
	Steps []string  `json:"steps"`
	Error string    `json:"error,omitempty"`
	At    time.Time `json:"updated"`
}

type runner struct {
	ctx context.Context
	c   *longhorn.Client
	dyn dynamic.Interface
	p   *Plan
	o   Options
	j   journal
	// what was done and has to be undone if a later step fails before
	// the old PVC is gone
	suspended []FluxRef
	scaled    []Workload
}

// pastReturn marks a failure after the old PVC's delete was accepted:
// no automatic rollback any more.
type pastReturn struct{ error }

// ErrBlocked is returned by Run when the plan has blockers.
var ErrBlocked = errors.New("the switch is blocked")

// Run executes the plan. Until the old PVC is deleted every failure is
// rolled back (workloads scaled up again, Flux resumed); after that point
// the error explains the state and how to continue or switch back.
func Run(ctx context.Context, c *longhorn.Client, p *Plan, o Options) error {
	if len(p.Blockers) > 0 {
		return fmt.Errorf("%w: %s", ErrBlocked, strings.Join(p.Blockers, "; "))
	}
	if o.Log == nil {
		o.Log = func(string, ...any) {}
	}
	if o.Wait == 0 {
		o.Wait = 10 * time.Minute
	}
	if o.Poll == 0 {
		o.Poll = 3 * time.Second
	}
	r := &runner{ctx: ctx, c: c, dyn: c.Dynamic(), p: p, o: o, j: journal{Plan: p}}
	err := r.run()
	if err != nil {
		r.j.Error = err.Error()
		r.save()
	}
	return err
}

func (r *runner) step(format string, a ...any) {
	s := fmt.Sprintf(format, a...)
	r.o.Log("✔ %s", s)
	r.j.Steps = append(r.j.Steps, s)
	r.save()
}

func (r *runner) save() {
	if r.o.Journal == "" {
		return
	}
	r.j.At = time.Now()
	b, err := json.MarshalIndent(r.j, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(r.o.Journal), 0o755)
	_ = os.WriteFile(r.o.Journal, b, 0o644)
}

func (r *runner) run() error {
	p := r.p
	if err := r.waitTargetReady(); err != nil {
		return err
	}

	// --- reversible part ---------------------------------------------
	if err := r.suspendFlux(); err != nil {
		return r.rollback(err)
	}
	if err := r.scaleDown(); err != nil {
		return r.rollback(err)
	}
	if err := r.waitUnused(); err != nil {
		return r.rollback(err)
	}
	if p.OldReclaim != "Retain" && p.OldPV != "" {
		if err := r.patch(gvrPV, "", p.OldPV, `{"spec":{"persistentVolumeReclaimPolicy":"Retain"}}`); err != nil {
			return r.rollback(fmt.Errorf("set Retain on PV %s: %w", p.OldPV, err))
		}
		r.step("PV %s: reclaim policy Retain (keeps the old volume)", p.OldPV)
	}

	// --- point of no return: the old PVC goes --------------------------
	if err := r.deleteOldPVC(); err != nil {
		var pe pastReturn
		if errors.As(err, &pe) {
			return r.stuck(pe.error)
		}
		return r.rollback(err)
	}
	if err := r.bindNew(); err != nil {
		return r.stuck(err)
	}
	if err := r.scaleUp(); err != nil {
		return r.stuck(err)
	}
	if err := r.resumeFlux(); err != nil {
		return r.stuck(err)
	}
	r.waitReady() // only reports
	r.o.Log("done: %s now uses %s; the old volume %s is kept (detached) – switch back with: lhtx --rw switch %s --to-volume %s",
		p.PVCRef(), p.NewVolume, p.OldVolume, p.PVCRef(), p.OldVolume)
	r.step("finished")
	return nil
}

// rollback undoes scaling and suspending (before the old PVC was deleted).
func (r *runner) rollback(cause error) error {
	r.o.Log("✖ %v – rolling back", cause)
	// also after Ctrl+C: undo with a fresh context
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.ctx), 2*time.Minute)
	defer cancel()
	r.ctx = ctx
	var errs []string
	for _, w := range r.scaled {
		if err := r.setReplicas(w, w.Replicas); err != nil {
			errs = append(errs, err.Error())
		} else {
			r.o.Log("↺ %s scaled back to %d", w, w.Replicas)
		}
	}
	for _, f := range r.suspended {
		if err := setSuspend(r.ctx, r.dyn, f, false); err != nil {
			errs = append(errs, err.Error())
		} else {
			r.o.Log("↺ %s resumed", f)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%w; rollback incomplete: %s (journal: %s)", cause, strings.Join(errs, "; "), r.o.Journal)
	}
	return fmt.Errorf("%w (rolled back, nothing changed)", cause)
}

// stuck explains the state after the point of no return.
func (r *runner) stuck(cause error) error {
	p := r.p
	return fmt.Errorf("%w – the old PVC is already deleted; old volume %s is kept (PV %s, Retain). "+
		"Fix the cause and run the same switch again, or switch back with: lhtx --rw switch %s --to-volume %s. "+
		"Workloads/Flux may still be scaled down/suspended (journal: %s)",
		cause, p.OldVolume, p.OldPV, p.PVCRef(), p.OldVolume, r.o.Journal)
}

// waitTargetReady waits until the restore/clone into the target finished
// and the volume is detached.
func (r *runner) waitTargetReady() error {
	last := time.Time{}
	for {
		v, err := r.c.Volume(r.ctx, r.p.NewVolume)
		if err != nil {
			return fmt.Errorf("target volume: %w", err)
		}
		busy := v.Status.RestoreRequired || strings.HasPrefix(v.Status.CloneStatus.State, "initiated") ||
			strings.HasPrefix(v.Status.CloneStatus.State, "copy-completed")
		if v.Status.CloneStatus.State == "failed" {
			return fmt.Errorf("cloning into %s failed", v.Name)
		}
		if !busy && v.Status.State == "detached" {
			if v.Status.Robustness == "faulted" {
				return fmt.Errorf("target volume %s is faulted", v.Name)
			}
			r.step("target volume %s is ready (%s)", v.Name, human(int64(v.Spec.Size)))
			return nil
		}
		if time.Since(last) > 15*time.Second {
			last = time.Now()
			r.o.Log("… waiting for %s: %s%s", v.Name, v.Status.State, r.restoreProgress(v.Name))
		}
		if err := r.sleep(); err != nil {
			return err
		}
	}
}

// restoreProgress reads the engine's restore status (", restore 42%").
func (r *runner) restoreProgress(vol string) string {
	engs, err := r.c.Engines(r.ctx, vol)
	if err != nil || len(engs) == 0 {
		return ""
	}
	minP := -1
	for _, rs := range engs[0].Status.RestoreStatus {
		if minP < 0 || rs.Progress < minP {
			minP = rs.Progress
		}
	}
	if minP < 0 {
		return ""
	}
	return fmt.Sprintf(", restore %d%%", minP)
}

func (r *runner) sleep() error {
	select {
	case <-r.ctx.Done():
		return r.ctx.Err()
	case <-time.After(r.o.Poll):
		return nil
	}
}

func (r *runner) suspendFlux() error {
	for _, f := range r.p.Flux {
		if f.WasSuspended {
			continue
		}
		if err := setSuspend(r.ctx, r.dyn, f, true); err != nil {
			return fmt.Errorf("suspend %w", err)
		}
		r.suspended = append(r.suspended, f)
		r.step("%s suspended", f)
	}
	return nil
}

func (r *runner) resumeFlux() error {
	for _, f := range r.suspended {
		if err := setSuspend(r.ctx, r.dyn, f, false); err != nil {
			return fmt.Errorf("resume %w", err)
		}
		r.step("%s resumed (reconcile requested)", f)
	}
	r.suspended = nil
	return nil
}

func (r *runner) setReplicas(w Workload, n int64) error {
	return r.patch(w.gvr(), r.p.Namespace, w.Name, fmt.Sprintf(`{"spec":{"replicas":%d}}`, n))
}

func (r *runner) scaleDown() error {
	for _, w := range r.p.Workloads {
		if err := r.setReplicas(w, 0); err != nil {
			return fmt.Errorf("scale %s to 0: %w", w, err)
		}
		r.scaled = append(r.scaled, w)
		r.step("%s scaled to 0 (was %d)", w, w.Replicas)
	}
	return nil
}

func (r *runner) scaleUp() error {
	for _, w := range r.scaled {
		if err := r.setReplicas(w, w.Replicas); err != nil {
			return fmt.Errorf("scale %s to %d: %w", w, w.Replicas, err)
		}
		r.step("%s scaled to %d", w, w.Replicas)
	}
	r.scaled = nil
	return nil
}

// waitUnused waits until no pod mounts the PVC and the old volume is
// detached.
func (r *runner) waitUnused() error {
	deadline := time.Now().Add(r.o.Wait)
	for {
		pods, err := r.dyn.Resource(gvrPod).Namespace(r.p.Namespace).List(r.ctx, metav1.ListOptions{})
		if err != nil {
			return err
		}
		var using []string
		for i := range pods.Items {
			if mountsClaim(&pods.Items[i], r.p.PVC) {
				using = append(using, pods.Items[i].GetName())
			}
		}
		state := "detached"
		if r.p.OldVolume != "" {
			if v, err := r.c.Volume(r.ctx, r.p.OldVolume); err == nil {
				state = v.Status.State
			}
		}
		if len(using) == 0 && state == "detached" {
			r.step("no pod uses the PVC, volume %s is detached", r.p.OldVolume)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("still in use after %s: pods %s, volume %s", r.o.Wait, strings.Join(using, ","), state)
		}
		if err := r.sleep(); err != nil {
			return err
		}
	}
}

func (r *runner) deleteOldPVC() error {
	res := r.dyn.Resource(gvrPVC).Namespace(r.p.Namespace)
	cur, err := res.Get(r.ctx, r.p.PVC, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("PVC: %w", err)
	}
	if vn, _, _ := unstructured.NestedString(cur.Object, "spec", "volumeName"); vn != r.p.OldPV {
		return fmt.Errorf("the PVC changed meanwhile (now bound to %s)", vn)
	}
	uid := cur.GetUID()
	if err := res.Delete(r.ctx, r.p.PVC, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}); err != nil {
		return fmt.Errorf("delete PVC: %w", err)
	}
	deadline := time.Now().Add(r.o.Wait)
	for {
		_, err := res.Get(r.ctx, r.p.PVC, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			r.step("old PVC %s deleted (PV %s and volume %s stay)", r.p.PVCRef(), r.p.OldPV, r.p.OldVolume)
			return nil
		}
		if time.Now().After(deadline) {
			// the delete was accepted: from here on there is no way back
			return pastReturn{fmt.Errorf("PVC still terminating after %s (finalizer kubernetes.io/pvc-protection: something still uses it?)", r.o.Wait)}
		}
		if err := r.sleep(); err != nil {
			return err
		}
	}
}

// bindNew creates (or rebinds) the PV of the target volume and the PVC.
func (r *runner) bindNew() error {
	p := r.p
	claimRef := map[string]any{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "namespace": p.Namespace, "name": p.PVC}
	pvName := p.NewVolume
	if p.ReusePV != "" {
		pvName = p.ReusePV
		b, _ := json.Marshal(map[string]any{"spec": map[string]any{"claimRef": map[string]any{
			"apiVersion": "v1", "kind": "PersistentVolumeClaim", "namespace": p.Namespace, "name": p.PVC,
			"uid": nil, "resourceVersion": nil}}})
		if err := r.patch(gvrPV, "", pvName, string(b)); err != nil {
			return fmt.Errorf("rebind PV %s: %w", pvName, err)
		}
		r.step("PV %s reserved for %s", pvName, p.PVCRef())
	} else {
		reclaim := p.OldReclaim
		if reclaim == "" {
			reclaim = "Delete"
		}
		attrs := map[string]any{}
		for k, v := range p.PVAttributes {
			attrs[k] = v
		}
		pv := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1", "kind": "PersistentVolume",
			"metadata": map[string]any{"name": pvName,
				"annotations": map[string]any{"pv.kubernetes.io/provisioned-by": longhornDriver}},
			"spec": map[string]any{
				"capacity":                      map[string]any{"storage": human(p.NewSize)},
				"accessModes":                   toAny(p.AccessModes),
				"volumeMode":                    orDefault(p.VolumeMode, "Filesystem"),
				"persistentVolumeReclaimPolicy": reclaim,
				"storageClassName":              p.StorageClass,
				"claimRef":                      claimRef,
				"csi": map[string]any{"driver": longhornDriver, "fsType": p.FSType, "volumeHandle": p.NewVolume,
					"volumeAttributes": attrs},
			},
		}}
		if _, err := r.dyn.Resource(gvrPV).Create(r.ctx, pv, metav1.CreateOptions{FieldManager: "lhtx"}); err != nil {
			return fmt.Errorf("create PV %s: %w", pvName, err)
		}
		r.step("PV %s created for volume %s", pvName, p.NewVolume)
	}

	labels, ann := map[string]any{}, map[string]any{}
	for k, v := range p.Labels {
		labels[k] = v
	}
	for k, v := range p.Annotations {
		ann[k] = v
	}
	pvc := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "PersistentVolumeClaim",
		"metadata": map[string]any{"name": p.PVC, "namespace": p.Namespace, "labels": labels, "annotations": ann},
		"spec": map[string]any{
			"accessModes":      toAny(p.AccessModes),
			"storageClassName": p.StorageClass,
			"volumeMode":       orDefault(p.VolumeMode, "Filesystem"),
			"volumeName":       pvName,
			"resources":        map[string]any{"requests": map[string]any{"storage": human(p.Request)}},
		},
	}}
	res := r.dyn.Resource(gvrPVC).Namespace(p.Namespace)
	if _, err := res.Create(r.ctx, pvc, metav1.CreateOptions{FieldManager: "lhtx"}); err != nil {
		return fmt.Errorf("create PVC: %w", err)
	}
	deadline := time.Now().Add(r.o.Wait)
	for {
		u, err := res.Get(r.ctx, p.PVC, metav1.GetOptions{})
		if err == nil {
			if phase, _, _ := unstructured.NestedString(u.Object, "status", "phase"); phase == "Bound" {
				r.step("new PVC %s bound to %s (volume %s)", p.PVCRef(), pvName, p.NewVolume)
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("new PVC not Bound after %s", r.o.Wait)
		}
		if err := r.sleep(); err != nil {
			return err
		}
	}
}

// waitReady reports whether the workloads came up (no failure if not).
func (r *runner) waitReady() {
	deadline := time.Now().Add(r.o.Wait)
	for _, w := range r.p.Workloads {
		for {
			u, err := r.dyn.Resource(w.gvr()).Namespace(r.p.Namespace).Get(r.ctx, w.Name, metav1.GetOptions{})
			ready := int64(0)
			if err == nil {
				ready, _, _ = unstructured.NestedInt64(u.Object, "status", "readyReplicas")
			}
			if ready >= w.Replicas {
				r.step("%s ready (%d/%d)", w, ready, w.Replicas)
				break
			}
			if time.Now().After(deadline) || r.ctx.Err() != nil {
				r.o.Log("⚠ %s not ready yet (%d/%d) – check its pods", w, ready, w.Replicas)
				break
			}
			if r.sleep() != nil {
				return
			}
		}
	}
}

func (r *runner) patch(g schema.GroupVersionResource, ns, name, body string) error {
	res := r.dyn.Resource(g)
	var err error
	if ns != "" {
		_, err = res.Namespace(ns).Patch(r.ctx, name, types.MergePatchType, []byte(body), metav1.PatchOptions{FieldManager: "lhtx"})
	} else {
		_, err = res.Patch(r.ctx, name, types.MergePatchType, []byte(body), metav1.PatchOptions{FieldManager: "lhtx"})
	}
	return err
}

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
