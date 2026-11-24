package longhorn

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

// Managing RecurringJob CRs (what the web UI's "Recurring Job" tab does).
// longhorn-manager validates jobs in its webhook (cron with the standard
// parser, retain, concurrency, names); lhtx checks the same up front so
// mistakes show before the request, and a server dry run can confirm.

// JobTasks are the tasks of RecurringJob.spec.task (v1.12.1 CRD enum).
var JobTasks = []string{"snapshot", "snapshot-force-create", "snapshot-cleanup", "snapshot-delete",
	"backup", "backup-force-create", "filesystem-trim", "system-backup"}

// MaxJobRetain is longhorn-manager's upper limit for retain.
const MaxJobRetain = 100

// JobSpec is the editable part of a recurring job.
type JobSpec struct {
	Name        string
	Task        string
	Cron        string
	Retain      int
	Concurrency int
	Groups      []string
	Labels      map[string]string
	Parameters  map[string]string
}

// SpecOf returns the editable spec of an existing job.
func SpecOf(j *RecurringJob) JobSpec {
	return JobSpec{Name: j.Name, Task: j.Spec.Task, Cron: j.Spec.Cron, Retain: j.Spec.Retain,
		Concurrency: j.Spec.Concurrency, Groups: slices.Clone(j.Spec.Groups),
		Labels: cloneMap(j.Spec.Labels), Parameters: cloneMap(j.Spec.Parameters)}
}

func cloneMap(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// taskUsesRetain: cleanup and trim keep nothing, so retain must be 0.
func taskUsesRetain(task string) bool {
	return task != "snapshot-cleanup" && task != "filesystem-trim"
}

var nameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// Validate checks a spec like longhorn-manager's webhook does.
func (s JobSpec) Validate() error {
	if !nameRE.MatchString(s.Name) || len(s.Name) > 40 {
		return fmt.Errorf("name %q: lowercase letters, digits and '-', at most 40 characters", s.Name)
	}
	if !slices.Contains(JobTasks, s.Task) {
		return fmt.Errorf("task %q: one of %s", s.Task, strings.Join(JobTasks, ", "))
	}
	if _, err := ParseCron(s.Cron); err != nil {
		return err
	}
	switch {
	case !taskUsesRetain(s.Task) && s.Retain != 0:
		return fmt.Errorf("retain must be 0 for %s", s.Task)
	case taskUsesRetain(s.Task) && (s.Retain < 1 || s.Retain > MaxJobRetain):
		return fmt.Errorf("retain must be 1–%d for %s", MaxJobRetain, s.Task)
	}
	if s.Concurrency < 1 {
		return fmt.Errorf("concurrency must be at least 1")
	}
	seen := map[string]bool{}
	for _, g := range s.Groups {
		if !nameRE.MatchString(g) {
			return fmt.Errorf("group %q: lowercase letters, digits and '-'", g)
		}
		if seen[g] {
			return fmt.Errorf("group %q twice", g)
		}
		seen[g] = true
	}
	if v, ok := s.Parameters["full-backup-interval"]; ok {
		if n, err := strconv.Atoi(v); err != nil || n < 0 {
			return fmt.Errorf("full-backup-interval must be a number ≥ 0 (every n-th backup is a full one, 0 = never)")
		}
	}
	if v, ok := s.Parameters["volume-backup-policy"]; ok && !slices.Contains([]string{"if-not-present", "always", "disabled"}, v) {
		return fmt.Errorf("volume-backup-policy: if-not-present, always or disabled")
	}
	return nil
}

// ParseCron parses a cron expression like Longhorn (5 fields or @daily …).
func ParseCron(expr string) (cron.Schedule, error) {
	sch, err := cron.ParseStandard(strings.TrimSpace(expr))
	if err != nil {
		return nil, fmt.Errorf("cron %q: %v (5 fields: minute hour day month weekday, e.g. \"0 2 * * *\")", expr, err)
	}
	return sch, nil
}

// NextRuns returns the next n run times after now – the best way to see
// whether a cron expression means what you think. Longhorn's CronJobs have
// no timeZone, so they run in the kube-controller-manager's zone, which is
// practically always UTC: the schedule is evaluated in UTC and the times
// are returned in now's location.
func NextRuns(expr string, now time.Time, n int) []time.Time {
	sch, err := ParseCron(expr)
	if err != nil {
		return nil
	}
	var out []time.Time
	t := now.UTC()
	for range n {
		t = sch.Next(t)
		if t.IsZero() {
			break
		}
		out = append(out, t.In(now.Location()))
	}
	return out
}

// FormatRuns renders run times compactly: "today 23:00, tomorrow 23:00, Wed 23:00".
func FormatRuns(ts []time.Time, now time.Time) string {
	parts := make([]string, 0, len(ts))
	y, m, d := now.Date()
	today := time.Date(y, m, d, 0, 0, 0, 0, now.Location())
	for _, t := range ts {
		day := t.Format("Mon 2 Jan")
		switch days := int(t.Sub(today).Hours() / 24); {
		case days == 0:
			day = "today"
		case days == 1:
			day = "tomorrow"
		case days < 7:
			day = t.Format("Mon")
		}
		parts = append(parts, day+" "+t.Format("15:04"))
	}
	return strings.Join(parts, ", ")
}

func (s JobSpec) object() map[string]any {
	groups := s.Groups
	if groups == nil {
		groups = []string{}
	}
	labels, params := map[string]any{}, map[string]any{}
	for k, v := range s.Labels {
		labels[k] = v
	}
	for k, v := range s.Parameters {
		params[k] = v
	}
	return map[string]any{
		"name": s.Name, "task": s.Task, "cron": s.Cron, "retain": int64(s.Retain),
		"concurrency": int64(s.Concurrency), "groups": toAny(groups), "labels": labels, "parameters": params,
	}
}

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// CreateJob builds the create action for a new recurring job.
func CreateJob(s JobSpec, existing []*RecurringJob) (*Action, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	for _, j := range existing {
		if j.Name == s.Name {
			return nil, fmt.Errorf("recurring job %q exists", s.Name)
		}
	}
	a := &Action{
		Title:  fmt.Sprintf("Create recurring job %s (%s, %s)", s.Name, s.Task, s.Cron),
		Verb:   VerbCreate,
		GVR:    GVRRecurringJob,
		Name:   s.Name,
		Object: lhObject("RecurringJob", s.Name, nil, s.object()),
	}
	a.Warnings = append(a.Warnings, jobWarnings(s, time.Now())...)
	return a, nil
}

// UpdateJob patches the fields that changed (name and task are fixed:
// Longhorn does not allow changing the task of an existing job).
func UpdateJob(j *RecurringJob, s JobSpec, vols []*Volume) (*Action, error) {
	s.Name = j.Name
	if s.Task != j.Spec.Task {
		return nil, fmt.Errorf("the task of a job cannot change (%s → %s): create a new job instead", j.Spec.Task, s.Task)
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	old := SpecOf(j)
	patch := map[string]any{}
	var ch []Change
	if s.Cron != old.Cron {
		patch["cron"], ch = s.Cron, append(ch, Change{"spec.cron", old.Cron, s.Cron})
	}
	if s.Retain != old.Retain {
		patch["retain"], ch = int64(s.Retain), append(ch, Change{"spec.retain", fmt.Sprint(old.Retain), fmt.Sprint(s.Retain)})
	}
	if s.Concurrency != old.Concurrency {
		patch["concurrency"], ch = int64(s.Concurrency), append(ch, Change{"spec.concurrency", fmt.Sprint(old.Concurrency), fmt.Sprint(s.Concurrency)})
	}
	if !slices.Equal(sortedCopy(s.Groups), sortedCopy(old.Groups)) {
		g := s.Groups
		if g == nil {
			g = []string{}
		}
		patch["groups"] = toAny(g) // lists are replaced as a whole by a merge patch
		ch = append(ch, Change{"spec.groups", strings.Join(old.Groups, ","), strings.Join(s.Groups, ",")})
	}
	if lp := mapPatch(old.Labels, s.Labels); lp != nil {
		patch["labels"], ch = lp, append(ch, Change{"spec.labels", fmtMap(old.Labels), fmtMap(s.Labels)})
	}
	if pp := mapPatch(old.Parameters, s.Parameters); pp != nil {
		patch["parameters"], ch = pp, append(ch, Change{"spec.parameters", fmtMap(old.Parameters), fmtMap(s.Parameters)})
	}
	if len(patch) == 0 {
		return nil, fmt.Errorf("nothing changed")
	}
	a := &Action{
		Title: "Change recurring job " + j.Name,
		Verb:  VerbPatch, GVR: GVRRecurringJob, Name: j.Name,
		Patch: patchWithRV(j.ResourceVersion, patch), Changes: ch,
	}
	a.Warnings = append(gitopsWarning(j), append(a.Warnings, jobWarnings(s, time.Now())...)...)
	if s.Retain < old.Retain && taskUsesRetain(s.Task) {
		a.Warnings = append(a.Warnings, fmt.Sprintf("lower retain: on the next run Longhorn deletes the oldest %ss beyond %d per volume",
			retainWhat(s.Task), s.Retain))
	}
	if removed := removedGroups(old.Groups, s.Groups); len(removed) > 0 {
		after := *j
		after.Spec = specWithGroups(j, s.Groups)
		if n := len(JobVolumes(j, vols)) - len(JobVolumes(&after, vols)); n > 0 {
			a.Warnings = append(a.Warnings, fmt.Sprintf("%d volumes lose this job (group %s removed)", n, strings.Join(removed, ", ")))
		}
	}
	return a, nil
}

func specWithGroups(j *RecurringJob, groups []string) RecurringJobSpec {
	s := j.Spec
	s.Groups = groups
	return s
}

func retainWhat(task string) string {
	if strings.HasPrefix(task, "backup") {
		return "backup"
	}
	return "snapshot"
}

func removedGroups(old, cur []string) []string {
	var out []string
	for _, g := range old {
		if !slices.Contains(cur, g) {
			out = append(out, g)
		}
	}
	return out
}

func sortedCopy(s []string) []string {
	c := slices.Clone(s)
	sort.Strings(c)
	return c
}

// mapPatch is a merge patch from old to new (nil values delete keys); nil
// if nothing changed.
func mapPatch(old, cur map[string]string) map[string]any {
	p := map[string]any{}
	for k, v := range cur {
		if old[k] != v {
			p[k] = v
		}
	}
	for k := range old {
		if _, ok := cur[k]; !ok {
			p[k] = nil
		}
	}
	if len(p) == 0 {
		return nil
	}
	return p
}

func fmtMap(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + m[k]
	}
	return strings.Join(parts, ",")
}

func jobWarnings(s JobSpec, now time.Time) []string {
	var w []string
	if runs := NextRuns(s.Cron, now, 3); len(runs) > 0 {
		w = append(w, "next runs: "+FormatRuns(runs, now)+" "+now.Format("MST")+" (cron is evaluated in UTC by the cluster)")
	}
	if len(s.Groups) == 0 {
		w = append(w, "no group: the job only runs for volumes it is assigned to directly (lhtx jobs add / J panel)")
	}
	if slices.Contains(s.Groups, "default") {
		w = append(w, "group default: applies to every volume without its own job or group labels")
	}
	if s.Task == "snapshot-delete" {
		w = append(w, "snapshot-delete removes snapshots beyond retain, including ones you created by hand")
	}
	return w
}

// JobVolumes returns the volumes a job applies to (directly or via a group).
func JobVolumes(j *RecurringJob, vols []*Volume) []string {
	var out []string
	for _, v := range vols {
		if slices.ContainsFunc(VolumeRecurringJobs(v, []*RecurringJob{j}), func(x *RecurringJob) bool { return x == j }) {
			out = append(out, volKeyOf(v))
		}
	}
	sort.Strings(out)
	return out
}

func volKeyOf(v *Volume) string {
	if p := v.PVC(); p != "" {
		return p
	}
	return v.Name
}

// DeleteJob deletes a job. Longhorn removes its CronJob; volume labels that
// name the job stay (harmless) – the preview lists who used it.
func DeleteJob(j *RecurringJob, vols []*Volume) (*Action, error) {
	a := &Action{
		Title: fmt.Sprintf("Delete recurring job %s (%s, %s)", j.Name, j.Spec.Task, j.Spec.Cron),
		Verb:  VerbDelete, GVR: GVRRecurringJob, Name: j.Name, UID: string(j.UID),
		Destructive: true,
		Warnings:    gitopsWarning(j),
	}
	if users := JobVolumes(j, vols); len(users) > 0 {
		shown := users
		if len(shown) > 8 {
			shown = append(slices.Clone(shown[:8]), fmt.Sprintf("… %d more", len(users)-8))
		}
		a.Warnings = append(a.Warnings, fmt.Sprintf("%d volumes use this job and lose it: %s", len(users), strings.Join(shown, ", ")))
		if strings.HasPrefix(j.Spec.Task, "backup") {
			a.Warnings = append(a.Warnings, "check that these volumes are still backed up by another job (lhtx backups)")
		}
	}
	a.Warnings = append(a.Warnings, "existing snapshots and backups made by the job stay")
	return a, nil
}

// SortJobs orders jobs by name.
func SortJobs(js []*RecurringJob) {
	sort.Slice(js, func(i, k int) bool { return js[i].Name < js[k].Name })
}
