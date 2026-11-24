package longhorn

import (
	"strings"
	"testing"
	"time"
)

func TestJobValidate(t *testing.T) {
	ok := JobSpec{Name: "nightly-backup", Task: "backup", Cron: "0 2 * * *", Retain: 7, Concurrency: 2, Groups: []string{"default"}}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mod := range map[string]func(*JobSpec){
		"bad name":      func(s *JobSpec) { s.Name = "Nightly" },
		"long name":     func(s *JobSpec) { s.Name = strings.Repeat("a", 41) },
		"bad task":      func(s *JobSpec) { s.Task = "backups" },
		"bad cron":      func(s *JobSpec) { s.Cron = "0 25 * * *" },
		"retain 0":      func(s *JobSpec) { s.Retain = 0 },
		"retain 101":    func(s *JobSpec) { s.Retain = 101 },
		"cleanup keeps": func(s *JobSpec) { s.Task, s.Retain = "snapshot-cleanup", 3 },
		"concurrency":   func(s *JobSpec) { s.Concurrency = 0 },
		"dup group":     func(s *JobSpec) { s.Groups = []string{"a", "a"} },
		"full interval": func(s *JobSpec) { s.Parameters = map[string]string{"full-backup-interval": "x"} },
	} {
		s := ok
		mod(&s)
		if s.Validate() == nil {
			t.Errorf("%s accepted", name)
		}
	}
	trim := JobSpec{Name: "trim", Task: "filesystem-trim", Cron: "@daily", Concurrency: 1}
	if err := trim.Validate(); err != nil {
		t.Errorf("trim: %v", err)
	}
}

func TestNextRuns(t *testing.T) {
	now := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC) // Tuesday
	runs := NextRuns("0 2 * * *", now, 3)
	if got := FormatRuns(runs, now); got != "tomorrow 02:00, Thu 02:00, Fri 02:00" {
		t.Errorf("%s", got)
	}
	if got := FormatRuns(NextRuns("30 23 * * *", now, 1), now); got != "today 23:30" {
		t.Errorf("%s", got)
	}
	// cron in UTC, shown in the caller's zone: 02:00 UTC = 03:00 CET
	cet := time.FixedZone("CET", 3600)
	if got := FormatRuns(NextRuns("0 2 * * *", now.In(cet), 1), now.In(cet)); got != "tomorrow 03:00" {
		t.Errorf("CET: %s", got)
	}
}

func TestJobActions(t *testing.T) {
	j := &RecurringJob{}
	j.Name, j.ResourceVersion, j.UID = "daily-backup", "42", "u1"
	j.Spec = RecurringJobSpec{Task: "backup", Cron: "0 3 * * *", Retain: 14, Concurrency: 1, Groups: []string{"default"},
		Labels: map[string]string{"a": "1"}}
	vol := func(name string, labels map[string]string) *Volume {
		v := &Volume{}
		v.Name, v.Labels = name, labels
		return v
	}
	vols := []*Volume{
		vol("v1", map[string]string{"recurring-job-group.longhorn.io/default": "enabled"}),
		vol("v2", map[string]string{"recurring-job.longhorn.io/daily-backup": "enabled"}),
		vol("v3", map[string]string{"recurring-job-group.longhorn.io/other": "enabled"}),
	}
	if got := strings.Join(JobVolumes(j, vols), ","); got != "v1,v2" {
		t.Errorf("users %s", got)
	}

	if _, err := CreateJob(SpecOf(j), []*RecurringJob{j}); err == nil {
		t.Error("duplicate created")
	}
	s := SpecOf(j)
	s.Name = "weekly"
	a, err := CreateJob(s, []*RecurringJob{j})
	if err != nil || a.Object["spec"].(map[string]any)["name"] != "weekly" {
		t.Fatalf("%v %+v", err, a)
	}

	s = SpecOf(j)
	s.Retain, s.Groups, s.Labels = 7, nil, map[string]string{"b": "2"}
	a, err = UpdateJob(j, s, vols)
	if err != nil {
		t.Fatal(err)
	}
	spec := a.Patch["spec"].(map[string]any)
	if spec["retain"] != int64(7) || len(spec["groups"].([]any)) != 0 || spec["cron"] != nil {
		t.Errorf("patch %v", spec)
	}
	if l := spec["labels"].(map[string]any); l["a"] != nil || l["b"] != "2" {
		t.Errorf("labels %v", l)
	}
	warn := strings.Join(a.Warnings, "\n")
	if !strings.Contains(warn, "lower retain") || !strings.Contains(warn, "1 volumes lose this job") {
		t.Errorf("warnings: %s", warn)
	}
	s = SpecOf(j)
	s.Task = "snapshot"
	if _, err := UpdateJob(j, s, vols); err == nil {
		t.Error("task change accepted")
	}
	if _, err := UpdateJob(j, SpecOf(j), vols); err == nil {
		t.Error("no-op accepted")
	}

	d, _ := DeleteJob(j, vols)
	if !d.Destructive || d.UID != "u1" || !strings.Contains(strings.Join(d.Warnings, " "), "2 volumes use this job") {
		t.Errorf("%+v", d)
	}
}
