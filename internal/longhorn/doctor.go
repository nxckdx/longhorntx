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

// backupAgeFindings compares volume.status.lastBackupAt with the schedule of
// the backup jobs that apply to the volume (or BackupMaxAge).
func backupAgeFindings(in DoctorInput, views []VolumeView) []Finding {
	var f []Finding
	for i := range views {
		v := &views[i]
		if v.Volume.Status.State == "" {
			continue
		}
		var maxAge time.Duration
		var jobName string
		for _, j := range VolumeRecurringJobs(v.Volume, in.Jobs) {
			if j.Spec.Task != "backup" && j.Spec.Task != "backup-force-create" {
				continue
			}
			if p, ok := cronPeriod(j.Spec.Cron, in.Now); ok && (maxAge == 0 || 2*p < maxAge) {
				maxAge, jobName = 2*p+time.Hour, j.Name
			}
		}
		source := "job " + jobName
		if maxAge == 0 {
			if in.BackupMaxAge == 0 {
				continue
			}
			maxAge, source = in.BackupMaxAge, "--backup-max-age"
		}
		last, ok := parseTime(v.Volume.Status.LastBackupAt)
		obj := volObj(v)
		switch {
		case !ok:
			f = append(f, Finding{SevWarning, "backup-age", obj, "never backed up (expected by " + source + ")", ""})
		case in.Now.Sub(last) > maxAge:
			f = append(f, Finding{SevWarning, "backup-age", obj,
				fmt.Sprintf("last backup %s ago, expected within %s (%s)", roundDur(in.Now.Sub(last)), roundDur(maxAge), source),
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
