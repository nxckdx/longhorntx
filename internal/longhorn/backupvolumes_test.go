package longhorn

import "testing"

func TestBackupVolumes(t *testing.T) {
	cur := must[BackupVolume](t, `{"metadata":{"name":"pvc-a-e6139943","labels":{"backup-volume":"pvc-a"}},
		"spec":{"backupTargetName":"default","volumeName":"pvc-a"},
		"status":{"size":"10737418240","dataStored":"127926272","lastBackupAt":"2026-11-23T01:04:33Z",
		"labels":{"KubernetesStatus":"{\"namespace\":\"old\",\"pvcName\":\"before-rename\"}"}}}`)
	gone := must[BackupVolume](t, `{"metadata":{"name":"pvc-b"},"status":{"size":"1048576",
		"labels":{"KubernetesStatus":"{\"namespace\":\"apps\",\"pvcName\":\"data\"}"}}}`)
	v := &Volume{}
	v.Name = "pvc-a"
	v.Status.KubernetesStatus.Namespace, v.Status.KubernetesStatus.PVCName = "db", "pg"
	out := BuildBackupVolumes([]*BackupVolume{cur, gone}, []*Volume{v})
	if len(out) != 2 {
		t.Fatal(out)
	}
	// deleted volume first, PVC from the backup's KubernetesStatus, pre-1.8 name
	if g := out[0]; g.Volume != "pvc-b" || g.VolExists || g.PVC != "apps/data" || g.Target != "default" || g.Size != 1<<20 {
		t.Errorf("gone: %+v", g)
	}
	if c := out[1]; c.Volume != "pvc-a" || !c.VolExists || c.PVC != "db/pg" || c.Stored != 127926272 {
		t.Errorf("current: %+v", c)
	}
}
