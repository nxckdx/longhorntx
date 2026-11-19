package tui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nxckdx/longhorntx/internal/format"
	"github.com/nxckdx/longhorntx/internal/longhorn"
)

// Health tab: longhorn.Diagnose over the live store data (free) plus extras
// that are loaded only when the tab is opened (targets, jobs, orphans,
// snapshots of rebuilding volumes).

type healthExtra struct {
	targets   []*longhorn.BackupTarget
	jobs      []*longhorn.RecurringJob
	orphans   int
	snapshots []*longhorn.VolumeSnapshot
	loadedAt  time.Time
	err       error
}

type healthMsg struct {
	client *longhorn.Client
	extra  *healthExtra
}

func (m *model) loadHealth() tea.Cmd {
	if m.client == nil {
		return nil
	}
	c, ctx := m.client, m.ctx
	rebuilding := longhorn.RebuildingVolumes(m.state.Replicas)
	m.healthLoading = true
	return func() tea.Msg {
		x := &healthExtra{loadedAt: time.Now()}
		var err error
		if x.targets, err = c.BackupTargets(ctx); err != nil {
			x.err = err
		}
		if x.jobs, err = c.RecurringJobs(ctx); err != nil && x.err == nil {
			x.err = err
		}
		x.orphans, _, _ = c.ListCount(ctx, "orphans")
		for _, v := range rebuilding {
			if s, err := c.Snapshots(ctx, v); err == nil {
				x.snapshots = append(x.snapshots, s...)
			}
		}
		return healthMsg{client: c, extra: x}
	}
}

// findings returns the cached result of the checks (recomputed in refresh,
// not on every frame).
func (m *model) findings() []longhorn.Finding { return m.fcache }

// computeFindings runs the checks on what is available right now.
func (m *model) computeFindings() []longhorn.Finding {
	if m.store == nil || !m.store.AllSynced() {
		return nil
	}
	in := longhorn.DoctorInput{State: m.state, Now: time.Now()}
	if x := m.health; x != nil {
		in.Targets, in.Jobs, in.Orphans, in.Snapshots = x.targets, x.jobs, x.orphans, x.snapshots
	}
	return longhorn.Diagnose(in)
}

func sevCell(s longhorn.Severity) string {
	switch s {
	case longhorn.SevCritical:
		return sErr.Render("✖ critical")
	case longhorn.SevWarning:
		return fg(cWarn, "⚠ warning")
	}
	return sDim.Render("ℹ info")
}

func (m *model) healthRows() []row {
	fs := m.findings()
	rows := make([]row, 0, len(fs))
	for _, f := range fs {
		msg := f.Message
		if f.Hint != "" {
			msg += sDim.Render("  → " + f.Hint)
		}
		r := row{cells: []string{sevCell(f.Severity), f.Check, f.Object, msg}}
		if vv := m.viewByObject(f.Object); vv != nil {
			name := vv.Name
			r.open = func() tea.Cmd { return m.openDetail(name) }
			r.ref = volRef(name)
		}
		rows = append(rows, r)
	}
	return rows
}

// viewByObject maps a finding object (namespace/pvc or volume name) back to
// its volume.
func (m *model) viewByObject(obj string) *longhorn.VolumeView {
	for i := range m.views {
		if m.views[i].PVC == obj || m.views[i].Name == obj {
			return &m.views[i]
		}
	}
	return nil
}

func (m *model) healthTitle() string {
	t := " " + sBold.Render("Health") + sDim.Render("  live checks on volumes, replicas, nodes, disks")
	switch x := m.health; {
	case m.healthLoading:
		t += sDim.Render(" · loading backup targets, jobs, orphans …")
	case x == nil:
		t += sDim.Render(" · ctrl+r: also check backups and orphans")
	case x.err != nil:
		t += "  " + sErr.Render(x.err.Error())
	default:
		t += sDim.Render(fmt.Sprintf(" · backups/orphans checked %s ago · ctrl+r reload", format.Age(x.loadedAt.Format(time.RFC3339), time.Now())))
	}
	return t
}

// healthBadge is the tab label: number of warnings/critical findings.
func (m *model) healthBadge() string {
	var crit, warn int
	for _, f := range m.findings() {
		switch f.Severity {
		case longhorn.SevCritical:
			crit++
		case longhorn.SevWarning:
			warn++
		}
	}
	var b []string
	if crit > 0 {
		b = append(b, fg(cErr, fmt.Sprintf("✖%d", crit)))
	}
	if warn > 0 {
		b = append(b, fg(cWarn, fmt.Sprintf("⚠%d", warn)))
	}
	if len(b) == 0 {
		return "Health"
	}
	return "Health " + strings.Join(b, " ")
}

func (m *model) computeBackupLate() map[string]bool {
	if m.health == nil {
		return nil
	}
	out := map[string]bool{}
	for _, b := range longhorn.BackupOverview(m.state.Volumes, m.health.jobs, time.Now(), 0) {
		if b.Status == "late" || b.Status == "never" {
			out[b.Volume] = true
		}
	}
	return out
}

func (m *model) drainRows(p *page) []row {
	fs := longhorn.DrainCheck(m.state, p.node)
	rows := make([]row, 0, len(fs))
	for _, f := range fs {
		msg := f.Message
		if f.Hint != "" {
			msg += sDim.Render("  → " + f.Hint)
		}
		r := row{cells: []string{sevCell(f.Severity), f.Check, f.Object, msg}}
		if vv := m.viewByObject(f.Object); vv != nil {
			name := vv.Name
			r.open = func() tea.Cmd { return m.openDetail(name) }
			r.ref = volRef(name)
		}
		rows = append(rows, r)
	}
	return rows
}

func (m *model) drainTitle(p *page) string {
	var crit, warn int
	for _, f := range longhorn.DrainCheck(m.state, p.node) {
		switch f.Severity {
		case longhorn.SevCritical:
			crit++
		case longhorn.SevWarning:
			warn++
		}
	}
	verdict := fg(cOK, "✔ safe to take down")
	switch {
	case crit > 0:
		verdict = sErr.Render(fmt.Sprintf("✖ %d volume(s) become unavailable", crit))
	case warn > 0:
		verdict = fg(cWarn, fmt.Sprintf("⚠ %d warning(s)", warn))
	}
	return " " + sBold.Render("Drain check "+p.node) + sDim.Render("  what happens if this node goes away · ") + verdict
}
