// Package longhorn contains slim representations of the Longhorn CRDs
// (longhorn.io/v1beta2) and the logic to join them into views.
//
// We deliberately do not import github.com/longhorn/longhorn-manager: it pulls
// in a huge dependency tree and pins a specific Longhorn version. Only the
// fields lhtx actually needs are modelled here; unknown fields are ignored, so
// these structs tolerate version differences between Longhorn releases.
//
// Field names were verified against Longhorn v1.12.1.
package longhorn

import (
	"strconv"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

const (
	Group   = "longhorn.io"
	Version = "v1beta2"

	DefaultNamespace = "longhorn-system"

	// LabelVolume is set on replicas and engines and points at the owning volume.
	LabelVolume = "longhornvolume"
	// LabelNode is set on replicas and engines.
	LabelNode = "longhornnode"
)

// Quantity is a byte count that Longhorn serialises either as a JSON string
// ("10737418240") or as a number, depending on the field.
type Quantity int64

func (q *Quantity) UnmarshalJSON(b []byte) error {
	s := string(b)
	if s == "null" || s == `""` {
		*q = 0
		return nil
	}
	if len(s) >= 2 && s[0] == '"' {
		s = s[1 : len(s)-1]
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return err
	}
	*q = Quantity(n)
	return nil
}

type Condition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// ---------------------------------------------------------------- Volume

type Volume struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	Spec              VolumeSpec   `json:"spec"`
	Status            VolumeStatus `json:"status"`
}

type VolumeSpec struct {
	Size             Quantity `json:"size"`
	NumberOfReplicas int      `json:"numberOfReplicas"`
	DataEngine       string   `json:"dataEngine"` // v1 | v2
	AccessMode       string   `json:"accessMode"` // rwo | rwx
	NodeID           string   `json:"nodeID"`
	Frontend         string   `json:"frontend"`
	DataLocality     string   `json:"dataLocality"`
	Encrypted        bool     `json:"encrypted"`
	BackupTargetName string   `json:"backupTargetName"`
	MigrationNodeID  string   `json:"migrationNodeID"`
	Image            string   `json:"image"` // desired engine image
}

type VolumeStatus struct {
	State            string           `json:"state"`      // attached | detached | attaching | ...
	Robustness       string           `json:"robustness"` // healthy | degraded | faulted | unknown
	ActualSize       Quantity         `json:"actualSize"`
	CurrentNodeID    string           `json:"currentNodeID"`
	KubernetesStatus KubernetesStatus `json:"kubernetesStatus"`
	LastBackup       string           `json:"lastBackup"`
	LastBackupAt     string           `json:"lastBackupAt"`
	LastDegradedAt   string           `json:"lastDegradedAt"`
	ShareState       string           `json:"shareState"`
	IsStandby        bool             `json:"isStandby"`
	CurrentImage     string           `json:"currentImage"` // engine image in use
	Conditions       []Condition      `json:"conditions"`
}

type KubernetesStatus struct {
	PVName    string `json:"pvName"`
	PVStatus  string `json:"pvStatus"`
	Namespace string `json:"namespace"`
	PVCName   string `json:"pvcName"`
}

// PVC returns "namespace/pvc" or "" if the volume is not bound to a PVC.
func (v *Volume) PVC() string {
	ks := v.Status.KubernetesStatus
	if ks.PVCName == "" {
		return ""
	}
	return ks.Namespace + "/" + ks.PVCName
}

// ---------------------------------------------------------------- Replica

type Replica struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	Spec              ReplicaSpec   `json:"spec"`
	Status            ReplicaStatus `json:"status"`
}

type ReplicaSpec struct {
	VolumeName        string `json:"volumeName"`
	EngineName        string `json:"engineName"`
	NodeID            string `json:"nodeID"`
	DiskID            string `json:"diskID"` // disk UUID, see Node.Status.DiskStatus[*].DiskUUID
	DiskPath          string `json:"diskPath"`
	DataDirectoryName string `json:"dataDirectoryName"`
	DataEngine        string `json:"dataEngine"`
	DesireState       string `json:"desireState"`
	Active            bool   `json:"active"`
	HealthyAt         string `json:"healthyAt"`
	FailedAt          string `json:"failedAt"`
	LastFailedAt      string `json:"lastFailedAt"`
	EvictionRequested bool   `json:"evictionRequested"`
	RebuildRetryCount int    `json:"rebuildRetryCount"`
}

type ReplicaStatus struct {
	CurrentState string `json:"currentState"` // running | stopped | error | starting | stopping
	IP           string `json:"ip"`
	StorageIP    string `json:"storageIP"`
	Port         int    `json:"port"`
}

// LikelyRebuilding is a cheap heuristic derived from the replica CR alone:
// a running replica that has never become healthy is being rebuilt. It lets
// us decide when it is worth watching the (chatty) engine CR for progress.
func (r *Replica) LikelyRebuilding() bool {
	return r.Status.CurrentState == "running" && r.Spec.HealthyAt == "" && r.Spec.FailedAt == ""
}

// ---------------------------------------------------------------- Engine

type Engine struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	Spec              EngineSpec   `json:"spec"`
	Status            EngineStatus `json:"status"`
}

type EngineSpec struct {
	VolumeName string   `json:"volumeName"`
	VolumeSize Quantity `json:"volumeSize"`
	NodeID     string   `json:"nodeID"`
	DataEngine string   `json:"dataEngine"`
	Active     bool     `json:"active"`
}

// The progress maps below are keyed by replica address ("tcp://ip:port"),
// NOT by replica name. Use CurrentReplicaAddressMap (name -> "ip:port") to
// translate.
type EngineStatus struct {
	CurrentState             string                         `json:"currentState"`
	CurrentReplicaAddressMap map[string]string              `json:"currentReplicaAddressMap"`
	ReplicaModeMap           map[string]string              `json:"replicaModeMap"`           // name -> RW | WO | ERR
	ReplicaTransitionTimeMap map[string]string              `json:"replicaTransitionTimeMap"` // name -> RFC3339 of last mode change
	RebuildStatus            map[string]RebuildStatus       `json:"rebuildStatus"`
	PurgeStatus              map[string]PurgeStatus         `json:"purgeStatus"`
	RestoreStatus            map[string]RestoreStatus       `json:"restoreStatus"`
	CloneStatus              map[string]CloneStatus         `json:"cloneStatus"`
	Snapshots                map[string]EngineSnapshotEntry `json:"snapshots"`
}

type RebuildStatus struct {
	Error                  string   `json:"error"`
	IsRebuilding           bool     `json:"isRebuilding"`
	Progress               int      `json:"progress"`
	State                  string   `json:"state"`
	FromReplicaAddressList []string `json:"fromReplicaAddressList"`
	AppliedRebuildingMBps  int64    `json:"appliedRebuildingMBps"`
}

type PurgeStatus struct {
	Error     string `json:"error"`
	IsPurging bool   `json:"isPurging"`
	Progress  int    `json:"progress"`
	State     string `json:"state"`
}

type RestoreStatus struct {
	IsRestoring            bool   `json:"isRestoring"`
	Progress               int    `json:"progress"`
	State                  string `json:"state"`
	Error                  string `json:"error"`
	CurrentRestoringBackup string `json:"currentRestoringBackup"`
}

type CloneStatus struct {
	IsCloning    bool   `json:"isCloning"`
	Progress     int    `json:"progress"`
	State        string `json:"state"`
	Error        string `json:"error"`
	SnapshotName string `json:"snapshotName"`
}

type EngineSnapshotEntry struct {
	Name        string   `json:"name"`
	Parent      string   `json:"parent"`
	Created     string   `json:"created"`
	Size        Quantity `json:"size"`
	UserCreated bool     `json:"usercreated"`
	Removed     bool     `json:"removed"`
}

// ---------------------------------------------------------------- Node

type Node struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	Spec              NodeSpec   `json:"spec"`
	Status            NodeStatus `json:"status"`
}

type NodeSpec struct {
	AllowScheduling   bool                `json:"allowScheduling"`
	EvictionRequested bool                `json:"evictionRequested"`
	Disks             map[string]DiskSpec `json:"disks"`
	Tags              []string            `json:"tags"`
}

type DiskSpec struct {
	Path              string   `json:"path"`
	DiskType          string   `json:"diskType"` // filesystem | block (v2 data engine)
	AllowScheduling   bool     `json:"allowScheduling"`
	EvictionRequested bool     `json:"evictionRequested"`
	StorageReserved   Quantity `json:"storageReserved"`
	Tags              []string `json:"tags"`
}

type NodeStatus struct {
	Region     string                `json:"region"`
	Zone       string                `json:"zone"`
	Conditions []Condition           `json:"conditions"`
	DiskStatus map[string]DiskStatus `json:"diskStatus"` // key = disk name
}

type DiskStatus struct {
	DiskName         string              `json:"diskName"`
	DiskUUID         string              `json:"diskUUID"`
	DiskPath         string              `json:"diskPath"`
	DiskType         string              `json:"diskType"`
	DiskDriver       string              `json:"diskDriver"`
	FilesystemType   string              `json:"filesystemType"`
	StorageAvailable Quantity            `json:"storageAvailable"`
	StorageMaximum   Quantity            `json:"storageMaximum"`
	StorageScheduled Quantity            `json:"storageScheduled"`
	ScheduledReplica map[string]Quantity `json:"scheduledReplica"`
	Conditions       []Condition         `json:"conditions"`
	// HealthData holds SMART data (Longhorn ≥ 1.10), keyed by device.
	HealthData map[string]DiskHealth `json:"healthData"`
}

type DiskHealth struct {
	DiskName     string `json:"diskName"`
	ModelName    string `json:"modelName"`
	HealthStatus string `json:"healthStatus"`
	Temperature  int    `json:"temperature"`
	Attributes   []struct {
		Name     string `json:"name"`
		RawValue int64  `json:"rawValue"`
		Value    int    `json:"value"` // normalized (0-100/200/253, higher = better)
	} `json:"attributes"`
}

func (n *Node) Ready() bool { return conditionTrue(n.Status.Conditions, "Ready") }

func conditionTrue(cs []Condition, t string) bool {
	for _, c := range cs {
		if c.Type == t {
			return c.Status == "True"
		}
	}
	return false
}

// ---------------------------------------------------------------- Setting

type Setting struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	Value             string `json:"value"`
}

// ---------------------------------------------------------------- runtime.Object
//
// The informer store only needs runtime.Object + metav1.Object. Objects in the
// store are treated as immutable, so a shallow copy is sufficient.

func (o *Volume) DeepCopyObject() runtime.Object  { c := *o; return &c }
func (o *Replica) DeepCopyObject() runtime.Object { c := *o; return &c }
func (o *Engine) DeepCopyObject() runtime.Object  { c := *o; return &c }
func (o *Node) DeepCopyObject() runtime.Object    { c := *o; return &c }
func (o *Setting) DeepCopyObject() runtime.Object { c := *o; return &c }
