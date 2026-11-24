package longhorn

import (
	"strings"
	"testing"
	"time"
)

func TestRestoreHelpers(t *testing.T) {
	now := time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC)
	if got := RestoreName("databases/data-postgres-0", "pvc-1", now); got != "data-postgres-0-restore-1124" {
		t.Error(got)
	}
	if got := RestoreName("", "pvc-1", now); got != "pvc-1-restore-1124" {
		t.Error(got)
	}
	if got := RestoreName("ns/"+strings.Repeat("a", 70), "", now); len(got) > 63 {
		t.Error(len(got))
	}
	for in, want := range map[string]int{"3": 3, `{"v1":"2","v2":"1"}`: 2, "x": 3} {
		if got := DefaultReplicas(in); got != want {
			t.Errorf("%s: %d", in, got)
		}
	}
	old, newer, failed := &Backup{}, &Backup{}, &Backup{}
	old.Status.State, old.Status.BackupCreatedAt = "Completed", "2026-11-01T00:00:00Z"
	newer.Status.State, newer.Status.BackupCreatedAt = "Completed", "2026-11-20T00:00:00Z"
	failed.Status.State, failed.Status.BackupCreatedAt = "Error", "2026-11-23T00:00:00Z"
	if LatestBackup([]*Backup{old, failed, newer}) != newer {
		t.Error("latest")
	}
}

func TestRestoreBackup(t *testing.T) {
	b := &Backup{}
	b.Name = "backup-1"
	b.Status.State, b.Status.URL = "Completed", "s3://bk@r/?backup=backup-1&volume=pvc-a"
	b.Status.VolumeName, b.Status.VolumeSize, b.Status.Size = "pvc-a", 10<<30, 3<<30
	b.Status.BackupTargetName, b.Status.BackupCreatedAt = "default", "2026-11-23T02:00:00Z"
	bv := &BackupVolume{}
	bv.Status.Labels = map[string]string{"longhorn.io/volume-access-mode": "rwx", "KubernetesStatus": `{"namespace":"cloud","pvcName":"data"}`}
	none := func(string) bool { return false }

	s := RestoreSpec{Name: "data-restore", Replicas: 2, PVCNamespace: "cloud", PVCName: "data-restore", StorageClass: "longhorn-static"}
	acts, err := RestoreBackup(b, bv, s, none)
	if err != nil || len(acts) != 3 {
		t.Fatalf("%v %d", err, len(acts))
	}
	vs := acts[0].Object["spec"].(map[string]any)
	if vs["fromBackup"] != b.Status.URL || vs["size"] != "10737418240" || vs["accessMode"] != "rwx" || vs["numberOfReplicas"] != int64(2) {
		t.Errorf("volume %v", vs)
	}
	if !acts[1].ClusterScoped || acts[1].GVR != GVRPV {
		t.Errorf("pv %+v", acts[1])
	}
	pvc := acts[2].Object["spec"].(map[string]any)
	if acts[2].Namespace != "cloud" || pvc["volumeName"] != "data-restore" || pvc["accessModes"].([]any)[0] != "ReadWriteMany" {
		t.Errorf("pvc %v", pvc)
	}
	if !strings.Contains(acts[0].Title, "cloud/data") {
		t.Errorf("title %s", acts[0].Title)
	}

	// volume only
	s.PVCName = ""
	if acts, _ := RestoreBackup(b, bv, s, none); len(acts) != 1 {
		t.Errorf("volume only: %d", len(acts))
	}
	// refusals
	s.PVCName = "data"
	if _, err := RestoreBackup(b, bv, s, func(k string) bool { return k == "pvc" }); err == nil {
		t.Error("existing PVC accepted")
	}
	if _, err := RestoreBackup(b, bv, s, func(k string) bool { return k == "volume" }); err == nil {
		t.Error("existing volume accepted")
	}
	b.Status.State = "InProgress"
	if _, err := RestoreBackup(b, bv, s, none); err == nil {
		t.Error("unfinished backup accepted")
	}
}
