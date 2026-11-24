package tui

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nxckdx/longhorntx/internal/format"
	"github.com/nxckdx/longhorntx/internal/longhorn"
)

// Recurring jobs page (Backups tab 6 → j): all jobs with schedule, next
// run and the number of volumes they apply to (from the live volume list).
// Write mode: n new, enter/e edit, X delete – each through a form and the
// usual preview/confirmation.

type jobsMsg struct {
	client *longhorn.Client
	jobs   []*longhorn.RecurringJob
	err    error
}

func (m *model) loadJobs() tea.Cmd {
	if m.client == nil {
		return nil
	}
	c, ctx := m.client, m.ctx
	m.jobsLoading = true
	return func() tea.Msg {
		js, err := c.RecurringJobs(ctx)
		return jobsMsg{client: c, jobs: js, err: err}
	}
}

func (m *model) applyJobs(msg jobsMsg) {
	if msg.client != m.client {
		return
	}
	m.jobsLoading, m.jobsErr = false, msg.err
	if msg.err == nil {
		longhorn.SortJobs(msg.jobs)
		m.jobsRaw, m.jobsAt = msg.jobs, time.Now()
	}
	m.clamp()
}

func (m *model) jobRows() []row {
	now := time.Now()
	rows := make([]row, 0, len(m.jobsRaw))
	for _, j := range m.jobsRaw {
		users := len(longhorn.JobVolumes(j, m.state.Volumes))
		vols := fmt.Sprint(users)
		if users == 0 {
			vols = sDim.Render("0")
		}
		next := longhorn.FormatRuns(longhorn.NextRuns(j.Spec.Cron, now, 1), now)
		task := j.Spec.Task
		if strings.HasPrefix(task, "backup") {
			task = fg(cInfo, task)
		}
		j := j
		rows = append(rows, row{
			cells: []string{j.Name, task, j.Spec.Cron, dash(next), fmt.Sprint(j.Spec.Retain), fmt.Sprint(j.Spec.Concurrency),
				dash(strings.Join(j.Spec.Groups, ",")), vols, fmt.Sprint(j.Status.ExecutionCount)},
			ref: j,
			open: func() tea.Cmd {
				if !m.requireRW() {
					return nil
				}
				m.jobForm(j)
				return nil
			},
		})
	}
	return rows
}

func (m *model) jobsTitle() string {
	t := " " + sBold.Render(fmt.Sprintf("Recurring jobs %d", len(m.jobsRaw))) +
		sDim.Render("  schedules run by Longhorn (cron in UTC, NEXT in your time) · VOLUMES = volumes it applies to")
	switch {
	case m.jobsLoading:
		t += sDim.Render(" · loading …")
	case m.jobsErr != nil:
		t += "  " + sErr.Render(m.jobsErr.Error())
	case !m.jobsAt.IsZero():
		t += sDim.Render(fmt.Sprintf(" · loaded %s ago", format.Age(m.jobsAt.Format(time.RFC3339), time.Now())))
	}
	if m.rw {
		t += sDim.Render(" · n new · enter edit · X delete")
	} else {
		t += sDim.Render(" · W: write mode to change")
	}
	return t
}

// jobForm opens the create (j == nil) or edit form.
func (m *model) jobForm(j *longhorn.RecurringJob) {
	s := longhorn.JobSpec{Task: "backup", Cron: "0 2 * * *", Retain: 7, Concurrency: 1, Groups: []string{"default"}}
	title := "New recurring job"
	if j != nil {
		s = longhorn.SpecOf(j)
		title = "Edit recurring job " + j.Name
	}
	f := &form{title: title, fields: []formField{
		{key: "name", label: "name", value: s.Name, readOnly: j != nil, hint: "a-z 0-9 -"},
		{key: "task", label: "task", value: s.Task, options: longhorn.JobTasks, readOnly: j != nil},
		{key: "cron", label: "cron", value: s.Cron, hint: "min hour day month weekday, UTC"},
		{key: "retain", label: "retain", value: fmt.Sprint(s.Retain), hint: "kept per volume (0 for cleanup/trim)"},
		{key: "concurrency", label: "concurrency", value: fmt.Sprint(s.Concurrency), hint: "volumes at once"},
		{key: "groups", label: "groups", value: strings.Join(s.Groups, ","), hint: "comma separated; default = volumes without own jobs"},
		{key: "params", label: "parameters", value: kvString(s.Parameters), hint: "e.g. full-backup-interval=7"},
		{key: "labels", label: "labels", value: kvString(s.Labels), hint: "on created snapshots/backups, k=v,…"},
	}}
	f.live = func(v map[string]string) []string {
		now := time.Now()
		if _, err := longhorn.ParseCron(v["cron"]); err != nil {
			return []string{"✖ " + err.Error()}
		}
		return []string{"next runs: " + longhorn.FormatRuns(longhorn.NextRuns(v["cron"], now, 3), now) + " " + now.Format("MST")}
	}
	f.submit = func(v map[string]string) (*longhorn.Action, error) {
		ns, err := specFromForm(v)
		if err != nil {
			return nil, err
		}
		if j == nil {
			return longhorn.CreateJob(ns, m.jobsRaw)
		}
		return longhorn.UpdateJob(j, ns, m.state.Volumes)
	}
	m.openForm(f)
}

func specFromForm(v map[string]string) (longhorn.JobSpec, error) {
	s := longhorn.JobSpec{Name: strings.TrimSpace(v["name"]), Task: v["task"], Cron: strings.TrimSpace(v["cron"])}
	var err error
	if s.Retain, err = strconv.Atoi(strings.TrimSpace(v["retain"])); err != nil {
		return s, fmt.Errorf("retain: a number")
	}
	if s.Concurrency, err = strconv.Atoi(strings.TrimSpace(v["concurrency"])); err != nil {
		return s, fmt.Errorf("concurrency: a number")
	}
	for _, g := range strings.Split(v["groups"], ",") {
		if g = strings.TrimSpace(g); g != "" {
			s.Groups = append(s.Groups, g)
		}
	}
	if s.Parameters, err = parseKV(v["params"]); err != nil {
		return s, fmt.Errorf("parameters: %w", err)
	}
	if s.Labels, err = parseKV(v["labels"]); err != nil {
		return s, fmt.Errorf("labels: %w", err)
	}
	return s, nil
}

func kvString(m map[string]string) string {
	parts := make([]string, 0, len(m))
	for k, v := range m {
		parts = append(parts, k+"="+v)
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

func parseKV(s string) (map[string]string, error) {
	var out map[string]string
	for _, kv := range strings.Split(s, ",") {
		if kv = strings.TrimSpace(kv); kv == "" {
			continue
		}
		k, v, ok := strings.Cut(kv, "=")
		if !ok || strings.TrimSpace(k) == "" {
			return nil, fmt.Errorf("expected key=value, got %q", kv)
		}
		if out == nil {
			out = map[string]string{}
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out, nil
}

// jobsKey handles n / e / X on the jobs page.
func (m *model) jobsKey(p *page, key string) (tea.Cmd, bool) {
	if p.kind != pJobs {
		return nil, false
	}
	var cur *longhorn.RecurringJob
	if rows := m.rowsFor(p); p.cursor < len(rows) {
		cur, _ = rows[p.cursor].ref.(*longhorn.RecurringJob)
	}
	switch key {
	case "n":
		if m.requireRW() {
			m.jobForm(nil)
		}
		return nil, true
	case "e":
		if cur != nil && m.requireRW() {
			m.jobForm(cur)
		}
		return nil, true
	case "X":
		if cur != nil && m.requireRW() {
			m.confirm(longhorn.DeleteJob(cur, m.state.Volumes))
		}
		return nil, true
	case "ctrl+r":
		return m.loadJobs(), true
	}
	return nil, false
}
