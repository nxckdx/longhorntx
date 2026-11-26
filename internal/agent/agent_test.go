package agent_test

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/nxckdx/longhorntx/internal/agent"
	"github.com/nxckdx/longhorntx/internal/demo"
	"github.com/nxckdx/longhorntx/internal/longhorn"
	"github.com/nxckdx/longhorntx/internal/switchover"
)

func setup(t *testing.T) (context.Context, *longhorn.Client, []string) {
	return setupMode(t, true)
}

func setupMode(t *testing.T, rw bool) (context.Context, *longhorn.Client, []string) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c := demo.NewClient(ctx)
	a := agent.New(c, "agent-0")
	a.RW = rw
	a.Poll, a.Resync, a.Logf = 50*time.Millisecond, 200*time.Millisecond, t.Logf
	go func() { _ = a.Run(ctx) }()
	vols, _ := c.Volumes(ctx)
	var pvcs []string
	for _, v := range vols {
		if v.BoundPVC() != "" && v.Status.State == "attached" {
			pvcs = append(pvcs, v.BoundPVC())
		}
		if len(pvcs) == 3 {
			break
		}
	}
	if len(pvcs) == 0 {
		t.Fatal("no attached demo volume")
	}
	return ctx, c, pvcs
}

func follow(t *testing.T, ctx context.Context, c *longhorn.Client, name string, done func(*agent.Freeze) bool) (*agent.Freeze, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	f, gone, err := agent.Follow(ctx, c, name, true, t.Logf, done)
	if err != nil {
		t.Fatalf("follow %s: %v", name, err)
	}
	return f, gone
}

// A Freeze resource is carried out by the agent and ended by deleting it.
func TestAgentFreezeUnfreeze(t *testing.T) {
	ctx, c, pvcs := setup(t)
	name, err := agent.Request(ctx, c, agent.Spec{PVCs: pvcs, Reason: "upgrade", Wait: "20s"}, "me")
	if err != nil {
		t.Fatal(err)
	}
	f, _ := follow(t, ctx, c, name, agent.Settled)
	if f.Status.Phase != agent.PhaseFrozen || f.Status.Freeze == nil || len(f.Status.Freeze.Owners) == 0 {
		t.Fatalf("status %+v", f.Status)
	}
	fs, _ := switchover.ActiveFreezes(ctx, c.Dynamic())
	if len(fs) != 1 || fs[0].Request != name || fs[0].Name != agent.PolicyName(name) {
		t.Fatalf("policies %+v", fs)
	}
	for _, v := range mustVolumes(t, ctx, c) {
		for _, p := range pvcs {
			if v.BoundPVC() == p && v.Status.State != "detached" {
				t.Errorf("%s still %s", p, v.Status.State)
			}
		}
	}
	if err := agent.End(ctx, c, name); err != nil {
		t.Fatal(err)
	}
	if _, gone := follow(t, ctx, c, name, func(*agent.Freeze) bool { return false }); !gone {
		t.Fatal("Freeze not removed")
	}
	if fs, _ := switchover.ActiveFreezes(ctx, c.Dynamic()); len(fs) != 0 {
		t.Errorf("still frozen: %+v", fs)
	}
}

// autoUnfreezeAfter: the agent ends a forgotten freeze on its own.
func TestAgentAutoUnfreeze(t *testing.T) {
	ctx, c, pvcs := setup(t)
	name, err := agent.Request(ctx, c, agent.Spec{PVCs: pvcs[:1], AutoUnfreezeAfter: "500ms", Wait: "20s"}, "me")
	if err != nil {
		t.Fatal(err)
	}
	if _, gone := follow(t, ctx, c, name, func(*agent.Freeze) bool { return false }); !gone {
		t.Fatal("not auto-unfrozen")
	}
	if fs, _ := switchover.ActiveFreezes(ctx, c.Dynamic()); len(fs) != 0 {
		t.Errorf("still frozen: %+v", fs)
	}
}

// A request for a PVC that cannot be frozen fails visibly and can be
// deleted.
func TestAgentFailed(t *testing.T) {
	ctx, c, _ := setup(t)
	name, err := agent.Request(ctx, c, agent.Spec{PVCs: []string{"Not/Valid"}}, "me")
	if err != nil {
		t.Fatal(err)
	}
	f, _ := follow(t, ctx, c, name, agent.Settled)
	if f.Status.Phase != agent.PhaseFailed || f.Status.Message == "" {
		t.Fatalf("status %+v", f.Status)
	}
	_ = agent.End(ctx, c, name)
	if _, gone := follow(t, ctx, c, name, func(*agent.Freeze) bool { return false }); !gone {
		t.Fatal("failed Freeze not removed")
	}
}

// lhtx sees the agent only while its lease is fresh.
func TestDetect(t *testing.T) {
	ctx, c, _ := setup(t)
	if agent.Detect(ctx, c) != nil {
		t.Error("agent detected without a lease")
	}
	t.Setenv("LHTX_DEMO_AGENT", "1")
	ctx2 := t.Context()
	c2 := demo.NewClient(ctx2)
	if info := agent.Detect(ctx2, c2); info == nil || info.Holder == "" {
		t.Errorf("demo agent not detected: %+v", info)
	}
}

func mustVolumes(t *testing.T, ctx context.Context, c *longhorn.Client) []*longhorn.Volume {
	vs, err := c.Volumes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return vs
}

// An agent whose lease is outside the Longhorn namespace is found by label.
func TestDetectByLabel(t *testing.T) {
	ctx := t.Context()
	c := demo.NewClient(ctx)
	lease := func(ns, name string, labels map[string]any, renew time.Time) {
		u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "coordination.k8s.io/v1", "kind": "Lease",
			"metadata": map[string]any{"name": name, "namespace": ns, "labels": labels},
			"spec":     map[string]any{"holderIdentity": name + "-pod", "renewTime": renew.UTC().Format(time.RFC3339Nano)}}}
		if _, err := c.Dynamic().Resource(gvrLease).Namespace(ns).Create(ctx, u, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	lbl := map[string]any{"app.kubernetes.io/name": "lhtx-agent"}
	lease("kube-node-lease", "node-1", nil, time.Now())
	lease("old", "lhtx-agent", lbl, time.Now().Add(-time.Hour))
	if agent.Detect(ctx, c) != nil {
		t.Error("stale or unlabelled lease taken for an agent")
	}
	lease("lhtx-system", "lhtx-agent", lbl, time.Now())
	if i := agent.Detect(ctx, c); i == nil || i.Namespace != "lhtx-system" {
		t.Errorf("agent in lhtx-system not found: %+v", i)
	}
}

var gvrLease = schema.GroupVersionResource{Group: "coordination.k8s.io", Version: "v1", Resource: "leases"}

// A Switch restores a backup and points the PVC to it, in the agent.
func TestAgentSwitchFromBackup(t *testing.T) {
	ctx, c, _ := setup(t)
	// the requester policy comes with the agent's manifests
	vap := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "admissionregistration.k8s.io/v1",
		"kind": "ValidatingAdmissionPolicy", "metadata": map[string]any{"name": agent.RequesterPolicy}}}
	if _, err := c.Dynamic().Resource(gvrVAPt).Create(ctx, vap, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	// a PVC with a completed backup
	vols, _ := c.Volumes(ctx)
	var pvc, backup string
	for _, v := range vols {
		if v.BoundPVC() == "" {
			continue
		}
		if b, _ := c.Backups(ctx, v.Name); longhorn.LatestBackup(b) != nil {
			pvc, backup = v.BoundPVC(), longhorn.LatestBackup(b).Name
			break
		}
	}
	if pvc == "" {
		t.Fatal("no demo PVC with a backup")
	}
	ns, name, _ := strings.Cut(pvc, "/")
	sw, err := agent.RequestSwitch(ctx, c, ns, agent.SwitchSpec{PVC: name, FromBackup: backup, Wait: "30s"})
	if err != nil {
		t.Fatal(err)
	}
	fctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	s, err := agent.FollowSwitch(fctx, c, ns, sw, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if s.Status.Phase != agent.SwitchDone {
		t.Fatalf("phase %s: %s", s.Status.Phase, s.Status.Message)
	}
	for _, v := range mustVolumes(t, ctx, c) {
		if v.BoundPVC() == pvc && v.Name != s.Status.TargetVolume {
			t.Errorf("PVC still on %s, want %s", v.Name, s.Status.TargetVolume)
		}
	}
	// the same backup again: the first restore holds changes now, so a new
	// volume (…-2) is restored instead of taking it
	sw2, err := agent.RequestSwitch(ctx, c, ns, agent.SwitchSpec{PVC: name, FromBackup: backup, Wait: "30s"})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond) // distinct Switch names (seconds)
	s2, err := agent.FollowSwitch(fctx, c, ns, sw2, t.Logf)
	if err != nil || s2.Status.Phase != agent.SwitchDone || !strings.HasSuffix(s2.Status.TargetVolume, "-restore-"+time.Now().Format("0102")+"-2") {
		t.Fatalf("second switch: %v %s %s → %s", err, s2.Status.Phase, s2.Status.Message, s2.Status.TargetVolume)
	}
}

var gvrVAPt = schema.GroupVersionResource{Group: "admissionregistration.k8s.io", Version: "v1", Resource: "validatingadmissionpolicies"}

// Without the requester policy requestedBy could be forged: refused.
func TestAgentSwitchNeedsPolicy(t *testing.T) {
	ctx, c, pvcs := setup(t)
	ns, name, _ := strings.Cut(pvcs[0], "/")
	sw, err := agent.RequestSwitch(ctx, c, ns, agent.SwitchSpec{PVC: name, ToVolume: "whatever"})
	if err != nil {
		t.Fatal(err)
	}
	fctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	s, err := agent.FollowSwitch(fctx, c, ns, sw, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if s.Status.Phase != agent.SwitchFailed || !strings.Contains(s.Status.Message, agent.RequesterPolicy) {
		t.Fatalf("%s: %s", s.Status.Phase, s.Status.Message)
	}
}

// A volume that happens to have the restore's name is not taken.
func TestAgentSwitchNameTaken(t *testing.T) {
	ctx, c, _ := setup(t)
	vap := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "admissionregistration.k8s.io/v1",
		"kind": "ValidatingAdmissionPolicy", "metadata": map[string]any{"name": agent.RequesterPolicy}}}
	_, _ = c.Dynamic().Resource(gvrVAPt).Create(ctx, vap, metav1.CreateOptions{})
	vols, _ := c.Volumes(ctx)
	var pvc, backup, other string
	for _, v := range vols {
		if b, _ := c.Backups(ctx, v.Name); v.BoundPVC() != "" && longhorn.LatestBackup(b) != nil && pvc == "" {
			pvc, backup = v.BoundPVC(), longhorn.LatestBackup(b).Name
		} else if other == "" {
			other = v.Name
		}
	}
	ns, name, _ := strings.Cut(pvc, "/")
	// spec.volumeName points at an unrelated, existing volume
	sw, err := agent.RequestSwitch(ctx, c, ns, agent.SwitchSpec{PVC: name, FromBackup: backup, VolumeName: other})
	if err != nil {
		t.Fatal(err)
	}
	fctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	s, err := agent.FollowSwitch(fctx, c, ns, sw, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if s.Status.Phase != agent.SwitchFailed || !strings.Contains(s.Status.Message, "give another spec.volumeName") {
		t.Fatalf("%s: %s", s.Status.Phase, s.Status.Message)
	}
}

// The agent keeps a digest per volume; lhtx's store watches the digests
// (auto picks them when an agent keeps them) and sees every volume.
func TestAgentDigests(t *testing.T) {
	t.Setenv("LHTX_DEMO_AGENT", "1")
	ctx := t.Context()
	c := demo.NewClient(ctx)
	vols := mustVolumes(t, ctx, c)
	deadline := time.Now().Add(20 * time.Second)
	for {
		l, _ := c.Dynamic().Resource(longhorn.GVRVolumeDigest).Namespace(c.Namespace).List(ctx, metav1.ListOptions{})
		if len(l.Items) == len(vols) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d digests for %d volumes", len(l.Items), len(vols))
		}
		time.Sleep(100 * time.Millisecond)
	}
	s := longhorn.NewStore(c)
	s.Start(ctx)
	if err := s.WaitSynced(ctx, 20*time.Second); err != nil {
		t.Fatal(err)
	}
	if s.RefreshInterval() != longhorn.RefreshAgent {
		t.Fatalf("refresh %v, want agent", s.RefreshInterval())
	}
	st := s.State()
	if len(st.Volumes) != len(vols) {
		t.Fatalf("%d volumes from digests, want %d", len(st.Volumes), len(vols))
	}
	byName := map[string]*longhorn.Volume{}
	for _, v := range st.Volumes {
		byName[v.Name] = v
	}
	for _, v := range vols {
		d := byName[v.Name]
		if d == nil || d.Status.State != v.Status.State || d.BoundPVC() != v.BoundPVC() || d.Spec.NumberOfReplicas != v.Spec.NumberOfReplicas {
			t.Errorf("%s: digest %+v differs", v.Name, d)
		}
	}
}

// A deleted digest comes back.
func TestAgentDigestRecreated(t *testing.T) {
	t.Setenv("LHTX_DEMO_AGENT", "1")
	ctx := t.Context()
	c := demo.NewClient(ctx)
	res := c.Dynamic().Resource(longhorn.GVRVolumeDigest).Namespace(c.Namespace)
	vols := mustVolumes(t, ctx, c)
	wait := func(n int) {
		deadline := time.Now().Add(20 * time.Second)
		for {
			l, _ := res.List(ctx, metav1.ListOptions{})
			if len(l.Items) == n {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%d digests, want %d", len(l.Items), n)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	wait(len(vols))
	if err := res.Delete(ctx, vols[0].Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	wait(len(vols))
}

// A backup run in the cluster shows up in the volume's history.
func TestAgentHistoryBackup(t *testing.T) {
	t.Setenv("LHTX_DEMO_AGENT", "1")
	ctx := t.Context()
	c := demo.NewClient(ctx)
	var vol *longhorn.Volume
	for _, v := range mustVolumes(t, ctx, c) {
		if v.Status.State == "attached" {
			vol = v
			break
		}
	}
	// the recorder starts after its watches synced
	time.Sleep(1500 * time.Millisecond)
	snaps, _ := c.Snapshots(ctx, vol.Name)
	if len(snaps) == 0 {
		t.Skip("demo volume without snapshots")
	}
	a, err := longhorn.CreateBackup(vol, snaps[0].Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Apply(ctx, a, longhorn.DryRunNone); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		ev, err := c.History(ctx, vol.Name)
		if err == nil && len(ev) > 0 && ev[0].Kind == longhorn.HistBackup {
			t.Logf("%+v", ev[0])
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no backup in the history: %+v %v", ev, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// lhtx diag asks the agent for the filtered logs (a LogQuery) and gets an
// answer; the query is removed afterwards.
func TestAgentLogQuery(t *testing.T) {
	t.Setenv("LHTX_DEMO_AGENT", "1")
	ctx := t.Context()
	c := demo.NewClient(ctx)
	time.Sleep(500 * time.Millisecond)
	if !agent.LogQueriesAvailable(ctx, c) {
		t.Fatal("log queries not available")
	}
	logs, notes, err := agent.RemoteLogs(c)(ctx, "pvc-x", []string{"node-1"}, longhorn.DiagOptions{Since: time.Hour})
	if err != nil {
		t.Fatalf("remote logs: %v (notes %v)", err, notes)
	}
	t.Logf("%d logs, notes %v", len(logs), notes)
	l, _ := c.Dynamic().Resource(agent.GVRLogQuery).Namespace(c.Namespace).List(ctx, metav1.ListOptions{})
	if len(l.Items) != 0 {
		t.Error("query not removed")
	}
}

// Without --rw the agent refuses Freezes (and changes nothing).
func TestAgentReadOnlyRefusesFreeze(t *testing.T) {
	ctx, c, pvcs := setupMode(t, false)
	name, err := agent.Request(ctx, c, agent.Spec{PVCs: pvcs, Reason: "upgrade", Wait: "20s"}, "me")
	if err != nil {
		t.Fatal(err)
	}
	f, _ := follow(t, ctx, c, name, agent.Settled)
	if f.Status.Phase != agent.PhaseFailed || !strings.Contains(f.Status.Message, "read-only") {
		t.Fatalf("phase %q message %q", f.Status.Phase, f.Status.Message)
	}
	if _, err := c.Dynamic().Resource(gvrVAPt).Get(ctx, agent.PolicyName(name), metav1.GetOptions{}); err == nil {
		t.Error("a read-only agent created a freeze policy")
	}
}
