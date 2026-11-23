package longhorn

import (
	"strings"
	"testing"
)

func TestOrphans(t *testing.T) {
	o := must[Orphan](t, `{"metadata":{"name":"orphan-1","uid":"u1"},"spec":{"nodeID":"carlos","orphanType":"replica",
		"parameters":{"DataName":"pvc-abc-d69a4a12","DiskPath":"/var/mnt/lh","DiskType":"filesystem"}},
		"status":{"conditions":[{"type":"DataCleanable","status":"True"}]}}`)
	v := &Volume{}
	v.Name = "pvc-abc"
	r := &Replica{}
	r.Spec.NodeID, r.Spec.DataDirectoryName = "carlos", "pvc-abc-11111111"
	ov := BuildOrphans([]*Orphan{o}, []*Volume{v}, []*Replica{r})[0]
	if ov.Volume != "pvc-abc" || !ov.VolExists || ov.InUse || !ov.Cleanable {
		t.Fatalf("%+v", ov)
	}
	a, err := DeleteOrphan(ov)
	if err != nil || !a.Destructive || a.UID != "u1" || len(a.Warnings) != 2 {
		t.Fatalf("%+v %v", a, err)
	}
	// data dir used by a current replica must be refused
	r.Spec.DataDirectoryName = "pvc-abc-d69a4a12"
	ov = BuildOrphans([]*Orphan{o}, []*Volume{v}, []*Replica{r})[0]
	if _, err := DeleteOrphan(ov); err == nil {
		t.Error("in-use data must be refused")
	}
}

func TestOrphanRisk(t *testing.T) {
	o := must[Orphan](t, `{"metadata":{"name":"orphan-1","uid":"u1"},"spec":{"nodeID":"n1","orphanType":"replica-data",
		"parameters":{"DataName":"pvc-abc-d69a4a12","DiskPath":"/var/lib/longhorn"}},
		"status":{"conditions":[{"type":"DataCleanable","status":"True"}]}}`)
	healthy := &Replica{}
	healthy.Spec.NodeID, healthy.Spec.VolumeName, healthy.Spec.DataDirectoryName = "n2", "pvc-abc", "pvc-abc-22222222"
	healthy.Spec.HealthyAt = "2026-01-01T00:00:00Z"
	vol := func(rob string) *Volume {
		v := &Volume{}
		v.Name, v.Status.Robustness = "pvc-abc", rob
		return v
	}
	for _, tc := range []struct {
		name string
		vols []*Volume
		reps []*Replica
		risk string
	}{
		{"healthy", []*Volume{vol("healthy")}, []*Replica{healthy}, ""},
		{"detached with healthy replica", []*Volume{vol("unknown")}, []*Replica{healthy}, ""},
		{"deleted", nil, nil, "volume deleted"},
		{"faulted", []*Volume{vol("faulted")}, []*Replica{healthy}, "volume faulted"},
		{"degraded", []*Volume{vol("degraded")}, []*Replica{healthy}, "volume degraded"},
		{"no healthy replica", []*Volume{vol("unknown")}, nil, "volume has no healthy replica"},
	} {
		ov := BuildOrphans([]*Orphan{o}, tc.vols, tc.reps)[0]
		if ov.Risk != tc.risk || ov.BulkDeletable() != (tc.risk == "") {
			t.Errorf("%s: risk %q bulk %v", tc.name, ov.Risk, ov.BulkDeletable())
		}
		a, err := DeleteOrphan(ov)
		if err != nil {
			t.Fatal(err)
		}
		warned := false
		for _, w := range a.Warnings {
			warned = warned || strings.HasPrefix(w, "RISKY")
		}
		if warned != (tc.risk != "") {
			t.Errorf("%s: RISKY warning %v: %v", tc.name, warned, a.Warnings)
		}
	}
	// instance orphans hold no data: never risky
	inst := must[Orphan](t, `{"metadata":{"name":"o2"},"spec":{"nodeID":"n1","orphanType":"engine-instance"}}`)
	if ov := BuildOrphans([]*Orphan{inst}, nil, nil)[0]; ov.Risk != "" {
		t.Errorf("instance orphan risky: %q", ov.Risk)
	}
}
