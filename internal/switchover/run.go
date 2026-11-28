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
	// KeySettle: how long after the PVC is re-created the key Secret of an
	// encrypted volume is watched before the pods may start (default 20 s).
	KeySettle time.Duration
	// FreezeName names the freeze policy (default lhtx-freeze-<time>); with
	// a name an interrupted switch can be continued (see Continue).
	FreezeName string
	// Request: the agent's Switch resource, stored on the freeze policy.
	Request string
	// AfterFreeze runs once the PVC is frozen and its volume detached,
	// before the old PVC goes (lhtx shrink copies the data here). An error
	// rolls back: the workload starts again on the old volume.
	AfterFreeze func(ctx context.Context) error
	// Handover: the caller stopping (ctx cancelled) hands the run over
	// instead of rolling it back – the lhtx agent losing its lease: the
	// next agent continues from the cluster state, and a rollback of the
	// old one would race with it.
	Handover bool
}

// journal is what the journal file holds.
type journal struct {
	Context    string    `json:"context"` // kubeconfig context
	Plan       *Plan     `json:"plan"`
	Steps      []string  `json:"steps"`
	Error      string    `json:"error,omitempty"`
	RolledBack bool      `json:"rolledBack,omitempty"` // failed, but everything was undone
	At         time.Time `json:"updated"`
}

type runner struct {
	ctx    context.Context
	c      *longhorn.Client
	dyn    dynamic.Interface
	p      *Plan
	o      Options
	j      journal
	freeze *Freeze                    // active while the PVC is swapped
	key    *unstructured.Unstructured // the key Secret as it was (memory only)
	keyFP  string
}

// pastReturn marks a failure after the old PVC's delete was accepted:
// no automatic rollback any more.
type pastReturn struct{ error }

// KeepFrozen wraps an AfterFreeze error after which the workload must not
// start again by itself (its volume is not in a state it can use): no
// rollback, the freeze stays – the error says how to get out.
func KeepFrozen(err error) error { return keepFrozen{err} }

type keepFrozen struct{ error }

func (e keepFrozen) Unwrap() error { return e.error }

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
	if o.KeySettle == 0 {
		o.KeySettle = 20 * time.Second
	}
	r := &runner{ctx: ctx, c: c, dyn: c.Dynamic(), p: p, o: o, j: journal{Plan: p, Context: c.Cluster.Context}}
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
	if err := r.readKey(); err != nil {
		return err
	}

	// --- reversible part ---------------------------------------------
	fo := FreezeOptions{Log: r.o.Log, Wait: r.o.Wait, Poll: r.o.Poll, Name: r.o.FreezeName, Request: r.o.Request}
	f, err := ApplyFreeze(r.ctx, r.c, p.Freeze, fo)
	r.freeze = f
	if err != nil {
		return r.rollback(err)
	}
	r.step("%s frozen (%s), its pods are gone and volume %s is detached", p.PVCRef(), f.Name, p.OldVolume)
	if r.o.AfterFreeze != nil {
		if err := r.o.AfterFreeze(r.ctx); err != nil {
			var kf keepFrozen
			if errors.As(err, &kf) {
				name := r.freeze.Name
				r.freeze = nil
				return fmt.Errorf("%w – %s stays frozen (%s), nothing was rolled back", kf.error, p.PVCRef(), name)
			}
			return r.rollback(err)
		}
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
	if err := r.guardKey(); err != nil {
		var ke keyError
		if errors.As(err, &ke) {
			r.freeze = nil // stays frozen on purpose
			return err
		}
		return r.stuck(err)
	}
	unfreezeErr := Unfreeze(r.ctx, r.c, *f, fo)
	r.freeze = nil
	switch {
	case unfreezeErr != nil && strings.Contains(unfreezeErr.Error(), "remove policy"):
		return fmt.Errorf("the PVC uses %s now, but ending the freeze failed: %w – run lhtx unfreeze %s", p.NewVolume, unfreezeErr, f.Name)
	case unfreezeErr != nil:
		r.step("unfrozen; %v", unfreezeErr)
	default:
		r.step("unfrozen, the workload runs on %s", p.NewVolume)
	}
	r.o.Log("done: %s now uses %s; the old volume %s is kept (detached) – switch back with: lhtx --rw switch %s --to-volume %s",
		p.PVCRef(), p.NewVolume, p.OldVolume, p.PVCRef(), p.OldVolume)
	r.step("finished")
	return nil
}

// readKey keeps the key Secret of an encrypted volume in memory: the
// guard compares it after the PVC is re-created and restores it into a
// backup Secret if a manager replaced it.
func (r *runner) readKey() error {
	if r.p.KeySecret == nil {
		return nil
	}
	k := r.p.KeySecret
	u, err := r.dyn.Resource(gvrSecret).Namespace(k.Namespace).Get(r.ctx, k.Name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("read the key Secret %s to guard it during the switch: %w (nothing changed)", k, err)
	}
	fp, err := longhorn.Fingerprint(u)
	if err != nil {
		return fmt.Errorf("%w (nothing changed)", err)
	}
	r.key, r.keyFP = u, fp
	return nil
}

// guardKey: after the PVC is re-created, whatever manages the key Secret
// may react (re-create it, generate a new key). Before any pod starts,
// lhtx waits KeySettle and checks the key: missing → wait for it to come
// back; another key → keep the old one in a backup Secret and stop, the
// PVC stays frozen.
func (r *runner) guardKey() error {
	if r.key == nil {
		return nil
	}
	k := *r.p.KeySecret
	r.o.Log("… watching the key Secret %s for %s (a manager may react to the new PVC)", k, r.o.KeySettle)
	settle := time.Now().Add(r.o.KeySettle)
	deadline := time.Now().Add(r.o.Wait)
	missingLogged := false
	for {
		fp, err := r.c.KeyFingerprint(r.ctx, k)
		switch {
		case errors.Is(err, longhorn.ErrNoSecret):
			if !missingLogged {
				r.o.Log("… the key Secret %s is gone – waiting for its manager to create it again", k)
				missingLogged = true
			}
		case err != nil && r.ctx.Err() != nil:
			return r.ctx.Err()
		case err != nil:
			r.o.Log("⚠ reading the key Secret: %v", err)
		case fp != r.keyFP:
			name, berr := r.backupKey()
			if berr != nil {
				return fmt.Errorf("the key Secret %s now holds ANOTHER key and keeping a copy of the old one failed: %v – "+
					"the old key is lost unless your secret source keeps versions; the volume needs the old key", k, berr)
			}
			return keyError{fmt.Errorf("the key Secret %s now holds ANOTHER key – whatever manages it generated a new one when the "+
				"PVC was re-created. The volume opens only with the old key, so its pods were not started (%s stays frozen; a wrong "+
				"key does not harm the data, the volume just does not open).\n"+
				"The old key is in Secret %s/%s. Put it back:\n"+
				"  - the manager reads it from a source (Vault, a cloud KMS, Git …): restore the old value there and let it sync\n"+
				"  - the manager generates keys itself (no source): detach the Secret from it (e.g. delete the managing object with\n"+
				"    --cascade=orphan, or stop the policy that re-creates it) and write the old key into %s\n"+
				"Then: lhtx --rw unfreeze %s (it checks the key against the copy first) and delete the copy",
				k, r.p.PVCRef(), k.Namespace, name, k, r.freeze.Name)}
		case time.Now().After(settle):
			r.step("key Secret %s unchanged", k)
			return nil
		}
		if time.Now().After(deadline) {
			name, _ := r.backupKey()
			return fmt.Errorf("the key Secret %s did not come back within %s – the old key is kept in Secret %s/%s", k, r.o.Wait, k.Namespace, name)
		}
		if err := r.sleep(); err != nil {
			return err
		}
	}
}

// keyError: the switch stopped because the key changed – the PVC stays
// frozen and the message says how to recover (no generic "stuck" text).
type keyError struct{ error }

func (e keyError) Unwrap() error { return e.error }

// backupKey writes the old key into a Secret of its own: no owner, so no
// manager touches it. lhtx doctor reminds to remove it.
func (r *runner) backupKey() (string, error) {
	k := r.p.KeySecret
	name := fmt.Sprintf("lhtx-key-backup-%s-%s", r.p.PVC, time.Now().UTC().Format("20060102-150405"))
	if len(name) > 253 {
		name = name[:253]
	}
	obj := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Secret", "type": r.key.Object["type"],
		"metadata": map[string]any{"name": name, "namespace": k.Namespace,
			"labels": map[string]any{longhorn.LabelKeyBackup: "true"},
			"annotations": map[string]any{"lhtx.io/key-of": k.String(), "lhtx.io/pvc": r.p.PVCRef(),
				"lhtx.io/reason": "the key Secret was replaced during lhtx switch"}},
		"data": r.key.Object["data"]}}
	_, err := r.dyn.Resource(gvrSecret).Namespace(k.Namespace).Create(context.WithoutCancel(r.ctx), obj, metav1.CreateOptions{FieldManager: "lhtx"})
	if err == nil {
		r.o.Log("⚠ the old key is kept in Secret %s/%s", k.Namespace, name)
	}
	return name, err
}

// ErrStartOver: Continue found the switch before its point of no return
// (the old PVC is still there) – run it again from the start (Build + Run
// with the same FreezeName takes the freeze over).
var ErrStartOver = errors.New("the old PVC is still there: start the switch over")

// Continue picks a switch up after an interruption (the agent restarted)
// from what the cluster shows: the old PVC still there → ErrStartOver; the
// PVC gone → bind the new one, check the key, unfreeze; the PVC already on
// the target → unfreeze. p is the plan of the interrupted run.
func Continue(ctx context.Context, c *longhorn.Client, p *Plan, o Options) error {
	if o.Log == nil {
		o.Log = func(string, ...any) {}
	}
	if o.Wait == 0 {
		o.Wait = 10 * time.Minute
	}
	if o.Poll == 0 {
		o.Poll = 3 * time.Second
	}
	if o.KeySettle == 0 {
		o.KeySettle = 20 * time.Second
	}
	r := &runner{ctx: ctx, c: c, dyn: c.Dynamic(), p: p, o: o, j: journal{Plan: p, Context: c.Cluster.Context}}
	if o.FreezeName != "" {
		f, err := GetFreeze(ctx, r.dyn, o.FreezeName)
		if err != nil {
			return err
		}
		r.freeze = f
	}
	cur, err := r.dyn.Resource(gvrPVC).Namespace(p.Namespace).Get(ctx, p.PVC, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		o.Log("continuing: the old PVC is gone – binding the new one")
		if err := r.readKey(); err != nil {
			return r.stuck(err)
		}
		if r.key != nil {
			o.Log("⚠ the key Secret was read now, after the interruption – a key replaced before that cannot be noticed")
		}
		if err := r.bindNew(); err != nil {
			return r.stuck(err)
		}
		if err := r.guardKey(); err != nil {
			var ke keyError
			if errors.As(err, &ke) {
				return err
			}
			return r.stuck(err)
		}
	case err != nil:
		return err
	default:
		vn, _, _ := unstructured.NestedString(cur.Object, "spec", "volumeName")
		switch {
		case vn == p.OldPV && cur.GetDeletionTimestamp() == nil:
			return ErrStartOver
		case vn != p.NewVolume && vn != p.ReusePV:
			return fmt.Errorf("the PVC is bound to %s now – neither the old nor the target volume; check it by hand", vn)
		}
		o.Log("continuing: the PVC already uses %s", p.NewVolume)
	}
	if r.freeze == nil {
		r.step("finished (no freeze left to end)")
		return nil
	}
	if err := Unfreeze(ctx, c, *r.freeze, FreezeOptions{Log: o.Log, Wait: o.Wait, Poll: o.Poll}); err != nil {
		return err
	}
	r.step("unfrozen, the workload runs on %s", p.NewVolume)
	r.step("finished")
	return nil
}

// rollback ends the freeze (before the old PVC was deleted): the pods
// start again on the old volume.
func (r *runner) rollback(cause error) error {
	if r.o.Handover && r.ctx.Err() != nil {
		r.o.Log("stopping – the PVC stays frozen, the next run continues")
		return fmt.Errorf("%w (handed over: nothing rolled back)", r.ctx.Err())
	}
	r.o.Log("✖ %v – rolling back", cause)
	if r.freeze == nil {
		r.j.RolledBack = true
		return fmt.Errorf("%w (nothing changed; volume %s stays – delete it if not needed)", cause, r.p.NewVolume)
	}
	// also after Ctrl+C: undo with a fresh context
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.ctx), 5*time.Minute)
	defer cancel()
	err := Unfreeze(ctx, r.c, *r.freeze, FreezeOptions{Log: func(f string, a ...any) { r.o.Log("↺ "+f, a...) }, Wait: 2 * time.Minute, Poll: r.o.Poll})
	if err != nil && strings.Contains(err.Error(), "remove policy") {
		return fmt.Errorf("%w; rollback incomplete: %v – run lhtx unfreeze %s (journal: %s)", cause, err, r.freeze.Name, r.o.Journal)
	}
	r.j.RolledBack = true
	return fmt.Errorf("%w (rolled back: unfrozen, the PVC untouched; volume %s stays – delete it if not needed)", cause, r.p.NewVolume)
}

// stuck explains the state after the point of no return. The freeze stays:
// nothing can start on (or re-create) the PVC until it is fixed.
func (r *runner) stuck(cause error) error {
	p := r.p
	name := ""
	if r.freeze != nil {
		name = r.freeze.Name
	}
	return fmt.Errorf("%w – the old PVC is already deleted; old volume %s is kept (PV %s, Retain). "+
		"The PVC stays frozen (%s) so nothing re-creates it meanwhile. Run the same switch again, or switch back with: "+
		"lhtx --rw switch %s --to-volume %s, then lhtx unfreeze %s (journal: %s)",
		cause, p.OldVolume, p.OldPV, name, p.PVCRef(), p.OldVolume, name, r.o.Journal)
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
			state := v.Status.State
			switch {
			case state == "":
				state = "creating"
			case v.Status.RestoreRequired:
				state = "restoring"
			}
			progress := ""
			if v.Status.RestoreRequired {
				progress = r.restoreProgress(v.Name)
			}
			r.o.Log("… waiting for %s: %s%s", v.Name, state, progress)
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
		csi := map[string]any{"driver": longhornDriver, "fsType": p.FSType, "volumeHandle": p.NewVolume,
			"volumeAttributes": attrs}
		for k, v := range p.SecretRefs {
			csi[k] = v
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
				"csi":                           csi,
			},
		}}
		_, err := r.dyn.Resource(gvrPV).Create(r.ctx, pv, metav1.CreateOptions{FieldManager: "lhtx"})
		if apierrors.IsAlreadyExists(err) { // continued after an interruption
			if old, gerr := r.dyn.Resource(gvrPV).Get(r.ctx, pvName, metav1.GetOptions{}); gerr == nil {
				if h, _, _ := unstructured.NestedString(old.Object, "spec", "csi", "volumeHandle"); h == p.NewVolume {
					err = nil
				}
			}
		}
		if err != nil {
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
