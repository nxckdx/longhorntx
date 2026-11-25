package longhorn

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
)

func testNode() *Node {
	n := &Node{}
	n.Name, n.ResourceVersion = "carlos", "42"
	n.Spec.AllowScheduling = true
	n.Spec.Disks = map[string]DiskSpec{"disk-1": {Path: "/var/mnt/longhorn-volume", AllowScheduling: true}}
	return n
}

func TestEvictionDisablesScheduling(t *testing.T) {
	a, err := SetEviction(testNode(), "disk-1", true)
	if err != nil {
		t.Fatal(err)
	}
	d := a.Patch["spec"].(map[string]any)["disks"].(map[string]any)["disk-1"].(map[string]any)
	if d["evictionRequested"] != true || d["allowScheduling"] != false {
		t.Fatalf("eviction must also disable scheduling: %v", d)
	}
	if a.Patch["metadata"].(map[string]any)["resourceVersion"] != "42" {
		t.Error("patch must carry resourceVersion")
	}
	if !a.Destructive {
		t.Error("eviction should be marked destructive")
	}
}

func TestEnableSchedulingCancelsEviction(t *testing.T) {
	n := testNode()
	n.Spec.AllowScheduling, n.Spec.EvictionRequested = false, true
	a, err := SetScheduling(n, "", true)
	if err != nil {
		t.Fatal(err)
	}
	s := a.Patch["spec"].(map[string]any)
	if s["allowScheduling"] != true || s["evictionRequested"] != false {
		t.Fatalf("got %v", s)
	}
	if _, err := SetScheduling(testNode(), "", true); err == nil {
		t.Error("no-op change must fail")
	}
	if _, err := SetScheduling(testNode(), "nope", false); err == nil {
		t.Error("unknown disk must fail")
	}
}

func rep(name string, healthy bool) *Replica {
	r := &Replica{}
	r.Name, r.UID = name, k8stypes.UID("uid-"+name)
	r.Spec.VolumeName = "vol"
	if healthy {
		r.Spec.HealthyAt = "2026-01-01T00:00:00Z"
	}
	return r
}

func TestDeleteReplicaGuard(t *testing.T) {
	a, b := rep("a", true), rep("b", false)
	if _, err := DeleteReplica(a, nil, []*Replica{a, b}); err == nil {
		t.Fatal("deleting the only healthy replica must be refused")
	}
	act, err := DeleteReplica(b, nil, []*Replica{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if act.UID != "uid-b" || act.Verb != VerbDelete {
		t.Errorf("unexpected action %+v", act)
	}
}

func TestCreateBackupLabels(t *testing.T) {
	v := &Volume{}
	v.Name = "vol"
	v.Spec.BackupTargetName = "default"
	a, err := CreateBackup(v, "snap-1")
	if err != nil {
		t.Fatal(err)
	}
	md := a.Object["metadata"].(map[string]any)
	l := md["labels"].(map[string]any)
	if l["backup-target"] != "default" || l["backup-volume"] != "vol" {
		t.Errorf("labels: %v", l)
	}
	if !strings.HasPrefix(a.Name, "backup-") || len(a.Name) != len("backup-")+16 {
		t.Errorf("name %q", a.Name)
	}
	v.Spec.BackupTargetName = ""
	if _, err := CreateBackup(v, "snap-1"); err == nil {
		t.Error("missing backup target must fail")
	}
}

func TestSetReplicaCount(t *testing.T) {
	v := &Volume{}
	v.Name = "vol"
	v.Spec.NumberOfReplicas = 2
	v.Status.State = "detached"
	if _, err := SetReplicaCount(v, 3); err == nil {
		t.Error("detached volume must be refused")
	}
	v.Status.State = "attached"
	a, err := SetReplicaCount(v, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !a.Destructive || len(a.Warnings) < 1 {
		t.Errorf("reducing to 1 should warn: %+v", a)
	}
	if !strings.Contains(a.Preview("longhorn-system"), `"numberOfReplicas": 1`) {
		t.Error(a.Preview("longhorn-system"))
	}
}

// Lowering the replica count also deletes the surplus (Longhorn keeps it):
// a failed one first, then one sharing a node, never below n healthy.
func TestReplicaCountActionsSurplus(t *testing.T) {
	v := &Volume{}
	v.Name, v.Spec.NumberOfReplicas, v.Status.State = "vol", 3, "attached"
	rep := func(name, node string, healthy, failed bool, age int) *Replica {
		r := &Replica{}
		r.Name, r.Spec.VolumeName, r.Spec.NodeID, r.Status.CurrentState = name, "vol", node, "running"
		r.CreationTimestamp = metav1.NewTime(time.Unix(int64(1000+age), 0))
		if healthy {
			r.Spec.HealthyAt = "2026-01-01T00:00:00Z"
		}
		if failed {
			r.Spec.FailedAt, r.Status.CurrentState = "2026-01-02T00:00:00Z", "stopped"
		}
		return r
	}
	reps := []*Replica{rep("a", "n1", true, false, 1), rep("b", "n2", true, false, 2), rep("c", "n3", true, false, 3), rep("d", "n3", false, true, 4)}
	acts, err := ReplicaCountActions(v, 2, reps)
	if err != nil {
		t.Fatal(err)
	}
	var del []string
	for _, a := range acts[1:] {
		del = append(del, a.Name)
	}
	// 4 live, want 2: the failed d, then c (shares n3 with d)
	if strings.Join(del, ",") != "d,c" {
		t.Errorf("deleted %v, want d,c", del)
	}
	// raising does not delete anything
	if acts, _ := ReplicaCountActions(v, 4, reps); len(acts) != 1 {
		t.Errorf("raise: %d actions", len(acts))
	}
	// never below n healthy: only 2 healthy, want 2 → only the failed one goes
	reps = []*Replica{rep("a", "n1", true, false, 1), rep("b", "n2", true, false, 2), rep("d", "n3", false, true, 4)}
	acts, _ = ReplicaCountActions(v, 2, reps)
	if len(acts) != 2 || acts[1].Name != "d" {
		t.Errorf("kept healthy: %+v", acts)
	}
}
