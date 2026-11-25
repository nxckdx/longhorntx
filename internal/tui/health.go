package tui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nxckdx/longhorntx/internal/format"
	"github.com/nxckdx/longhorntx/internal/longhorn"
	"github.com/nxckdx/longhorntx/internal/switchover"
)

// Health tab: longhorn.Diagnose over the live store data (free) plus extras
// that are loaded only when the tab is opened (targets, jobs, orphans,
// snapshots of rebuilding volumes).

type healthExtra struct {
	targets   []*longhorn.BackupTarget
	jobs      []*longhorn.RecurringJob
	orphans   int
	snapshots []*longhorn.VolumeSnapshot
	extra     []longhorn.Finding // active freezes, unfinished switches
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
	vols := m.state.Volumes
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
		x.extra = append(switchover.Checks(ctx, c, switchover.JournalDir()), c.EncryptionChecks(ctx, vols)...)
		x.extra = append(x.extra, c.HistoryChecks(ctx)...)
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

// lateBackupVolumes are the volumes the backup-age check complains about
// (never backed up, or older than their job expects).
func (m *model) lateBackupVolumes() []string {
	var out []string
	for _, f := range m.findings() {
		if f.Check != "backup-age" {
			continue
		}
		if vv := m.viewByObject(f.Object); vv != nil {
			out = append(out, vv.Name)
		}
	}
	return out
}

// computeFindings runs the checks on what is available right now.
func (m *model) computeFindings() []longhorn.Finding {
	if m.store == nil || !m.store.AllSynced() {
		return nil
	}
	in := longhorn.DoctorInput{State: m.state, Now: time.Now()}
	if x := m.health; x != nil {
		in.Targets, in.Jobs, in.Orphans, in.Snapshots, in.Extra = x.targets, x.jobs, x.orphans, x.snapshots, x.extra
	}
	if len(m.orphans) > 0 || in.Orphans == 0 {
		in.Orphans = len(m.orphans) // live from the store
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
	if n := len(m.lateBackupVolumes()); n > 0 {
		if m.rw {
			t += "  " + fg(cWarn, fmt.Sprintf("B: back up the %d volumes with late/missing backups", n))
		} else {
			t += sDim.Render(fmt.Sprintf(" · %d late backups (W, then B backs them up)", n))
		}
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

// ---------------------------------------------------------------- orphans tab (6)

func (m *model) orphanViews() []longhorn.OrphanView { return m.orphans }

func (m *model) orphanRows() []row {
	rows := make([]row, 0, len(m.orphans))
	for _, ov := range m.orphans {
		vol := sDim.Render("deleted")
		if ov.VolExists {
			vol = dash(ov.PVC)
			if ov.PVC == "" {
				vol = ov.Volume
			}
			if ov.Risk != "" {
				vol += " " + fg(cWarn, strings.TrimPrefix(ov.Risk, "volume "))
			}
		}
		clean := fg(cOK, "yes")
		switch {
		case ov.InUse:
			clean = sErr.Render("IN USE")
		case !ov.Cleanable:
			clean = fg(cWarn, "no")
		case ov.Risk != "":
			clean = sErr.Render("RISKY")
		}
		r := row{
			cells: []string{ov.Node, ov.Type, dash(ov.DiskPath), sDim.Render(dash(ov.DataName)), vol, format.Age(ov.Created, time.Now()), clean},
			ref:   ov,
		}
		if ov.VolExists {
			name := ov.Volume
			r.open = func() tea.Cmd { return m.openDetail(name) }
		}
		rows = append(rows, r)
	}
	return rows
}

func (m *model) orphansTitle() string {
	cleanable := 0
	for _, ov := range m.orphans {
		if ov.BulkDeletable() {
			cleanable++
		}
	}
	t := " " + sBold.Render(fmt.Sprintf("Orphans %d", len(m.orphans))) +
		sDim.Render(fmt.Sprintf("  data/instances no replica or engine owns · %d safe to clean", cleanable))
	if m.rw {
		t += sDim.Render(" · X delete · ctrl+x delete all safe ones (RISKY: volume deleted/faulted/degraded)")
	} else {
		t += sDim.Render(" · W: write mode to delete")
	}
	return t
}
