package longhorn

import (
	"strings"
	"testing"
)

func TestJobAssignments(t *testing.T) {
	job := func(name string, groups ...string) *RecurringJob {
		j := &RecurringJob{}
		j.Name, j.Spec.Groups = name, groups
		return j
	}
	jobs := []*RecurringJob{job("daily", "default"), job("weekly"), job("hourly", "default", "fast")}
	v := &Volume{}
	v.Name = "pvc-1"
	v.Labels = map[string]string{"recurring-job-group.longhorn.io/default": "enabled", "recurring-job.longhorn.io/weekly": "enabled"}

	how := map[string]string{}
	for _, a := range JobAssignments(v, jobs) {
		how[a.Ref.String()] = a.How
	}
	want := map[string]string{"group:default": "direct", "group:fast": "", "daily": "via group default", "hourly": "via group default", "weekly": "direct"}
	for k, w := range want {
		if how[k] != w {
			t.Errorf("%s: got %q, want %q", k, how[k], w)
		}
	}

	a, err := SetJobAssignment(v, []JobRef{ParseJobRef("group:fast"), ParseJobRef("daily")}, true, jobs,
		map[string]string{"recurring-job.longhorn.io/source": "enabled"})
	if err != nil {
		t.Fatal(err)
	}
	labels := a.Patch["metadata"].(map[string]any)["labels"].(map[string]any)
	if labels["recurring-job-group.longhorn.io/fast"] != "enabled" || labels["recurring-job.longhorn.io/daily"] != "enabled" {
		t.Errorf("unexpected labels: %v", labels)
	}
	if !strings.Contains(strings.Join(a.Warnings, " "), "source=enabled") {
		t.Error("expected a warning about PVC-sourced job labels")
	}

	a, err = SetJobAssignment(v, []JobRef{ParseJobRef("weekly")}, false, jobs, nil)
	if err != nil || a.Patch["metadata"].(map[string]any)["labels"].(map[string]any)["recurring-job.longhorn.io/weekly"] != nil {
		t.Errorf("remove: %v %v", a, err)
	}
	if _, err := SetJobAssignment(v, []JobRef{ParseJobRef("daily")}, false, jobs, nil); err == nil {
		t.Error("removing a job that applies only via a group must fail")
	}
}
