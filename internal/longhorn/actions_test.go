package longhorn

import (
	"strings"
	"testing"

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
	if !a.Destructive || len(a.Warnings) < 2 {
		t.Errorf("reducing to 1 should warn: %+v", a)
	}
	if !strings.Contains(a.Preview("longhorn-system"), `"numberOfReplicas": 1`) {
		t.Error(a.Preview("longhorn-system"))
	}
}
