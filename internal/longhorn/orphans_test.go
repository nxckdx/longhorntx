package longhorn

import "testing"

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
