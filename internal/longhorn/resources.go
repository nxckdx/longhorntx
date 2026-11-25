package longhorn

import (
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Additional Longhorn CRDs, read on demand (not watched permanently).
// Verified against Longhorn v1.12.1.

// LabelBackupVolume is set on Backup CRs and points at the source volume.
const LabelBackupVolume = "backup-volume"

// VolumeSnapshot is the Longhorn Snapshot CR (snapshots.longhorn.io).
// Named VolumeSnapshot to avoid confusion with State.
type VolumeSnapshot struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	Spec              struct {
		Volume         string            `json:"volume"`
		CreateSnapshot bool              `json:"createSnapshot"`
		Labels         map[string]string `json:"labels"`
	} `json:"spec"`
	Status struct {
		Parent       string            `json:"parent"`
		Children     map[string]bool   `json:"children"`
		CreationTime string            `json:"creationTime"`
		Size         Quantity          `json:"size"`
		RestoreSize  Quantity          `json:"restoreSize"`
		ReadyToUse   bool              `json:"readyToUse"`
		UserCreated  bool              `json:"userCreated"`
		MarkRemoved  bool              `json:"markRemoved"`
		Labels       map[string]string `json:"labels"`
		Error        string            `json:"error"`
	} `json:"status"`
}

type Backup struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	Spec              struct {
		SnapshotName string `json:"snapshotName"`
		BackupMode   string `json:"backupMode"` // incremental | full
	} `json:"spec"`
	Status struct {
		State               string   `json:"state"` // New | Pending | InProgress | Completed | Error | Unknown
		Progress            int      `json:"progress"`
		VolumeName          string   `json:"volumeName"`
		VolumeSize          Quantity `json:"volumeSize"`
		Size                Quantity `json:"size"`
		NewlyUploadDataSize Quantity `json:"newlyUploadDataSize"`
		SnapshotName        string   `json:"snapshotName"`
		SnapshotCreatedAt   string   `json:"snapshotCreatedAt"`
		BackupCreatedAt     string   `json:"backupCreatedAt"`
		BackupTargetName    string   `json:"backupTargetName"`
		URL                 string   `json:"url"`
		Error               string   `json:"error"`
		ReplicaAddress      string   `json:"replicaAddress"`
		// Labels: Longhorn records e.g. "volume-encrypted": "enabled"
		Labels map[string]string `json:"labels"`
	} `json:"status"`
}

// Encrypted: the backup holds the data of an encrypted volume (still
// encrypted – restoring it needs the same key).
func (b *Backup) Encrypted() bool { return b.Status.Labels["volume-encrypted"] == "enabled" }

// Running reports whether the backup is still in flight.
func (b *Backup) Running() bool {
	switch b.Status.State {
	case "Completed", "Error", "Unknown":
		return false
	}
	return true
}

func (b *Backup) Volume() string {
	if b.Status.VolumeName != "" {
		return b.Status.VolumeName
	}
	return b.Labels[LabelBackupVolume]
}

type BackupTarget struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	Spec              struct {
		BackupTargetURL  string `json:"backupTargetURL"`
		CredentialSecret string `json:"credentialSecret"`
		PollInterval     string `json:"pollInterval"`
	} `json:"spec"`
	Status struct {
		Available    bool        `json:"available"`
		LastSyncedAt string      `json:"lastSyncedAt"`
		Conditions   []Condition `json:"conditions"`
	} `json:"status"`
}

type RecurringJob struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	Spec              RecurringJobSpec `json:"spec"`
	Status            struct {
		ExecutionCount int `json:"executionCount"`
	} `json:"status"`
}

type RecurringJobSpec struct {
	Task        string            `json:"task"`
	Cron        string            `json:"cron"`
	Retain      int               `json:"retain"`
	Concurrency int               `json:"concurrency"`
	Groups      []string          `json:"groups"`
	Labels      map[string]string `json:"labels"`
	Parameters  map[string]string `json:"parameters"`
}

func (o *VolumeSnapshot) DeepCopyObject() runtime.Object { c := *o; return &c }
func (o *Backup) DeepCopyObject() runtime.Object         { c := *o; return &c }
func (o *BackupTarget) DeepCopyObject() runtime.Object   { c := *o; return &c }
func (o *RecurringJob) DeepCopyObject() runtime.Object   { c := *o; return &c }

// SortSnapshots orders snapshots oldest first (chain order).
func SortSnapshots(s []*VolumeSnapshot) {
	sort.Slice(s, func(i, j int) bool {
		if s[i].Status.CreationTime != s[j].Status.CreationTime {
			return s[i].Status.CreationTime < s[j].Status.CreationTime
		}
		return s[i].Name < s[j].Name
	})
}

// SnapshotKind classifies a snapshot: "user", "backup" (created for a
// backup), "system" (e.g. rebuild/expansion) or "removed".
func SnapshotKind(s *VolumeSnapshot) string {
	switch {
	case s.Status.MarkRemoved:
		return "removed"
	case !s.Status.UserCreated:
		return "system"
	case s.Status.Labels["type"] == "bak":
		return "backup"
	case s.Status.Labels["type"] == "snap":
		return "recurring"
	}
	return "user"
}

// BackupTime returns the best timestamp for a backup.
func BackupTime(b *Backup) string {
	if b.Status.BackupCreatedAt != "" {
		return b.Status.BackupCreatedAt
	}
	if b.Status.SnapshotCreatedAt != "" {
		return b.Status.SnapshotCreatedAt
	}
	return b.CreationTimestamp.UTC().Format("2006-01-02T15:04:05Z")
}

// SortBackups orders backups newest first.
func SortBackups(b []*Backup) {
	sort.Slice(b, func(i, j int) bool { return BackupTime(b[i]) > BackupTime(b[j]) })
}
