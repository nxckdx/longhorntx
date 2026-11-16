package longhorn

import (
	"sort"
	"strconv"
	"sync"
	"time"
)

// State is a consistent-enough set of objects to build views from. The CLI
// fills it with one-shot lists, the TUI from informer caches.
type State struct {
	Volumes  []*Volume
	Replicas []*Replica
	Nodes    []*Node
	// Engines may be partial: lhtx only watches engines when needed.
	Engines []*Engine
}

// DiskRef identifies a Longhorn disk on a node.
type DiskRef struct {
	Node string `json:"node"`
	Name string `json:"name"` // key in node.spec.disks; "" if unknown
	UUID string `json:"uuid"`
	Path string `json:"path"` // directory (v1) or block device (v2)
	Type string `json:"type"` // filesystem | block
}

// DiskIndex maps disk UUID -> DiskRef.
type DiskIndex map[string]DiskRef

func IndexDisks(nodes []*Node) DiskIndex {
	idx := DiskIndex{}
	for _, n := range nodes {
		for name, ds := range n.Status.DiskStatus {
			if ds.DiskUUID == "" {
				continue
			}
			idx[ds.DiskUUID] = DiskRef{Node: n.Name, Name: name, UUID: ds.DiskUUID, Path: ds.DiskPath, Type: ds.DiskType}
		}
	}
	return idx
}

// Disk resolves the disk a replica lives on. Falls back to the replica's own
// diskPath if the node/disk is unknown (e.g. node object deleted).
func (idx DiskIndex) Disk(r *Replica) DiskRef {
	if d, ok := idx[r.Spec.DiskID]; ok {
		return d
	}
	return DiskRef{Node: r.Spec.NodeID, UUID: r.Spec.DiskID, Path: r.Spec.DiskPath}
}

// ReplicaView is a replica joined with its disk and (if known) engine state.
type ReplicaView struct {
	Replica *Replica       `json:"-"`
	Name    string         `json:"name"`
	Node    string         `json:"node"`
	Disk    DiskRef        `json:"disk"`
	State   string         `json:"state"` // running | stopped | error | ...
	Health  string         `json:"health"`
	Mode    string         `json:"mode,omitempty"` // RW | WO | ERR, only with engine data
	Rebuild *RebuildStatus `json:"rebuild,omitempty"`
}

// Replica health classification.
const (
	HealthHealthy    = "healthy"
	HealthRebuilding = "rebuilding"
	HealthFailed     = "failed"
	HealthStopped    = "stopped"
	HealthUnknown    = "unknown"
)

func BuildReplicaView(r *Replica, disks DiskIndex, eng *Engine) ReplicaView {
	rv := ReplicaView{
		Replica: r,
		Name:    r.Name,
		Node:    r.Spec.NodeID,
		Disk:    disks.Disk(r),
		State:   r.Status.CurrentState,
	}
	if eng != nil {
		rv.Mode = eng.Status.ReplicaModeMap[r.Name]
		if rs, ok := eng.RebuildFor(r); ok && (rs.IsRebuilding || rs.State == "in_progress") {
			rv.Rebuild = &rs
		}
	}
	switch {
	case r.Spec.FailedAt != "" || r.Status.CurrentState == "error" || rv.Mode == "ERR":
		rv.Health = HealthFailed
	case rv.Rebuild != nil || rv.Mode == "WO" || r.LikelyRebuilding():
		rv.Health = HealthRebuilding
	case r.Status.CurrentState == "stopped":
		rv.Health = HealthStopped
	case r.Status.CurrentState == "running" && r.Spec.HealthyAt != "":
		rv.Health = HealthHealthy
	default:
		rv.Health = HealthUnknown
	}
	return rv
}

// ReplicaAddress returns the "tcp://ip:port" key used in the engine's status
// maps for the given replica.
func (e *Engine) ReplicaAddress(r *Replica) string {
	if a, ok := e.Status.CurrentReplicaAddressMap[r.Name]; ok && a != "" {
		return "tcp://" + a
	}
	ip := r.Status.StorageIP
	if ip == "" {
		ip = r.Status.IP
	}
	if ip == "" || r.Status.Port == 0 {
		return ""
	}
	return "tcp://" + ip + ":" + strconv.Itoa(r.Status.Port)
}

func (e *Engine) RebuildFor(r *Replica) (RebuildStatus, bool) {
	addr := e.ReplicaAddress(r)
	if addr == "" {
		return RebuildStatus{}, false
	}
	rs, ok := e.Status.RebuildStatus[addr]
	return rs, ok
}

// VolumeView is a volume with all of its replicas resolved.
type VolumeView struct {
	Volume   *Volume       `json:"-"`
	Name     string        `json:"name"`
	PVC      string        `json:"pvc,omitempty"`
	State    string        `json:"state"`
	Health   string        `json:"robustness"`
	Size     int64         `json:"size"`
	Actual   int64         `json:"actualSize"`
	Node     string        `json:"node,omitempty"`
	Engine   string        `json:"dataEngine"`
	Desired  int           `json:"numberOfReplicas"`
	Replicas []ReplicaView `json:"replicas"`
	// EngineKnown is false if no engine data was available, i.e. Mode and
	// rebuild progress of the replicas are not populated.
	EngineKnown bool `json:"engineKnown"`
}

func (vv *VolumeView) HealthyReplicas() int {
	n := 0
	for _, r := range vv.Replicas {
		if r.Health == HealthHealthy {
			n++
		}
	}
	return n
}

func (vv *VolumeView) Rebuilding() bool {
	for _, r := range vv.Replicas {
		if r.Health == HealthRebuilding {
			return true
		}
	}
	return false
}

// Build joins a snapshot into volume views, sorted by PVC then name.
func Build(s State) []VolumeView {
	disks := IndexDisks(s.Nodes)
	byVol := map[string][]*Replica{}
	for _, r := range s.Replicas {
		v := r.Spec.VolumeName
		if v == "" {
			v = r.Labels[LabelVolume]
		}
		byVol[v] = append(byVol[v], r)
	}
	engines := ActiveEngines(s.Engines)

	out := make([]VolumeView, 0, len(s.Volumes))
	for _, v := range s.Volumes {
		out = append(out, BuildVolumeView(v, byVol[v.Name], disks, engines[v.Name]))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].PVC != out[j].PVC {
			// volumes without PVC last
			if out[i].PVC == "" || out[j].PVC == "" {
				return out[j].PVC == ""
			}
			return out[i].PVC < out[j].PVC
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// ActiveEngines returns volume name -> active engine.
func ActiveEngines(engs []*Engine) map[string]*Engine {
	m := map[string]*Engine{}
	for _, e := range engs {
		v := e.Spec.VolumeName
		if cur, ok := m[v]; !ok || (!cur.Spec.Active && e.Spec.Active) {
			m[v] = e
		}
	}
	return m
}

func BuildVolumeView(v *Volume, reps []*Replica, disks DiskIndex, eng *Engine) VolumeView {
	node := v.Status.CurrentNodeID
	vv := VolumeView{
		Volume:      v,
		Name:        v.Name,
		PVC:         v.PVC(),
		State:       v.Status.State,
		Health:      v.Status.Robustness,
		Size:        int64(v.Spec.Size),
		Actual:      int64(v.Status.ActualSize),
		Node:        node,
		Engine:      v.Spec.DataEngine,
		Desired:     v.Spec.NumberOfReplicas,
		EngineKnown: eng != nil,
	}
	for _, r := range reps {
		vv.Replicas = append(vv.Replicas, BuildReplicaView(r, disks, eng))
	}
	sort.Slice(vv.Replicas, func(i, j int) bool {
		a, b := vv.Replicas[i], vv.Replicas[j]
		if a.Node != b.Node {
			return a.Node < b.Node
		}
		return a.Name < b.Name
	})
	return vv
}

// ---------------------------------------------------------------- rates

// RateTracker derives throughput and ETA from successive progress samples
// (Longhorn only reports a percentage). Safe for concurrent use.
type RateTracker struct {
	mu sync.Mutex
	m  map[string]*rateState
}

type rateState struct {
	lastPct  int
	lastAt   time.Time
	bps      float64 // smoothed bytes/s
	seenMove bool
}

// Rate is a throughput estimate. Valid is false until two distinct progress
// values have been observed.
type Rate struct {
	BytesPerSec float64
	ETA         time.Duration
	Valid       bool
}

func NewRateTracker() *RateTracker { return &RateTracker{m: map[string]*rateState{}} }

// Observe records progress (0-100) of an operation covering total bytes.
func (t *RateTracker) Observe(key string, pct int, total int64, now time.Time) Rate {
	t.mu.Lock()
	defer t.mu.Unlock()
	st, ok := t.m[key]
	if !ok || pct < st.lastPct {
		t.m[key] = &rateState{lastPct: pct, lastAt: now}
		return Rate{}
	}
	if pct > st.lastPct {
		dt := now.Sub(st.lastAt).Seconds()
		if dt > 0 {
			inst := float64(pct-st.lastPct) / 100 * float64(total) / dt
			if st.seenMove {
				st.bps = 0.7*st.bps + 0.3*inst
			} else {
				st.bps = inst
			}
			st.seenMove = true
		}
		st.lastPct, st.lastAt = pct, now
	}
	if !st.seenMove || st.bps <= 0 {
		return Rate{}
	}
	remaining := float64(100-pct) / 100 * float64(total)
	return Rate{BytesPerSec: st.bps, ETA: time.Duration(remaining / st.bps * float64(time.Second)), Valid: true}
}

// Get returns the current estimate without recording a sample.
func (t *RateTracker) Get(key string, pct int, total int64) Rate {
	t.mu.Lock()
	defer t.mu.Unlock()
	st, ok := t.m[key]
	if !ok || !st.seenMove || st.bps <= 0 {
		return Rate{}
	}
	remaining := float64(100-pct) / 100 * float64(total)
	return Rate{BytesPerSec: st.bps, ETA: time.Duration(remaining / st.bps * float64(time.Second)), Valid: true}
}

// Forget drops state for keys not in keep.
func (t *RateTracker) Forget(keep map[string]bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for k := range t.m {
		if !keep[k] {
			delete(t.m, k)
		}
	}
}
