package longhorn

import (
	"fmt"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

var GVROrphan = gvr("orphans")

// Orphan is a Longhorn Orphan CR: replica data on a disk (or an instance)
// that no replica/engine owns anymore. Deleting the CR makes Longhorn delete
// <diskPath>/replicas/<DataName> if the DataCleanable condition is true.
type Orphan struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	Spec              struct {
		NodeID     string            `json:"nodeID"`
		OrphanType string            `json:"orphanType"` // replica (data), engine-instance, replica-instance, …
		DataEngine string            `json:"dataEngine"`
		Parameters map[string]string `json:"parameters"`
	} `json:"spec"`
	Status struct {
		OwnerID    string      `json:"ownerID"`
		Conditions []Condition `json:"conditions"`
	} `json:"status"`
}

func (o *Orphan) DeepCopyObject() runtime.Object { c := *o; return &c }

func (o *Orphan) DataName() string { return o.Spec.Parameters["DataName"] }
func (o *Orphan) DiskPath() string { return o.Spec.Parameters["DiskPath"] }
func (o *Orphan) DiskName() string { return o.Spec.Parameters["DiskName"] }

// IsReplicaData reports whether the orphan is replica data on a disk.
func (o *Orphan) IsReplicaData() bool {
	return o.Spec.OrphanType == "replica" || o.Spec.OrphanType == "replica-data"
}

// Cleanable reports whether Longhorn will delete the data with the CR.
func (o *Orphan) Cleanable() bool { return conditionTrue(o.Status.Conditions, "DataCleanable") }

// VolumeName derives the volume from the data directory name
// ("<volume>-<8 hex>"); "" if it does not look like one.
func (o *Orphan) VolumeName() string {
	d := o.DataName()
	i := strings.LastIndex(d, "-")
	if i <= 0 || len(d)-i-1 != 8 {
		return ""
	}
	return d[:i]
}

// OrphanView is an orphan joined with the volume it belonged to.
type OrphanView struct {
	Orphan    *Orphan `json:"-"`
	Name      string  `json:"name"`
	Node      string  `json:"node"`
	Type      string  `json:"type"`
	DiskPath  string  `json:"diskPath,omitempty"`
	DataName  string  `json:"dataName,omitempty"`
	Volume    string  `json:"volume,omitempty"`
	PVC       string  `json:"pvc,omitempty"`
	VolExists bool    `json:"volumeExists"`
	InUse     bool    `json:"inUse"` // data dir belongs to a current replica (should never happen)
	Cleanable bool    `json:"cleanable"`
	Created   string  `json:"created"`
}

func BuildOrphans(orphans []*Orphan, vols []*Volume, reps []*Replica) []OrphanView {
	byName := map[string]*Volume{}
	for _, v := range vols {
		byName[v.Name] = v
	}
	inUse := map[string]bool{}
	for _, r := range reps {
		inUse[r.Spec.NodeID+"/"+r.Spec.DataDirectoryName] = true
	}
	out := make([]OrphanView, 0, len(orphans))
	for _, o := range orphans {
		ov := OrphanView{
			Orphan: o, Name: o.Name, Node: o.Spec.NodeID, Type: o.Spec.OrphanType,
			DiskPath: o.DiskPath(), DataName: o.DataName(), Volume: o.VolumeName(),
			Cleanable: o.Cleanable(), Created: o.CreationTimestamp.UTC().Format("2006-01-02T15:04:05Z"),
			InUse: o.DataName() != "" && inUse[o.Spec.NodeID+"/"+o.DataName()],
		}
		if v := byName[ov.Volume]; v != nil {
			ov.VolExists, ov.PVC = true, v.PVC()
		}
		out = append(out, ov)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Node != b.Node {
			return a.Node < b.Node
		}
		return a.DataName < b.DataName
	})
	return out
}

// DeleteOrphan builds the delete action; refuses data that a replica uses.
func DeleteOrphan(ov OrphanView) (*Action, error) {
	if ov.InUse {
		return nil, fmt.Errorf("data directory %s is used by a current replica – refusing", ov.DataName)
	}
	what := ov.Type
	if ov.DataName != "" {
		what = ov.DiskPath + "/replicas/" + ov.DataName
	}
	a := &Action{
		Title: fmt.Sprintf("Delete orphan on %s: %s", ov.Node, what),
		Verb:  VerbDelete, GVR: GVROrphan, Name: ov.Name, UID: string(ov.Orphan.UID),
		Destructive: true,
	}
	switch {
	case ov.Orphan.IsReplicaData() && ov.Cleanable:
		a.Warnings = append(a.Warnings, "Longhorn deletes the data directory on the node; this cannot be undone")
	case ov.Orphan.IsReplicaData():
		a.Warnings = append(a.Warnings, "not DataCleanable: only the Orphan object is removed, the data stays on disk")
	}
	if ov.VolExists {
		a.Warnings = append(a.Warnings, "the volume still exists – this is leftover data of an earlier replica, not the current one")
	}
	a.Lines = nil
	a.summary = ov.Node + "  " + what
	return a, nil
}
