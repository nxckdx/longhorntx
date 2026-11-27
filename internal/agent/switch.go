package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/nxckdx/longhorntx/internal/longhorn"
	"github.com/nxckdx/longhorntx/internal/switchover"
)

// Switch (lhtx.io/v1alpha1, namespaced – in the PVC's namespace): the agent
// points the PVC to another Longhorn volume, restoring a backup into it
// first if asked, like lhtx switch / restore --switch. It runs in the
// cluster, so a long restore does not depend on a laptop, and it continues
// after its own restart.
//
// The agent has far more rights than the person asking. So:
//   - an admission policy (installed with the agent) makes sure
//     spec.requestedBy/requestedGroups are the real requester and the spec
//     never changes afterwards;
//   - the agent asks the API server (SubjectAccessReview) whether that
//     requester may delete and create the PVC and delete its pods;
//   - unless the requester may create Longhorn volumes (an admin), only the
//     PVC's own data may be used: a volume that belonged to this PVC, or a
//     restore/clone of one, or a backup of one.
var GVRSwitch = schema.GroupVersionResource{Group: "lhtx.io", Version: "v1alpha1", Resource: "switches"}

var (
	gvrSAR = schema.GroupVersionResource{Group: "authorization.k8s.io", Version: "v1", Resource: "subjectaccessreviews"}
	gvrVAP = schema.GroupVersionResource{Group: "admissionregistration.k8s.io", Version: "v1", Resource: "validatingadmissionpolicies"}
)

// annSwitch marks a volume the agent restored for a Switch (namespace/name).
const annSwitch = "lhtx.io/switch"

// RequesterPolicy is the admission policy that vouches for requestedBy.
const RequesterPolicy = "lhtx-switch-requester"

const (
	SwitchPending   = "Pending"
	SwitchRestoring = "Restoring"
	SwitchRunning   = "Switching"
	SwitchDone      = "Done"
	SwitchFailed    = "Failed"     // nothing changed (refused, blocked, rolled back)
	SwitchStuck     = "Stuck"      // past the point of no return; the PVC stays frozen
	SwitchKeyStop   = "KeyChanged" // the key Secret was replaced; PVC frozen, old key kept
)

// SwitchSpec of a Switch.
type SwitchSpec struct {
	PVC        string `json:"pvc"`                  // in the Switch's namespace
	ToVolume   string `json:"toVolume,omitempty"`   // an existing Longhorn volume, or
	FromBackup string `json:"fromBackup,omitempty"` // a Backup to restore into a new volume first
	Replicas   int    `json:"replicas,omitempty"`   // of the restored volume (default: like the PVC's volume)
	VolumeName string `json:"volumeName,omitempty"` // of the restored volume (default <pvc>-restore-<MMDD>)
	Wait       string `json:"wait,omitempty"`       // waits for pods/PVCs (default 10m)
	// ShrinkTo: instead of switching to an existing or restored volume,
	// shrink the PVC to this size (lhtx shrink): new smaller volume, copy,
	// switch. The copy pod uses the agent's own image.
	ShrinkTo         string   `json:"shrinkTo,omitempty"`
	ImagePullSecrets []string `json:"imagePullSecrets,omitempty"` // in the PVC's namespace, for the copy pod

	RequestedBy     string   `json:"requestedBy"`
	RequestedGroups []string `json:"requestedGroups,omitempty"`
}

// SwitchStatus of a Switch, written by the agent.
type SwitchStatus struct {
	Phase        string           `json:"phase,omitempty"`
	Message      string           `json:"message,omitempty"`
	TargetVolume string           `json:"targetVolume,omitempty"`
	Agent        string           `json:"agent,omitempty"`
	Plan         *switchover.Plan `json:"plan,omitempty"`
	Log          []string         `json:"log,omitempty"`
	LogCount     int              `json:"logCount,omitempty"`
}

// Switch as lhtx reads it.
type Switch struct {
	Namespace, Name string
	Spec            SwitchSpec
	Status          SwitchStatus
	Created         time.Time
}

// SwitchFinished: nothing more will happen.
func SwitchFinished(s *Switch) bool {
	switch s.Status.Phase {
	case SwitchDone, SwitchFailed, SwitchStuck, SwitchKeyStop:
		return true
	}
	return false
}

func switchFromU(u *unstructured.Unstructured) (*Switch, error) {
	var raw struct {
		Spec   SwitchSpec   `json:"spec"`
		Status SwitchStatus `json:"status"`
	}
	b, err := json.Marshal(u.Object)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, err
	}
	return &Switch{Namespace: u.GetNamespace(), Name: u.GetName(), Spec: raw.Spec, Status: raw.Status,
		Created: u.GetCreationTimestamp().Time}, nil
}

// switchFreezeName: the switch's freeze policy, stable across restarts.
func switchFreezeName(s *Switch) string {
	n := "lhtx-freeze-sw-" + s.Namespace + "-" + s.Name
	if len(n) > 253 {
		n = n[:253]
	}
	return n
}

// ---------------------------------------------------------------- agent side

// reconcileSwitch starts work for a Switch that is not finished.
func (a *Agent) reconcileSwitch(ctx context.Context, key string) {
	ns, name, _ := strings.Cut(key, "/")
	u, err := a.c.Dynamic().Resource(GVRSwitch).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return
	}
	if err != nil {
		a.Logf("switch %s: %v", key, err)
		return
	}
	s, err := switchFromU(u)
	if err != nil || SwitchFinished(s) {
		return
	}
	if !a.RW {
		lg := a.switchStatus(s)
		lg.add("refused: %s", ErrReadOnly)
		lg.flush(ctx, map[string]any{"phase": SwitchFailed, "message": ErrReadOnly.Error()})
		a.op("switch", "refused")
		return
	}
	tkey := "sw:" + key
	a.mu.Lock()
	_, running := a.tasks[tkey]
	a.mu.Unlock()
	if running {
		return
	}
	tctx, cancel := context.WithCancel(ctx)
	t := &task{cancel: cancel, done: make(chan struct{})}
	a.mu.Lock()
	a.tasks[tkey] = t
	a.mu.Unlock()
	go func() {
		defer close(t.done)
		defer cancel()
		a.runSwitch(tctx, s)
		a.mu.Lock()
		if a.tasks[tkey] == t {
			delete(a.tasks, tkey)
		}
		a.mu.Unlock()
	}()
}

// switchLog is statusLog for a Switch (namespaced).
type switchLog struct {
	*statusLog
	ns string
}

func (a *Agent) switchStatus(s *Switch) *switchLog {
	return &switchLog{statusLog: &statusLog{a: a, name: s.Name, lines: slices.Clone(s.Status.Log), count: s.Status.LogCount}, ns: s.Namespace}
}

func (l *switchLog) flush(ctx context.Context, fields map[string]any) {
	l.mu.Lock()
	st := map[string]any{"log": slices.Clone(l.lines), "logCount": l.count}
	l.dirty = false
	l.mu.Unlock()
	for k, v := range fields {
		st[k] = v
	}
	patch, _ := json.Marshal(map[string]any{"status": st})
	if _, err := l.a.c.Dynamic().Resource(GVRSwitch).Namespace(l.ns).Patch(context.WithoutCancel(ctx), l.name, types.MergePatchType, patch,
		metav1.PatchOptions{FieldManager: "lhtx-agent"}, "status"); err != nil && !apierrors.IsNotFound(err) {
		l.a.Logf("switch %s/%s: status: %v", l.ns, l.name, err)
	}
}

func (l *switchLog) autoFlush(ctx context.Context) func() {
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				l.mu.Lock()
				dirty := l.dirty
				l.mu.Unlock()
				if dirty {
					l.flush(ctx, nil)
				}
			}
		}
	}()
	return func() { close(done); <-finished }
}

func (a *Agent) runSwitch(ctx context.Context, s *Switch) {
	lg := a.switchStatus(s)
	fail := func(phase string, err error) {
		a.op("switch", "failed")
		lg.add("✘ %v", err)
		lg.flush(ctx, map[string]any{"phase": phase, "message": err.Error()})
	}
	resumed := s.Status.Phase != "" && s.Status.Phase != SwitchPending
	if resumed {
		lg.add("agent %s resumes the switch (%s)", a.identity, s.Status.Phase)
	} else {
		lg.add("agent %s takes the switch of %s/%s", a.identity, s.Namespace, s.Spec.PVC)
	}
	lg.flush(ctx, map[string]any{"agent": a.identity})

	admin, err := a.authorize(ctx, s)
	if err != nil {
		fail(SwitchFailed, err)
		return
	}
	if s.Spec.ShrinkTo != "" {
		a.runShrink(ctx, lg, s)
		return
	}
	o := switchover.Options{Log: lg.add, Wait: durationOr(s.Spec.Wait, 10*time.Minute), Poll: a.Poll,
		FreezeName: switchFreezeName(s), Request: s.Namespace + "/" + s.Name, Handover: true}
	stop := lg.autoFlush(ctx)
	defer stop()

	// picked up after a restart in the middle of the switch
	if s.Status.Phase == SwitchRunning && s.Status.Plan != nil {
		err := switchover.Continue(ctx, a.c, s.Status.Plan, o)
		switch {
		case err == nil:
			a.op("switch", "done")
			lg.flush(ctx, map[string]any{"phase": SwitchDone, "message": ""})
			return
		case errors.Is(err, switchover.ErrStartOver):
			lg.add("the old PVC is still there – starting over")
		default:
			a.finishSwitch(ctx, lg, s, err)
			return
		}
	}

	target := s.Status.TargetVolume
	if target == "" {
		if target, err = a.prepareTarget(ctx, lg, s, admin); err != nil {
			fail(SwitchFailed, err)
			return
		}
		lg.flush(ctx, map[string]any{"targetVolume": target, "phase": SwitchRestoring})
	}
	p, err := switchover.Build(ctx, a.c, s.Namespace, s.Spec.PVC, target)
	if err != nil {
		fail(SwitchFailed, err)
		return
	}
	if len(p.Blockers) > 0 {
		fail(SwitchFailed, fmt.Errorf("blocked: %s", strings.Join(p.Blockers, "; ")))
		return
	}
	for _, w := range p.Warnings {
		lg.add("⚠ %s", w)
	}
	lg.flush(ctx, map[string]any{"phase": SwitchRunning, "plan": p})
	err = switchover.Run(ctx, a.c, p, o)
	if ctx.Err() != nil {
		return // agent stopping: the next one continues
	}
	a.finishSwitch(ctx, lg, s, err)
}

// finishSwitch sets the final phase from the result and the cluster state.
func (a *Agent) finishSwitch(ctx context.Context, lg *switchLog, s *Switch, err error) {
	if err == nil {
		a.op("switch", "done")
		lg.flush(ctx, map[string]any{"phase": SwitchDone, "message": ""})
		return
	}
	lg.add("✘ %v", err)
	phase := SwitchStuck
	defer func() {
		a.op("switch", map[string]string{SwitchStuck: "stuck", SwitchKeyStop: "key-changed", SwitchFailed: "failed"}[phase])
	}()
	switch {
	case strings.Contains(err.Error(), "ANOTHER key"):
		phase = SwitchKeyStop
	case errors.Is(err, switchover.ErrBlocked) || strings.Contains(err.Error(), "rolled back") || strings.Contains(err.Error(), "nothing changed") ||
		strings.Contains(err.Error(), "nothing was stopped") || strings.Contains(err.Error(), "would be rejected"):
		phase = SwitchFailed
	}
	lg.flush(ctx, map[string]any{"phase": phase, "message": err.Error()})
}

// prepareTarget checks the target volume, or restores the backup into a
// new one, and returns its name.
func (a *Agent) prepareTarget(ctx context.Context, lg *switchLog, s *Switch, admin bool) (string, error) {
	pvc := s.Namespace + "/" + s.Spec.PVC
	switch {
	case s.Spec.ToVolume != "" && s.Spec.FromBackup != "":
		return "", fmt.Errorf("give toVolume or fromBackup, not both")
	case s.Spec.ToVolume != "":
		v, err := a.c.Volume(ctx, s.Spec.ToVolume)
		if err != nil {
			return "", fmt.Errorf("target volume: %w", err)
		}
		if !admin && !a.ownsVolume(ctx, v, pvc, 2) {
			return "", fmt.Errorf("volume %s is not data of %s (only an admin may switch to other volumes)", v.Name, pvc)
		}
		return v.Name, nil
	case s.Spec.FromBackup != "":
		b, err := a.c.Backup(ctx, s.Spec.FromBackup)
		if err != nil {
			return "", fmt.Errorf("backup: %w", err)
		}
		if !admin && !a.backupOf(ctx, b, pvc) {
			return "", fmt.Errorf("backup %s is not a backup of %s (only an admin may restore other data)", b.Name, pvc)
		}
		plan, err := a.c.PlanRestore(ctx, b, time.Now())
		if err != nil {
			return "", err
		}
		mine := s.Namespace + "/" + s.Name
		base := longhorn.RestoreName(pvc, b.Volume(), time.Now())
		if s.Spec.VolumeName != "" {
			base = s.Spec.VolumeName
		}
		// a free name; a volume this Switch created already (the agent
		// restarted right after) is taken – no other: it may hold changes
		name := ""
		for i := 1; i <= 9 && name == ""; i++ {
			cand := base
			if i > 1 {
				suf := fmt.Sprintf("-%d", i)
				cand = longhorn.RestoreNameMax(pvc, b.Volume(), time.Now(), longhorn.MaxVolumeName-len(suf)) + suf
			}
			v, err := a.c.Volume(ctx, cand)
			switch {
			case apierrors.IsNotFound(err):
				name = cand
			case err != nil:
				return "", err
			case v.Annotations[annSwitch] == mine:
				lg.add("✔ volume %s was created by this switch already – using it", v.Name)
				return v.Name, nil
			case s.Spec.VolumeName != "":
				return "", fmt.Errorf("volume %s exists – give another spec.volumeName", cand)
			}
		}
		if name == "" {
			return "", fmt.Errorf("no free volume name for the restore (%s-2 … -9 exist)", base)
		}
		plan.Spec.Name = name
		acts, err := a.c.BuildRestore(ctx, plan)
		if err != nil {
			return "", err
		}
		md := acts[0].Object["metadata"].(map[string]any)
		md["annotations"] = map[string]any{annSwitch: mine}
		if err := a.c.Apply(ctx, acts[0], longhorn.DryRunNone); err != nil {
			return "", fmt.Errorf("create the restore volume: %w", err)
		}
		lg.add("✔ restoring %s into new volume %s", b.Name, plan.Spec.Name)
		return plan.Spec.Name, nil
	}
	return "", fmt.Errorf("give toVolume or fromBackup")
}

// ownsVolume: the volume belonged to the PVC, or is a restore/clone of a
// volume that did (depth levels deep).
func (a *Agent) ownsVolume(ctx context.Context, v *longhorn.Volume, pvc string, depth int) bool {
	if v.PVC() == pvc {
		return true
	}
	if depth == 0 {
		return false
	}
	if ds := v.Spec.DataSource; ds != "" {
		src := strings.TrimPrefix(strings.TrimPrefix(ds, "vol://"), "snap://")
		src, _, _ = strings.Cut(src, "/")
		if sv, err := a.c.Volume(ctx, src); err == nil && a.ownsVolume(ctx, sv, pvc, depth-1) {
			return true
		}
	}
	if fb := v.Spec.FromBackup; fb != "" {
		// the URL names the backup: …?backup=backup-…&volume=…
		if u, err := url.Parse(fb); err == nil {
			if b, err := a.c.Backup(ctx, u.Query().Get("backup")); err == nil {
				return a.backupOf(ctx, b, pvc)
			}
		}
	}
	return false
}

// backupOf: the backup is of a volume that belongs/belonged to the PVC.
func (a *Agent) backupOf(ctx context.Context, b *longhorn.Backup, pvc string) bool {
	if v, err := a.c.Volume(ctx, b.Volume()); err == nil {
		return v.PVC() == pvc
	}
	if bvs, err := a.c.BackupVolumes(ctx); err == nil {
		for _, bv := range bvs {
			if bv.Volume() == b.Volume() && bv.LastPVC() == pvc {
				return true
			}
		}
	}
	return false
}

// authorize checks that the requester is vouched for by the admission
// policy and may do what the switch does. admin: may create Longhorn
// volumes – then any volume or backup may be used.
func (a *Agent) authorize(ctx context.Context, s *Switch) (admin bool, err error) {
	if s.Spec.RequestedBy == "" {
		return false, fmt.Errorf("spec.requestedBy is empty")
	}
	if _, err := a.c.Dynamic().Resource(gvrVAP).Get(ctx, RequesterPolicy, metav1.GetOptions{}); err != nil {
		return false, fmt.Errorf("the admission policy %s is missing (it vouches for requestedBy) – install the agent's manifests completely: %v", RequesterPolicy, err)
	}
	checks := []struct{ verb, group, resource, ns string }{
		{"delete", "", "persistentvolumeclaims", s.Namespace},
		{"create", "", "persistentvolumeclaims", s.Namespace},
		{"delete", "", "pods", s.Namespace},
	}
	if s.Spec.ShrinkTo != "" { // the copy pod runs in the namespace
		checks = append(checks, struct{ verb, group, resource, ns string }{"create", "", "pods", s.Namespace})
	}
	for _, c := range checks {
		ok, err := a.can(ctx, s, c.verb, c.group, c.resource, c.ns)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, fmt.Errorf("%s may not %s %s in %s – the switch does that", s.Spec.RequestedBy, c.verb, c.resource, c.ns)
		}
	}
	return a.can(ctx, s, "create", longhorn.Group, "volumes", a.c.Namespace)
}

func (a *Agent) can(ctx context.Context, s *Switch, verb, group, resource, ns string) (bool, error) {
	return a.sar(ctx, s.Spec.RequestedBy, s.Spec.RequestedGroups, verb, group, resource, "", ns)
}

// sar asks the API server whether user (with groups) may do verb on the
// resource (and subresource) in ns.
func (a *Agent) sar(ctx context.Context, user string, groups []string, verb, group, resource, subresource, ns string) (bool, error) {
	g := make([]any, len(groups))
	for i, x := range groups {
		g[i] = x
	}
	ra := map[string]any{"verb": verb, "group": group, "resource": resource, "namespace": ns}
	if subresource != "" {
		ra["subresource"] = subresource
	}
	sar := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "authorization.k8s.io/v1", "kind": "SubjectAccessReview",
		"spec": map[string]any{"user": user, "groups": g, "resourceAttributes": ra},
	}}
	out, err := a.c.Dynamic().Resource(gvrSAR).Create(ctx, sar, metav1.CreateOptions{})
	if err != nil {
		return false, fmt.Errorf("SubjectAccessReview: %w", err)
	}
	ok, _, _ := unstructured.NestedBool(out.Object, "status", "allowed")
	return ok, nil
}

// ---------------------------------------------------------------- client side

// RequestSwitch creates a Switch in the PVC's namespace with the caller's
// identity (the admission policy checks it).
func RequestSwitch(ctx context.Context, c *longhorn.Client, ns string, spec SwitchSpec) (string, error) {
	who, err := switchover.WhoAmI(ctx, c.Dynamic())
	if err != nil {
		return "", err
	}
	spec.RequestedBy, spec.RequestedGroups = who.Username, who.Groups
	name := spec.PVC + "-" + time.Now().UTC().Format("20060102-150405")
	if len(name) > 63 {
		name = name[len(name)-63:]
		name = strings.TrimLeft(name, "-.")
	}
	b, _ := json.Marshal(spec)
	var sp map[string]any
	_ = json.Unmarshal(b, &sp)
	u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "lhtx.io/v1alpha1", "kind": "Switch",
		"metadata": map[string]any{"name": name, "namespace": ns}, "spec": sp}}
	if _, err := c.Dynamic().Resource(GVRSwitch).Namespace(ns).Create(ctx, u, metav1.CreateOptions{FieldManager: "lhtx"}); err != nil {
		return "", fmt.Errorf("create Switch %s/%s: %w", ns, name, err)
	}
	return name, nil
}

// SwitchesAvailable: the agent can do switches (its CRD is installed and the
// caller may create Switches in ns – checked with a dry run).
func SwitchesAvailable(ctx context.Context, c *longhorn.Client, ns string) bool {
	u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "lhtx.io/v1alpha1", "kind": "Switch",
		"metadata": map[string]any{"generateName": "probe-", "namespace": ns},
		"spec":     map[string]any{"pvc": "probe", "toVolume": "probe", "requestedBy": "probe"}}}
	_, err := c.Dynamic().Resource(GVRSwitch).Namespace(ns).Create(ctx, u, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
	// the requester policy rejects the probe's requestedBy: that also means it works
	return err == nil || (apierrors.IsInvalid(err) || apierrors.IsForbidden(err)) && strings.Contains(err.Error(), RequesterPolicy)
}

// FollowSwitch prints the agent's log until the Switch is finished.
func FollowSwitch(ctx context.Context, c *longhorn.Client, ns, name string, logf func(string, ...any)) (*Switch, error) {
	seen := -1
	res := c.Dynamic().Resource(GVRSwitch).Namespace(ns)
	for {
		u, err := res.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		s, err := switchFromU(u)
		if err != nil {
			return nil, err
		}
		first := s.Status.LogCount - len(s.Status.Log)
		if seen < 0 {
			seen = first
		}
		for i, l := range s.Status.Log {
			if first+i >= seen {
				logf("%s", strings.TrimSpace(l))
			}
		}
		seen = max(seen, s.Status.LogCount)
		if SwitchFinished(s) {
			return s, nil
		}
		select {
		case <-ctx.Done():
			return s, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}
