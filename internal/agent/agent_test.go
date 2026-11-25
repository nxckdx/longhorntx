package agent_test

import (
	"context"
	"testing"
	"time"

	"github.com/nxckdx/longhorntx/internal/agent"
	"github.com/nxckdx/longhorntx/internal/demo"
	"github.com/nxckdx/longhorntx/internal/longhorn"
	"github.com/nxckdx/longhorntx/internal/switchover"
)

func setup(t *testing.T) (context.Context, *longhorn.Client, []string) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c := demo.NewClient(ctx)
	a := agent.New(c, "agent-0")
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
