package longhorn

import "sort"

// NodeView is a node with its disks and the replicas on each disk.
type NodeView struct {
	Node        *Node      `json:"-"`
	Name        string     `json:"name"`
	Ready       bool       `json:"ready"`
	Schedulable bool       `json:"schedulable"` // node condition and spec.allowScheduling
	Eviction    bool       `json:"evictionRequested"`
	Zone        string     `json:"zone,omitempty"`
	Disks       []DiskView `json:"disks"`
}

type DiskView struct {
	DiskRef
	Maximum         int64           `json:"storageMaximum"`
	Available       int64           `json:"storageAvailable"`
	Scheduled       int64           `json:"storageScheduled"`
	Reserved        int64           `json:"storageReserved"`
	AllowScheduling bool            `json:"allowScheduling"`
	Eviction        bool            `json:"evictionRequested"`
	Ready           bool            `json:"ready"`
	Schedulable     bool            `json:"schedulable"`
	FSType          string          `json:"filesystemType,omitempty"`
	SMART           *SMARTSummary   `json:"smart,omitempty"`
	Replicas        []ReplicaOnDisk `json:"replicas"`
	Conditions      []Condition     `json:"-"`
}

// SMARTSummary condenses node.status.diskStatus[*].healthData (Longhorn ≥ 1.10).
type SMARTSummary struct {
	Status        string `json:"status"` // PASSED | FAILED | ...
	Model         string `json:"model,omitempty"`
	Temperature   int    `json:"temperature,omitempty"`
	LifetimeLeft  int    `json:"lifetimeLeftPercent,omitempty"` // -1 if unknown
	Device        string `json:"device,omitempty"`
	PowerOnHours  int64  `json:"powerOnHours,omitempty"`
	Reallocated   int64  `json:"reallocated,omitempty"`
	PendingSector int64  `json:"pendingSectors,omitempty"`
}

type ReplicaOnDisk struct {
	Replica string `json:"replica"`
	Volume  string `json:"volume"`
	PVC     string `json:"pvc,omitempty"`
	Size    int64  `json:"size"`
	Health  string `json:"health"`
}

// BuildNodes joins nodes, their disks and the replicas scheduled on them.
func BuildNodes(s State) []NodeView {
	vols := map[string]*Volume{}
	for _, v := range s.Volumes {
		vols[v.Name] = v
	}
	byDisk := map[string][]*Replica{}
	for _, r := range s.Replicas {
		byDisk[r.Spec.DiskID] = append(byDisk[r.Spec.DiskID], r)
	}
	engines := ActiveEngines(s.Engines)

	var out []NodeView
	for _, n := range s.Nodes {
		nv := NodeView{
			Node:        n,
			Name:        n.Name,
			Ready:       n.Ready(),
			Schedulable: n.Spec.AllowScheduling && conditionTrue(n.Status.Conditions, "Schedulable"),
			Eviction:    n.Spec.EvictionRequested,
			Zone:        n.Status.Zone,
		}
		for name, ds := range n.Status.DiskStatus {
			spec := n.Spec.Disks[name]
			dv := DiskView{
				DiskRef:         DiskRef{Node: n.Name, Name: name, UUID: ds.DiskUUID, Path: ds.DiskPath, Type: ds.DiskType},
				Maximum:         int64(ds.StorageMaximum),
				Available:       int64(ds.StorageAvailable),
				Scheduled:       int64(ds.StorageScheduled),
				Reserved:        int64(spec.StorageReserved),
				AllowScheduling: spec.AllowScheduling,
				Eviction:        spec.EvictionRequested,
				Ready:           conditionTrue(ds.Conditions, "Ready"),
				Schedulable:     conditionTrue(ds.Conditions, "Schedulable"),
				FSType:          ds.FilesystemType,
				SMART:           summarizeSMART(ds.HealthData),
				Conditions:      ds.Conditions,
			}
			if dv.Path == "" {
				dv.Path = spec.Path
			}
			for _, r := range byDisk[ds.DiskUUID] {
				rod := ReplicaOnDisk{Replica: r.Name, Volume: r.Spec.VolumeName, Size: int64(ds.ScheduledReplica[r.Name])}
				if v := vols[r.Spec.VolumeName]; v != nil {
					rod.PVC = v.PVC()
					if rod.Size == 0 {
						rod.Size = int64(v.Spec.Size)
					}
				}
				rod.Health = BuildReplicaView(r, nil, engines[r.Spec.VolumeName]).Health
				dv.Replicas = append(dv.Replicas, rod)
			}
			sort.Slice(dv.Replicas, func(i, j int) bool {
				a, b := dv.Replicas[i], dv.Replicas[j]
				if a.PVC != b.PVC {
					return a.PVC < b.PVC
				}
				return a.Replica < b.Replica
			})
			nv.Disks = append(nv.Disks, dv)
		}
		sort.Slice(nv.Disks, func(i, j int) bool { return nv.Disks[i].Name < nv.Disks[j].Name })
		out = append(out, nv)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Used returns the bytes in use on the disk's filesystem.
func (d *DiskView) Used() int64 { return d.Maximum - d.Available }

func summarizeSMART(hd map[string]DiskHealth) *SMARTSummary {
	if len(hd) == 0 {
		return nil
	}
	// Normally one entry per disk; pick the worst.
	var s *SMARTSummary
	for _, h := range hd {
		cur := &SMARTSummary{Status: h.HealthStatus, Model: h.ModelName, Temperature: h.Temperature, LifetimeLeft: -1, Device: h.DiskName}
		for _, a := range h.Attributes {
			switch a.Name {
			case "Percent_Lifetime_Remain", "Wear_Leveling_Count", "Media_Wearout_Indicator", "SSD_Life_Left":
				// The normalized value is the remaining life in percent;
				// the raw value differs per vendor (Crucial: % used).
				if a.Value > 0 && a.Value <= 100 {
					cur.LifetimeLeft = a.Value
				}
			case "Power_On_Hours":
				cur.PowerOnHours = a.RawValue
			case "Reallocated_Sector_Ct", "Reallocate_NAND_Blk_Cnt":
				cur.Reallocated += a.RawValue
			case "Current_Pending_Sector", "Current_Pending_ECC_Cnt":
				cur.PendingSector += a.RawValue
			}
		}
		if s == nil || (s.Status == "PASSED" && cur.Status != "PASSED") {
			s = cur
		}
	}
	return s
}
