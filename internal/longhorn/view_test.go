package longhorn

import (
	"encoding/json"
	"testing"
	"time"
)

// Shapes taken from Longhorn v1.12.1 objects (trimmed).
const (
	nodeJSON = `{"metadata":{"name":"node-3"},"status":{"diskStatus":{"nvme0":{
		"diskUUID":"uuid-3","diskPath":"/var/lib/longhorn","diskType":"filesystem",
		"storageMaximum":999714713600,"scheduledReplica":{"vol-r-c":"10737418240"}}}}}`
	replicaJSON = `{"metadata":{"name":"vol-r-c","labels":{"longhornvolume":"vol"}},
		"spec":{"volumeName":"vol","nodeID":"node-3","diskID":"uuid-3","diskPath":"/var/lib/longhorn",
		"healthyAt":"","failedAt":"","volumeSize":"10737418240"},
		"status":{"currentState":"running","storageIP":"10.0.0.3","port":10133}}`
	engineJSON = `{"metadata":{"name":"vol-e-0"},"spec":{"volumeName":"vol","active":true,"volumeSize":"10737418240"},
		"status":{"currentReplicaAddressMap":{"vol-r-c":"10.0.0.3:10133"},
		"replicaModeMap":{"vol-r-c":"WO"},
		"rebuildStatus":{"tcp://10.0.0.3:10133":{"isRebuilding":true,"progress":64,"state":"in_progress",
			"fromReplicaAddressList":["tcp://10.0.0.1:10000"]}},
		"snapshots":{"volume-head":{"name":"volume-head","size":"28655616"}}}}`
	volumeJSON = `{"metadata":{"name":"vol"},"spec":{"size":"10737418240","numberOfReplicas":3,"dataEngine":"v1"},
		"status":{"state":"attached","robustness":"degraded","actualSize":270397440,"currentNodeID":"node-2",
		"kubernetesStatus":{"namespace":"gitea","pvcName":"gitea-data"}}}`
)

func must[T any](t *testing.T, s string) *T {
	t.Helper()
	o := new(T)
	if err := json.Unmarshal([]byte(s), o); err != nil {
		t.Fatal(err)
	}
	return o
}

func TestBuildRebuildingReplica(t *testing.T) {
	snap := State{
		Volumes:  []*Volume{must[Volume](t, volumeJSON)},
		Replicas: []*Replica{must[Replica](t, replicaJSON)},
		Nodes:    []*Node{must[Node](t, nodeJSON)},
		Engines:  []*Engine{must[Engine](t, engineJSON)},
	}
	vv := Build(snap)
	if len(vv) != 1 || len(vv[0].Replicas) != 1 {
		t.Fatalf("unexpected views: %+v", vv)
	}
	v := vv[0]
	if v.PVC != "gitea/gitea-data" || v.Size != 10737418240 || v.Actual != 270397440 {
		t.Errorf("volume fields: %+v", v)
	}
	r := v.Replicas[0]
	if r.Disk.Name != "nvme0" || r.Disk.Path != "/var/lib/longhorn" {
		t.Errorf("disk not resolved: %+v", r.Disk)
	}
	if r.Health != HealthRebuilding || r.Mode != "WO" {
		t.Errorf("health=%s mode=%s", r.Health, r.Mode)
	}
	if r.Rebuild == nil || r.Rebuild.Progress != 64 {
		t.Fatalf("rebuild status not mapped via address: %+v", r.Rebuild)
	}
	if !v.Rebuilding() || v.HealthyReplicas() != 0 {
		t.Error("volume should be rebuilding with 0 healthy replicas")
	}
}

func TestRebuildFallbackAddress(t *testing.T) {
	// replica missing from currentReplicaAddressMap -> use storageIP:port
	e := must[Engine](t, engineJSON)
	e.Status.CurrentReplicaAddressMap = nil
	r := must[Replica](t, replicaJSON)
	if rs, ok := e.RebuildFor(r); !ok || rs.Progress != 64 {
		t.Fatalf("fallback failed: %+v %v", rs, ok)
	}
}

func TestWithoutEngine(t *testing.T) {
	r := BuildReplicaView(must[Replica](t, replicaJSON), DiskIndex{}, nil)
	if r.Health != HealthRebuilding || r.Rebuild != nil {
		t.Errorf("heuristic should flag rebuild without progress: %+v", r)
	}
	if r.Disk.Path != "/var/lib/longhorn" || r.Disk.Name != "" {
		t.Errorf("fallback disk: %+v", r.Disk)
	}
}

func TestQuantity(t *testing.T) {
	var q struct{ A, B, C Quantity }
	if err := json.Unmarshal([]byte(`{"A":"42","B":7,"C":""}`), &q); err != nil {
		t.Fatal(err)
	}
	if q.A != 42 || q.B != 7 || q.C != 0 {
		t.Errorf("%+v", q)
	}
}

func TestRateTracker(t *testing.T) {
	rt := NewRateTracker()
	t0 := time.Unix(0, 0)
	const size = 100 << 20 // 100 MiB
	if r := rt.Observe("r", 10, size, t0); r.Valid {
		t.Error("first sample must not yield a rate")
	}
	r := rt.Observe("r", 20, size, t0.Add(2*time.Second)) // 10 MiB in 2s
	if !r.Valid || r.BytesPerSec != 5<<20 {
		t.Fatalf("rate: %+v", r)
	}
	if r.ETA != 16*time.Second { // 80 MiB at 5 MiB/s
		t.Errorf("eta: %v", r.ETA)
	}
	// no progress -> keep estimate
	if r2 := rt.Observe("r", 20, size, t0.Add(3*time.Second)); !r2.Valid {
		t.Error("estimate lost on unchanged sample")
	}
	// progress reset (new rebuild) -> start over
	if r3 := rt.Observe("r", 5, size, t0.Add(4*time.Second)); r3.Valid {
		t.Error("expected reset")
	}
}

func TestResolveVolume(t *testing.T) {
	mk := func(name, ns, pvc string) *Volume {
		v := &Volume{}
		v.Name = name
		v.Status.KubernetesStatus = KubernetesStatus{PVName: name, Namespace: ns, PVCName: pvc}
		return v
	}
	vols := []*Volume{mk("pvc-1", "a", "data"), mk("pvc-2", "b", "data"), mk("pvc-3", "c", "cfg")}
	for ref, want := range map[string]string{"pvc-2": "pvc-2", "b/data": "pvc-2", "cfg": "pvc-3"} {
		v, err := ResolveVolume(vols, ref)
		if err != nil || v.Name != want {
			t.Errorf("%s: got %v %v", ref, v, err)
		}
	}
	if _, err := ResolveVolume(vols, "data"); err == nil {
		t.Error("ambiguous name must fail")
	}
	if v, err := ResolveVolume(vols, "c cf"); err != nil || v.Name != "pvc-3" {
		t.Errorf("fuzzy fallback: %v %v", v, err)
	}
	if _, err := ResolveVolume(vols, "dat"); err == nil {
		t.Error("ambiguous fuzzy match must fail")
	}
}

func TestVersionAtLeast(t *testing.T) {
	cases := map[string]bool{"v1.12.1": true, "v1.6.0": true, "v1.5.9": false, "v1.10.0-rc1": true, "": false}
	for v, want := range cases {
		if got := VersionAtLeast(v, MinVersion); got != want {
			t.Errorf("%q: got %v", v, got)
		}
	}
}
