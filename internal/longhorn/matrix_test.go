package longhorn

import "testing"

func TestMatrix(t *testing.T) {
	rv := func(node, disk, health string) ReplicaView {
		return ReplicaView{Node: node, Disk: DiskRef{UUID: disk}, Health: health}
	}
	views := []VolumeView{
		{Name: "ok", PVC: "a/ok", Replicas: []ReplicaView{rv("n1", "d1", HealthHealthy), rv("n2", "d2", HealthRebuilding)}},
		{Name: "samenode", PVC: "b/node", Replicas: []ReplicaView{rv("n1", "d1", HealthHealthy), rv("n1", "d1b", HealthHealthy)}},
		{Name: "samedisk", PVC: "c/disk", Replicas: []ReplicaView{rv("n2", "d2", HealthHealthy), rv("n2", "d2", HealthRebuilding), rv("n3", "d3", HealthHealthy)}},
	}
	m := BuildMatrix(views, []string{"n1", "n2"})
	if len(m.Nodes) != 3 || m.Nodes[2] != "n3" {
		t.Fatalf("nodes %v", m.Nodes)
	}
	if m.Rows[0].View.Name != "samedisk" || m.Rows[0].Issue != "2 replicas on one disk of n2" {
		t.Errorf("row0 %s %q", m.Rows[0].View.Name, m.Rows[0].Issue)
	}
	if m.Rows[1].View.Name != "samenode" || m.Rows[1].Issue != "2 replicas on n1" {
		t.Errorf("row1 %s %q", m.Rows[1].View.Name, m.Rows[1].Issue)
	}
	// a failed replica next to a healthy one is no issue (Longhorn removes it)
	f := BuildMatrix([]VolumeView{{Name: "f", Replicas: []ReplicaView{rv("n1", "d1", HealthHealthy), rv("n1", "d1", HealthFailed)}}}, nil)
	if f.Rows[0].Issue != "" || f.Rows[0].Cells[0].Failed != 1 {
		t.Errorf("failed: %+v", f.Rows[0])
	}
	if r := m.Rows[2]; r.Issue != "" || r.Cells[0].Healthy != 1 || r.Cells[1].Rebuilding != 1 {
		t.Errorf("row2 %+v", r)
	}
	if m.Totals[0] != 3 || m.Totals[1] != 3 || m.Totals[2] != 1 {
		t.Errorf("totals %v", m.Totals)
	}
}
