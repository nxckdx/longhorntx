package longhorn

import (
	"context"
	"encoding/json"
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// BackupVolume is the Longhorn BackupVolume CR: one per volume and backup
// target that has backups in the backup store – also for volumes that were
// deleted since (restore candidates). Since v1.8 the name is
// "<volume>-<hash>" and spec.volumeName points at the volume; before it
// was the volume name itself.
type BackupVolume struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	Spec              struct {
		BackupTargetName string `json:"backupTargetName"`
		VolumeName       string `json:"volumeName"`
	} `json:"spec"`
	Status struct {
		Size             Quantity          `json:"size"`
		DataStored       Quantity          `json:"dataStored"`
		CreatedAt        string            `json:"createdAt"`
		LastBackupName   string            `json:"lastBackupName"`
		LastBackupAt     string            `json:"lastBackupAt"`
		LastSyncedAt     string            `json:"lastSyncedAt"`
		StorageClassName string            `json:"storageClassName"`
		Labels           map[string]string `json:"labels"`
		Messages         map[string]string `json:"messages"`
	} `json:"status"`
}

func (o *BackupVolume) DeepCopyObject() runtime.Object { c := *o; return &c }

var GVRBackupVolume = gvr("backupvolumes")

func (c *Client) BackupVolumes(ctx context.Context) ([]*BackupVolume, error) {
	return list[BackupVolume](ctx, c, GVRBackupVolume, "")
}

// Volume is the source volume name.
func (bv *BackupVolume) Volume() string {
	switch {
	case bv.Spec.VolumeName != "":
		return bv.Spec.VolumeName
	case bv.Labels[LabelBackupVolume] != "":
		return bv.Labels[LabelBackupVolume]
	}
	return bv.Name
}

// Target is the backup target name ("default" before v1.8).
func (bv *BackupVolume) Target() string {
	if bv.Spec.BackupTargetName != "" {
		return bv.Spec.BackupTargetName
	}
	if t := bv.Labels["backup-target"]; t != "" {
		return t
	}
	return "default"
}

// LastPVC is "namespace/pvc" as recorded with the last backup, so it is
// known even after the volume is gone.
func (bv *BackupVolume) LastPVC() string {
	var ks struct {
		Namespace string `json:"namespace"`
		PVCName   string `json:"pvcName"`
	}
	if json.Unmarshal([]byte(bv.Status.Labels["KubernetesStatus"]), &ks) != nil || ks.PVCName == "" {
		return ""
	}
	return ks.Namespace + "/" + ks.PVCName
}

// BackupVolumeView joins a backup volume with its source volume.
type BackupVolumeView struct {
	BV        *BackupVolume `json:"-"`
	Name      string        `json:"name"`
	Volume    string        `json:"volume"`
	PVC       string        `json:"pvc,omitempty"` // current PVC, or the one at the last backup
	Target    string        `json:"target"`
	VolExists bool          `json:"volumeExists"`
	LastAt    string        `json:"lastBackupAt,omitempty"`
	Last      string        `json:"lastBackup,omitempty"`
	Size      int64         `json:"size"`
	Stored    int64         `json:"dataStored"`
	Message   string        `json:"message,omitempty"`
}

// BuildBackupVolumes sorts by PVC (or volume); volumes that are gone first,
// they are the ones a restore would be for.
func BuildBackupVolumes(bvs []*BackupVolume, vols []*Volume) []BackupVolumeView {
	byName := map[string]*Volume{}
	for _, v := range vols {
		byName[v.Name] = v
	}
	out := make([]BackupVolumeView, 0, len(bvs))
	for _, bv := range bvs {
		v := BackupVolumeView{BV: bv, Name: bv.Name, Volume: bv.Volume(), Target: bv.Target(), PVC: bv.LastPVC(),
			LastAt: bv.Status.LastBackupAt, Last: bv.Status.LastBackupName,
			Size: int64(bv.Status.Size), Stored: int64(bv.Status.DataStored)}
		if src := byName[v.Volume]; src != nil {
			v.VolExists = true
			if p := src.PVC(); p != "" {
				v.PVC = p
			}
		}
		for _, m := range bv.Status.Messages {
			if m != "" {
				v.Message = m
				break
			}
		}
		out = append(out, v)
	}
	key := func(v BackupVolumeView) string {
		if v.PVC != "" {
			return v.PVC
		}
		return v.Volume
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.VolExists != b.VolExists {
			return !a.VolExists
		}
		if key(a) != key(b) {
			return key(a) < key(b)
		}
		return a.Target < b.Target
	})
	return out
}
