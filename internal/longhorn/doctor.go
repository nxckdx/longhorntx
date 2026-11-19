package longhorn

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

// Doctor: read-only health checks over data lhtx already has (plus a few
// small, targeted reads). Diagnose is a pure function so it can be used by
// the CLI (`lhtx doctor`) and the TUI alike and is easy to test.

type Severity int

const (
	SevInfo Severity = iota + 1
	SevWarning
	SevCritical
)

func (s Severity) String() string {
	switch s {
	case SevCritical:
		return "CRITICAL"
	case SevWarning:
		return "WARNING"
	case SevInfo:
		return "INFO"
	}
	return "OK"
}

func (s Severity) MarshalText() ([]byte, error) { return []byte(strings.ToLower(s.String())), nil }

type Finding struct {
	Severity Severity `json:"severity"`
	Check    string   `json:"check"`
	Object   string   `json:"object"`
	Message  string   `json:"message"`
	Hint     string   `json:"hint,omitempty"`
}

// DoctorInput is everything Diagnose looks at. Engines and Snapshots only
// need to cover volumes that are rebuilding (see RebuildingVolumes).
type DoctorInput struct {
	State
	Snapshots    []*VolumeSnapshot
	Targets      []*BackupTarget
	Jobs         []*RecurringJob
	Orphans      int
	Now          time.Time
	BackupMaxAge time.Duration // 0: only check volumes with a backup recurring job

	// thresholds (zero values use defaults)
	DiskWarnPct, DiskCritPct  int
	RebuildSlow, BlockedAfter time.Duration
}

// RebuildingVolumes returns volumes with a replica that looks like it is
// being rebuilt – the only ones whose engines/snapshots Diagnose needs.
func RebuildingVolumes(replicas []*Replica) []string {
	set := map[string]bool{}
	for _, r := range replicas {
		if r.LikelyRebuilding() {
			set[r.Spec.VolumeName] = true
		}
	}
	out := make([]string, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func Diagnose(in DoctorInput) []Finding {
	if in.Now.IsZero() {
		in.Now = time.Now()
	}
	if in.DiskWarnPct == 0 {
		in.DiskWarnPct = 80
	}
	if in.DiskCritPct == 0 {
		in.DiskCritPct = 90
	}
	if in.RebuildSlow == 0 {
		in.RebuildSlow = 2 * time.Hour
	}
	if in.BlockedAfter == 0 {
		in.BlockedAfter = 30 * time.Minute
	}
	var f []Finding
	add := func(sev Severity, check, obj, msg, hint string) {
		f = append(f, Finding{sev, check, obj, msg, hint})
	}

	views := Build(in.State)
	engines := ActiveEngines(in.Engines)

	// ---- volumes and replicas
	for i := range views {
		v := &views[i]
		obj := volObj(v)
		switch v.Health {
		case "faulted":
			add(SevCritical, "volume", obj, "volume is faulted (no usable replica)", "check replicas and events: lhtx events "+obj)
		case "degraded":
			add(SevWarning, "volume", obj, fmt.Sprintf("volume is degraded: %d/%d replicas healthy", v.HealthyReplicas(), v.Desired), "")
		}
		if v.State == "attached" && v.Desired > 1 && v.HealthyReplicas() == 1 {
			add(SevWarning, "redundancy", obj, "only one healthy replica left", "losing its node or disk loses the data")
		}
		// two replicas on the same node or disk
		byNode, byDisk := map[string]int{}, map[string]int{}
		for _, r := range v.Replicas {
			if r.Health == HealthFailed {
				continue
			}
			byNode[r.Node]++
			byDisk[r.Node+":"+r.Disk.Path]++
		}
		for d, n := range byDisk {
			if n > 1 {
				add(SevWarning, "placement", obj, fmt.Sprintf("%d replicas on the same disk %s", n, d), "replica disk anti-affinity is not satisfied")
			}
		}
		for node, n := range byNode {
			if n > 1 && len(byDisk) > 1 {
				add(SevInfo, "placement", obj, fmt.Sprintf("%d replicas on the same node %s", n, node), "replica node anti-affinity is soft or not satisfiable")
			}
		}
		// rebuild checks need the engine
		if e := engines[v.Name]; e != nil {
			f = append(f, rebuildFindings(in, v, e)...)
		}
	}

	// ---- nodes and disks
	for _, n := range BuildNodes(in.State) {
		if !n.Ready {
			add(SevCritical, "node", n.Name, "node is not ready", "")
		}
		if n.Eviction {
			add(SevInfo, "node", n.Name, "eviction requested", "")
		} else if !n.Node.Spec.AllowScheduling {
			add(SevInfo, "node", n.Name, "replica scheduling disabled", "")
		}
		for _, d := range n.Disks {
			obj := n.Name + ":" + d.Path
			if !d.Ready {
				add(SevCritical, "disk", obj, "disk is not ready"+condMsg(d.Conditions, "Ready"), "")
			}
			if d.Maximum > 0 {
				pct := int(d.Used() * 100 / d.Maximum)
				switch {
				case pct >= in.DiskCritPct:
					add(SevCritical, "disk", obj, fmt.Sprintf("disk %d%% full", pct), "")
				case pct >= in.DiskWarnPct:
					add(SevWarning, "disk", obj, fmt.Sprintf("disk %d%% full", pct), "")
				}
			}
			if d.Ready && d.AllowScheduling && !d.Schedulable {
				add(SevWarning, "disk", obj, "Longhorn does not schedule new replicas here"+condMsg(d.Conditions, "Schedulable"), "")
			}
			if d.Eviction {
				add(SevInfo, "disk", obj, "eviction requested", "")
			} else if !d.AllowScheduling && n.Node.Spec.AllowScheduling {
				add(SevInfo, "disk", obj, "replica scheduling disabled", "")
			}
			if s := d.SMART; s != nil {
				if s.Status != "" && s.Status != "PASSED" {
					add(SevCritical, "smart", obj, "SMART status "+s.Status+smartModel(s), "")
				}
				switch {
				case s.LifetimeLeft >= 0 && s.LifetimeLeft < 10:
					add(SevCritical, "smart", obj, fmt.Sprintf("SSD life left %d%%%s", s.LifetimeLeft, smartModel(s)), "")
				case s.LifetimeLeft >= 0 && s.LifetimeLeft < 20:
					add(SevWarning, "smart", obj, fmt.Sprintf("SSD life left %d%%%s", s.LifetimeLeft, smartModel(s)), "")
				}
				if s.Reallocated > 0 || s.PendingSector > 0 {
					add(SevWarning, "smart", obj, fmt.Sprintf("%d reallocated, %d pending sectors%s", s.Reallocated, s.PendingSector, smartModel(s)), "")
				}
			}
		}
	}

	// ---- backups
	for _, t := range in.Targets {
		if !t.Status.Available && t.Spec.BackupTargetURL != "" {
			add(SevCritical, "backup-target", t.Name, "backup target unavailable"+condMsg(t.Status.Conditions, "Unavailable"), "")
		}
	}
	f = append(f, backupAgeFindings(in, views)...)

	if in.Orphans > 0 {
		add(SevInfo, "orphans", fmt.Sprintf("%d orphans", in.Orphans), "orphaned replica data or instances exist", "they use disk space; review with kubectl -n longhorn-system get orphans.longhorn.io")
	}

	sort.SliceStable(f, func(i, j int) bool {
		if f[i].Severity != f[j].Severity {
			return f[i].Severity > f[j].Severity
		}
		if f[i].Check != f[j].Check {
			return f[i].Check < f[j].Check
		}
		return f[i].Object < f[j].Object
	})
	return f
}

func rebuildFindings(in DoctorInput, v *VolumeView, e *Engine) []Finding {
	var f []Finding
	obj := volObj(v)
	// pending snapshot removal for this volume
	removing := 0
	for _, s := range in.Snapshots {
		if s.Spec.Volume == v.Name && (s.Status.MarkRemoved || s.DeletionTimestamp != nil) {
			removing++
		}
	}
	purging := false
	for _, p := range e.Status.PurgeStatus {
		if p.IsPurging {
			purging = true
		}
	}
	for _, r := range v.Replicas {
		if r.Health != HealthRebuilding {
			continue
		}
		since, ok := parseTime(e.Status.ReplicaTransitionTimeMap[r.Name])
		if !ok {
			continue
		}
		dur := in.Now.Sub(since)
		prog := "unknown progress"
		if r.Rebuild != nil {
			prog = fmt.Sprintf("%d%%", r.Rebuild.Progress)
		}
		if dur >= in.BlockedAfter && (removing > 0 || purging) {
			f = append(f, Finding{SevWarning, "rebuild-blocked", obj,
				fmt.Sprintf("replica %s rebuilding for %s (%s) while %s – rebuild and snapshot removal may block each other",
					shortName(r.Name), roundDur(dur), prog, removalText(removing, purging)),
				fmt.Sprintf("known workaround: lower the replica count until the snapshot is purged, then restore it (lhtx set replicas %s N). Save engine status and snapshots first for a bug report.", obj)})
		} else if dur >= in.RebuildSlow {
			f = append(f, Finding{SevWarning, "rebuild-slow", obj,
				fmt.Sprintf("replica %s rebuilding for %s (%s)", shortName(r.Name), roundDur(dur), prog), ""})
		}
	}
	return f
}

func removalText(n int, purging bool) string {
	var p []string
	if n > 0 {
		p = append(p, fmt.Sprintf("%d snapshot(s) wait for removal", n))
	}
	if purging {
		p = append(p, "a purge is running")
	}
	return strings.Join(p, " and ")
}

// BackupStatus is the backup state of one volume.
type BackupStatus struct {
	Volume   string        `json:"volume"`
	PVC      string        `json:"pvc,omitempty"`
	Last     string        `json:"lastBackupAt,omitempty"`
	Age      time.Duration `json:"ageNs,omitempty"`
	Job      string        `json:"job,omitempty"`        // backup job that applies, or "--backup-max-age"
	Interval time.Duration `json:"intervalNs,omitempty"` // schedule interval of the job
	MaxAge   time.Duration `json:"maxAgeNs,omitempty"`
	Status   string        `json:"status"` // ok | late | never | unchecked
}

// BackupOverview evaluates every volume: the expected maximum age is twice
// the interval of the most frequent backup job that applies (+1h slack), or
// maxAge for volumes without such a job (0: unchecked).
func BackupOverview(vols []*Volume, jobs []*RecurringJob, now time.Time, maxAge time.Duration) []BackupStatus {
	out := make([]BackupStatus, 0, len(vols))
	for _, v := range vols {
		bs := BackupStatus{Volume: v.Name, PVC: v.PVC(), Last: v.Status.LastBackupAt}
		for _, j := range VolumeRecurringJobs(v, jobs) {
			if j.Spec.Task != "backup" && j.Spec.Task != "backup-force-create" {
				continue
			}
			if p, ok := cronPeriod(j.Spec.Cron, now); ok && (bs.Interval == 0 || p < bs.Interval) {
				bs.Interval, bs.Job = p, j.Name
			}
		}
		switch {
		case bs.Interval > 0:
			bs.MaxAge = 2*bs.Interval + time.Hour
		case maxAge > 0:
			bs.MaxAge, bs.Job = maxAge, "--backup-max-age"
		}
		last, ok := parseTime(v.Status.LastBackupAt)
		if ok {
			bs.Age = now.Sub(last)
		}
		switch {
		case bs.MaxAge == 0:
			bs.Status = "unchecked"
		case !ok:
			bs.Status = "never"
		case bs.Age > bs.MaxAge:
			bs.Status = "late"
		default:
			bs.Status = "ok"
		}
		out = append(out, bs)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.PVC != b.PVC {
			return a.PVC < b.PVC
		}
		return a.Volume < b.Volume
	})
	return out
}

func backupAgeFindings(in DoctorInput, views []VolumeView) []Finding {
	var f []Finding
	for _, bs := range BackupOverview(in.Volumes, in.Jobs, in.Now, in.BackupMaxAge) {
		obj := bs.PVC
		if obj == "" {
			obj = bs.Volume
		}
		switch bs.Status {
		case "never":
			f = append(f, Finding{SevWarning, "backup-age", obj, "never backed up (expected by " + bs.Job + ")", ""})
		case "late":
			f = append(f, Finding{SevWarning, "backup-age", obj,
				fmt.Sprintf("last backup %s ago, expected within %s (%s)", roundDur(bs.Age), roundDur(bs.MaxAge), bs.Job),
				"check the recurring job and the backup target"})
		}
	}
	return f
}

// cronPeriod estimates a job's interval from two consecutive runs.
func cronPeriod(spec string, now time.Time) (time.Duration, bool) {
	sched, err := cron.ParseStandard(spec)
	if err != nil {
		return 0, false
	}
	a := sched.Next(now)
	b := sched.Next(a)
	c := sched.Next(b)
	// irregular schedules (e.g. "0 1 * * 1,4"): use the longer gap
	return max(b.Sub(a), c.Sub(b)), true
}

func volObj(v *VolumeView) string {
	if v.PVC != "" {
		return v.PVC
	}
	return v.Name
}

func condMsg(cs []Condition, typ string) string {
	for _, c := range cs {
		if c.Type == typ && c.Message != "" {
			return ": " + c.Message
		}
	}
	return ""
}

func smartModel(s *SMARTSummary) string {
	if s.Model == "" {
		return ""
	}
	return " (" + s.Model + ")"
}

func parseTime(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339, s)
	return t, err == nil
}

func roundDur(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}

func shortName(replica string) string {
	if i := strings.LastIndex(replica, "-r-"); i >= 0 {
		return replica[i+1:]
	}
	return replica
}

// Worst returns the highest severity in the findings (0 if none).
func Worst(fs []Finding) Severity {
	var w Severity
	for _, f := range fs {
		w = max(w, f.Severity)
	}
	return w
}

// DrainCheck reports what happens to volumes if node goes away (reboot,
// drain, replacement). Read-only; based on the current state.
func DrainCheck(st State, node string) []Finding {
	var f []Finding
	for _, v := range Build(st) {
		obj := volObj(&v)
		var here, healthyHere, healthyElsewhere, rebuildingElsewhere int
		for _, r := range v.Replicas {
			if r.Node == node {
				here++
				if r.Health == HealthHealthy {
					healthyHere++
				}
				continue
			}
			switch r.Health {
			case HealthHealthy:
				healthyElsewhere++
			case HealthRebuilding:
				rebuildingElsewhere++
			}
		}
		attachedHere := v.State == "attached" && v.Node == node
		if here == 0 && !attachedHere {
			continue
		}
		switch {
		case healthyHere > 0 && healthyElsewhere == 0:
			msg := fmt.Sprintf("the only healthy replica is on %s – the volume is unavailable while it is away", node)
			if v.Desired == 1 {
				msg += " (volume has 1 replica by design)"
			}
			f = append(f, Finding{SevCritical, "last-replica", obj, msg,
				"evict first (lhtx evict " + node + ") or raise the replica count and wait for the rebuild"})
		case here > 0 && healthyElsewhere == 1 && v.Desired > 2:
			f = append(f, Finding{SevWarning, "redundancy", obj,
				fmt.Sprintf("only 1 of %d replicas healthy while %s is away", v.Desired, node), ""})
		case here > 0 && healthyElsewhere == 1:
			f = append(f, Finding{SevInfo, "redundancy", obj,
				fmt.Sprintf("degraded while %s is away (1 of %d replicas)", node, v.Desired), ""})
		}
		if here > 0 && rebuildingElsewhere > 0 {
			f = append(f, Finding{SevWarning, "rebuilding", obj, "a replica on another node is still rebuilding – wait for it", ""})
		}
		if attachedHere {
			f = append(f, Finding{SevInfo, "attached", obj,
				fmt.Sprintf("attached on %s – the workload must move; the volume is re-attached elsewhere", node), ""})
		}
	}
	sort.SliceStable(f, func(i, j int) bool {
		if f[i].Severity != f[j].Severity {
			return f[i].Severity > f[j].Severity
		}
		return f[i].Object < f[j].Object
	})
	return f
}
