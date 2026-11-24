package demo

import (
	"context"
	"testing"
	"time"

	"github.com/nxckdx/longhorntx/internal/switchover"
)

// The Longhorn-upgrade flow in the demo: freeze everything, all volumes
// detach, nothing starts; unfreeze, everything is attached again.
func TestDemoFreezeAll(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := NewClient(ctx)
	vols, _ := c.Volumes(ctx)
	var pvcs []string
	for _, v := range vols {
		if v.BoundPVC() != "" {
			pvcs = append(pvcs, v.BoundPVC())
		}
	}
	p, err := switchover.PlanFreeze(ctx, c, pvcs, "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Pods) == 0 || len(p.Owners) == 0 || len(p.Attached) == 0 {
		t.Fatalf("%+v", p)
	}
	o := switchover.FreezeOptions{Poll: 100 * time.Millisecond, Wait: 20 * time.Second}
	f, err := switchover.ApplyFreeze(ctx, c, p, o)
	if err != nil {
		t.Fatal(err)
	}
	fs, _ := switchover.ActiveFreezes(ctx, c.Dynamic())
	if len(fs) != 1 || fs[0].Name != f.Name {
		t.Fatalf("active %+v", fs)
	}
	if err := switchover.Unfreeze(ctx, c, fs[0], o); err != nil {
		t.Fatal(err)
	}
}
