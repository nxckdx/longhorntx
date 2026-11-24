package tui

import (
	"fmt"
	"image/color"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nxckdx/longhorntx/internal/format"
	"github.com/nxckdx/longhorntx/internal/fuzzy"
	"github.com/nxckdx/longhorntx/internal/longhorn"
)

// row is one line of a list page.
type row struct {
	cells  []string
	header bool // not selectable (group heading)
	open   func() tea.Cmd
	ref    any // object behind the row, for actions
}

// rowRef types used by actions
type (
	volRef     string // volume name
	replicaRef string // replica name
	nodeRef    struct{ node, disk string }
)

type col struct {
	title string
	w     int
	flex  int // share of remaining width
}

func layout(cols []col, total int) []col {
	out := append([]col(nil), cols...)
	fixed, flex := 0, 0
	for _, c := range out {
		fixed += c.w + 1
		flex += c.flex
	}
	rest := max(total-fixed, 10)
	for i := range out {
		if out[i].flex > 0 {
			out[i].w = rest * out[i].flex / flex
		}
	}
	return out
}

// rowsFor returns the (filtered) rows of a list page.
func (m *model) rowsFor(p *page) []row {
	var rows []row
	switch p.kind {
	case pVolumes:
		rows = m.volumeRows()
	case pNodes:
		rows = m.nodeRows()
	case pDisk:
		rows = m.diskRows(p)
	case pActivity:
		rows = m.activityRows()
	case pDetail:
		rows = m.panelRows(p)
	case pSettings:
		rows = m.settingsRows()
	case pHealth:
		rows = m.healthRows()
	case pDrain:
		rows = m.drainRows(p)
	case pOrphans:
		rows = m.orphanRows()
	case pMatrix:
		rows = m.matrixRows()
	case pJobs:
		rows = m.jobRows()
	case pTargets:
		rows = m.targetRows()
	case pPrefs:
		rows = m.prefRows()
	case pBackupVols:
		rows = m.backupVolRows()
	case pBackupList:
		rows = m.panelRows(p)
	default:
		return nil
	}
	pat := fuzzy.Parse(m.filter)
	if pat.Empty() {
		return rows
	}
	// fzf-style: every term must match within one column; best matches first
	type scored struct {
		r     row
		score int
	}
	var hits []scored
	fields := make([]string, 0, 8)
	for _, r := range rows {
		if r.header {
			continue
		}
		fields = fields[:0]
		for _, c := range r.cells {
			fields = append(fields, ansi.Strip(c))
		}
		if s, ok := pat.Match(fields); ok {
			hits = append(hits, scored{r, s})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	out := make([]row, len(hits))
	for i := range hits {
		out[i] = hits[i].r
	}
	return out
}

func (m *model) colsFor(p *page) []col {
	switch p.kind {
	case pVolumes:
		return []col{{"", 1, 0}, {"PVC", 0, 3}, {"STATE", 9, 0}, {"ROBUSTNESS", 10, 0}, {"REPL", 5, 0},
			{"SIZE", 9, 0}, {"ACTUAL", 9, 0}, {"NODE", 12, 0}, {"ACTIVITY", 21, 0}, {"BACKUP", 7, 0}, {"VOLUME", 0, 2}}
	case pNodes:
		return []col{{"NODE / DISK", 0, 1}, {"STATUS", 14, 0}, {"USED", 25, 0}, {"SIZE", 10, 0},
			{"SCHEDULED", 25, 0}, {"REPLICAS", 8, 0}, {"SMART", 22, 0}}
	case pDisk:
		cols := []col{{"PVC", 0, 3}, {"REPLICA", 10, 0}, {"SIZE", 10, 0}, {"HEALTH", 13, 0}, {"VOLUME", 0, 2}}
		if p.disk == "" { // several disks: show which one
			cols = append(cols, col{"PATH", 0, 1})
		}
		return cols
	case pActivity:
		return []col{{"KIND", 8, 0}, {"PVC", 0, 2}, {"TARGET", 0, 1}, {"NODE", 10, 0},
			{"PROGRESS", 24, 0}, {"SPEED", 12, 0}, {"ETA", 9, 0}, {"STATE", 12, 0}}
	case pDetail:
		return m.panelCols(p)
	case pSettings:
		return []col{{"NAME", 0, 2}, {"VALUE", 0, 3}}
	case pBackupVols:
		return []col{{"PVC", 0, 3}, {"TARGET", 10, 0}, {"LAST BACKUP", 12, 0}, {"SIZE", 10, 0}, {"STORED", 10, 0}, {"VOLUME", 0, 2}}
	case pBackupList:
		return m.panelCols(p)
	case pMatrix:
		return m.matrixCols()
	case pPrefs:
		return []col{{"SETTING", 15, 0}, {"VALUE", 0, 1}, {"FROM", 0, 2}, {"", 0, 2}}
	case pTargets:
		return []col{{"NAME", 0, 1}, {"URL", 0, 3}, {"STATUS", 13, 0}, {"SECRET", 0, 1}, {"POLL", 7, 0}, {"SYNCED", 7, 0},
			{"VOLUMES", 7, 0}, {"BY", 6, 0}, {"MESSAGE", 0, 3}}
	case pJobs:
		return []col{{"NAME", 0, 2}, {"TASK", 20, 0}, {"CRON", 0, 1}, {"NEXT", 16, 0}, {"RETAIN", 6, 0}, {"CONC", 5, 0},
			{"GROUPS", 0, 1}, {"VOLUMES", 7, 0}, {"RUNS", 6, 0}}
	case pOrphans:
		return []col{{"NODE", 12, 0}, {"TYPE", 9, 0}, {"DISK", 0, 1}, {"DATA DIR", 0, 2}, {"VOLUME", 0, 2}, {"AGE", 6, 0}, {"CLEANABLE", 9, 0}}
	case pHealth, pDrain:
		return []col{{"SEVERITY", 11, 0}, {"CHECK", 15, 0}, {"OBJECT", 0, 2}, {"MESSAGE", 0, 5}}
	}
	return nil
}

// viewPage renders a list page (with optional title lines).
func (m *model) viewPage(p *page) string {
	var b strings.Builder
	switch p.kind {
	case pDisk:
		b.WriteString(m.diskTitle(p) + "\n")
		b.WriteString(sDim.Render(" "+strings.Repeat("─", max(m.width-2, 0))) + "\n")
	case pSettings:
		t := " " + sBold.Render("Settings") + sDim.Render("  loaded on demand · ctrl+r reload")
		if m.rw {
			t += sDim.Render(" · enter: change value")
		} else {
			t += sDim.Render(" · W: write mode to change values")
		}
		switch {
		case m.settingsLoading:
			t += sDim.Render(" · loading …")
		case m.settingsErr != nil:
			t += "  " + sErr.Render(m.settingsErr.Error())
		}
		b.WriteString(t + "\n")
		b.WriteString(sDim.Render(" "+strings.Repeat("─", max(m.width-2, 0))) + "\n")
	case pPrefs:
		b.WriteString(m.prefsTitle() + "\n")
		b.WriteString(sDim.Render(" "+strings.Repeat("─", max(m.width-2, 0))) + "\n")
	case pTargets:
		b.WriteString(m.targetsTitle() + "\n")
		b.WriteString(sDim.Render(" "+strings.Repeat("─", max(m.width-2, 0))) + "\n")
	case pJobs:
		b.WriteString(m.jobsTitle() + "\n")
		b.WriteString(sDim.Render(" "+strings.Repeat("─", max(m.width-2, 0))) + "\n")
	case pMatrix:
		b.WriteString(m.matrixTitle() + "\n")
		b.WriteString(sDim.Render(" "+strings.Repeat("─", max(m.width-2, 0))) + "\n")
	case pBackupVols:
		b.WriteString(m.backupVolsTitle() + "\n")
		b.WriteString(sDim.Render(" "+strings.Repeat("─", max(m.width-2, 0))) + "\n")
	case pBackupList:
		b.WriteString(m.backupListTitle(p) + "\n")
		b.WriteString(sDim.Render(" "+strings.Repeat("─", max(m.width-2, 0))) + "\n")
	case pOrphans:
		b.WriteString(m.orphansTitle() + "\n")
		b.WriteString(sDim.Render(" "+strings.Repeat("─", max(m.width-2, 0))) + "\n")
	case pDrain:
		b.WriteString(m.drainTitle(p) + "\n")
		b.WriteString(sDim.Render(" "+strings.Repeat("─", max(m.width-2, 0))) + "\n")
	case pHealth:
		b.WriteString(m.healthTitle() + "\n")
		b.WriteString(sDim.Render(" "+strings.Repeat("─", max(m.width-2, 0))) + "\n")
	case pActivity:
		b.WriteString(" " + sBold.Render("Activity") + sDim.Render("  rebuilds, purges, restores, clones · backups since lhtx started (finished ones stay 2 min)") + "\n")
		b.WriteString(sDim.Render(" "+strings.Repeat("─", max(m.width-2, 0))) + "\n")
	}
	empty := map[pageKind]string{
		pVolumes:    "no volumes",
		pNodes:      "no nodes",
		pDisk:       "no replicas on this disk",
		pActivity:   "nothing running – rebuilds, purges and backups appear here live",
		pSettings:   "no settings loaded",
		pHealth:     "✔ nothing to report",
		pDrain:      "✔ no volume is affected",
		pOrphans:    "✔ no orphans",
		pBackupVols: "no backup volumes (no backups in the backup store)",
		pBackupList: "no backups",
		pJobs:       "no recurring jobs",
		pTargets:    "no backup targets",
	}[p.kind]
	b.WriteString(m.renderList(p, m.colsFor(p), m.rowsFor(p), empty))
	return b.String()
}

func (m *model) renderList(p *page, cols []col, rows []row, empty string) string {
	var b strings.Builder
	cols = layout(cols, m.width-1)
	if m.filtering || m.filter != "" {
		cur := ""
		if m.filtering {
			cur = "█"
		}
		b.WriteString(" " + sKey.Render("/") + m.filter + cur + sDim.Render(fmt.Sprintf("  %d matches", len(rows))))
		if m.filtering {
			b.WriteString(sDim.Render("   fuzzy · space = and · 'exact · !not · ^start · end$"))
		}
		b.WriteString("\n")
	}
	var hdr []string
	for _, c := range cols {
		hdr = append(hdr, fit(c.title, c.w))
	}
	b.WriteString(" " + sHead.Render(strings.Join(hdr, " ")) + "\n")
	if len(rows) == 0 {
		b.WriteString(sDim.Render("  " + empty))
		return b.String()
	}
	end := min(p.offset+m.listHeight(), len(rows))
	for i := p.offset; i < end; i++ {
		r := rows[i]
		var line []string
		for j, c := range cols {
			cell := ""
			if j < len(r.cells) {
				cell = r.cells[j]
			}
			line = append(line, fit(cell, c.w))
		}
		s := " " + strings.Join(line, " ")
		if i == p.cursor && !r.header {
			s = sSelected.Render(fit(ansi.Strip(s), m.width))
		}
		b.WriteString(s + "\n")
	}
	if len(rows) > end-p.offset {
		b.WriteString(sDim.Render(fmt.Sprintf("  %d/%d", p.cursor+1, len(rows))))
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// ---------------------------------------------------------------- volumes

// volumeOrder returns the indexes of m.views in display order: as built
// (by PVC), or with problemsFirst faulted → degraded → active → rest.
func (m *model) volumeOrder() []int {
	idx := make([]int, len(m.views))
	for i := range idx {
		idx[i] = i
	}
	if !m.problemsFirst {
		return idx
	}
	active := map[string]bool{}
	for _, a := range m.acts {
		if !a.Done {
			active[a.Volume] = true
		}
	}
	rank := func(v *longhorn.VolumeView) int {
		switch {
		case v.Health == "faulted":
			return 0
		case v.Health == "degraded":
			return 1
		case active[v.Name] || v.Rebuilding():
			return 2
		}
		return 3
	}
	sort.SliceStable(idx, func(a, b int) bool { return rank(&m.views[idx[a]]) < rank(&m.views[idx[b]]) })
	return idx
}

func (m *model) volumeRows() []row {
	haveReplicas := m.synced(longhorn.ResReplicas)
	rows := make([]row, 0, len(m.views))
	for _, i := range m.volumeOrder() {
		v := &m.views[i]
		healthy := v.HealthyReplicas()
		replC := cOK
		if healthy < v.Desired {
			replC = cWarn
		}
		if healthy == 0 {
			replC = cErr
		}
		name := v.Name
		rows = append(rows, row{
			cells: []string{
				m.markCell(v.Name),
				dash(v.PVC),
				fg(stateColor(v.State), v.State),
				fg(robustnessColor(v.Health), v.Health),
				replCell(haveReplicas, replC, healthy, v.Desired),
				format.Bytes(v.Size),
				format.Bytes(v.Actual),
				dash(v.Node),
				m.volumeActivity(v),
				m.backupCell(v),
				sDim.Render(v.Name),
			},
			open: func() tea.Cmd { return m.openDetail(name) },
			ref:  volRef(name),
		})
	}
	return rows
}

func replCell(have bool, c color.Color, healthy, desired int) string {
	if !have {
		return sDim.Render(fmt.Sprintf("…/%d", desired))
	}
	return fg(c, fmt.Sprintf("%d/%d", healthy, desired))
}

func (m *model) volumeActivity(v *longhorn.VolumeView) string {
	for _, a := range m.acts {
		if a.Volume != v.Name || a.Done {
			continue
		}
		if a.Progress >= 0 {
			// small animated bar, same smoothing as the activity view
			return fg(cInfo, a.Kind+" ") + bar(m.shownPct(a.Key(), a.Progress), 8, cInfo) + fmt.Sprintf(" %3d%%", a.Progress)
		}
		return fg(cInfo, "◐ "+a.Kind) + sDim.Render(" (no progress)")
	}
	return ""
}

// ---------------------------------------------------------------- nodes

func (m *model) nodeRows() []row {
	var rows []row
	for i := range m.nodes {
		n := &m.nodes[i]
		status := fg(cOK, "Ready")
		if !n.Ready {
			status = fg(cErr, "NotReady")
		}
		if !n.Schedulable {
			status += fg(cWarn, " NoSched")
		}
		if n.Eviction {
			status += fg(cWarn, " Evict")
		}
		nreps := 0
		for _, d := range n.Disks {
			nreps += len(d.Replicas)
		}
		name := n.Name
		nrepCell := fmt.Sprint(nreps)
		if !m.synced(longhorn.ResReplicas) {
			nrepCell = sDim.Render("…")
		}
		rows = append(rows, row{
			cells: []string{sBold.Render(n.Name), status, "", "", "", nrepCell, ""},
			open:  func() tea.Cmd { m.push(&page{kind: pDisk, node: name}); return nil },
			ref:   nodeRef{node: name},
		})
		for j := range n.Disks {
			d := &n.Disks[j]
			branch := "├ "
			if j == len(n.Disks)-1 {
				branch = "└ "
			}
			dstatus := ""
			switch {
			case !d.Ready:
				dstatus = fg(cErr, "NotReady")
			case !d.AllowScheduling || !d.Schedulable:
				dstatus = fg(cWarn, "NoSched")
			}
			if d.Eviction {
				dstatus += fg(cWarn, " Evict")
			}
			label := d.Path
			if d.Type == "block" {
				label += sDim.Render(" (block)")
			}
			uuid := d.UUID
			rows = append(rows, row{
				cells: []string{
					sDim.Render(branch) + label,
					dstatus,
					usageBar(d.Used(), d.Maximum, 8),
					format.Bytes(d.Maximum),
					usageBar(d.Scheduled, d.Maximum-d.Reserved, 8),
					nrepCellDisk(m.synced(longhorn.ResReplicas), len(d.Replicas)),
					smartCell(d.SMART),
				},
				open: func() tea.Cmd { m.push(&page{kind: pDisk, node: name, disk: uuid}); return nil },
				ref:  nodeRef{node: name, disk: d.Name},
			})
		}
	}
	return rows
}

func usageBar(used, total int64, w int) string {
	if total <= 0 {
		return format.Bytes(used)
	}
	pct := float64(used) * 100 / float64(total)
	c := cOK
	switch {
	case pct >= 90:
		c = cErr
	case pct >= 75:
		c = cWarn
	}
	return bar(pct, w, c) + fmt.Sprintf(" %3.0f%% ", pct) + sDim.Render(format.Bytes(used))
}

func smartCell(s *longhorn.SMARTSummary) string {
	if s == nil {
		return sDim.Render("-")
	}
	var c color.Color = cOK
	if s.Status != "PASSED" && s.Status != "" {
		c = cErr
	}
	out := fg(c, dash(s.Status))
	if s.Temperature > 0 {
		out += fmt.Sprintf(" %d°C", s.Temperature)
	}
	if s.LifetimeLeft >= 0 {
		lc := cDim
		if s.LifetimeLeft < 20 {
			lc = cWarn
		}
		if s.LifetimeLeft < 10 {
			lc = cErr
		}
		out += fg(lc, fmt.Sprintf(" life %d%%", s.LifetimeLeft))
	}
	return out
}

// ---------------------------------------------------------------- disk (node → disk → replicas)

func (m *model) diskViews(p *page) []*longhorn.DiskView {
	var out []*longhorn.DiskView
	for i := range m.nodes {
		if m.nodes[i].Name != p.node {
			continue
		}
		for j := range m.nodes[i].Disks {
			d := &m.nodes[i].Disks[j]
			if p.disk == "" || d.UUID == p.disk {
				out = append(out, d)
			}
		}
	}
	return out
}

func (m *model) diskTitle(p *page) string {
	ds := m.diskViews(p)
	t := " " + sBold.Render(p.node)
	if len(ds) == 1 {
		d := ds[0]
		t += ":" + sBold.Render(d.Path) + sDim.Render("  ("+d.Name+", "+dash(d.FSType)+")") +
			"   " + usageBar(d.Used(), d.Maximum, 12) + sDim.Render(" of "+format.Bytes(d.Maximum)) +
			"   scheduled " + usageBar(d.Scheduled, d.Maximum-d.Reserved, 12)
		if d.SMART != nil {
			t += "   " + smartCell(d.SMART)
		}
	} else {
		t += sDim.Render(fmt.Sprintf("  all disks (%d)", len(ds)))
	}
	return t
}

func (m *model) diskRows(p *page) []row {
	var rows []row
	for _, d := range m.diskViews(p) {
		for _, r := range d.Replicas {
			vol := r.Volume
			rows = append(rows, row{
				cells: []string{
					dash(r.PVC),
					sDim.Render(strings.TrimPrefix(shortReplica(r.Replica), "replica ")),
					format.Bytes(r.Size),
					healthCell(r.Health),
					sDim.Render(r.Volume),
					sDim.Render(d.Path),
				},
				open: func() tea.Cmd { return m.openDetail(vol) },
				ref:  replicaRef(r.Replica),
			})
		}
	}
	return rows
}

func healthCell(h string) string {
	switch h {
	case longhorn.HealthHealthy:
		return fg(cOK, "● healthy")
	case longhorn.HealthRebuilding:
		return fg(cInfo, "◐ rebuilding")
	case longhorn.HealthFailed:
		return fg(cErr, "✖ failed")
	case longhorn.HealthStopped:
		return fg(cDim, "○ stopped")
	}
	return fg(cDim, "? "+h)
}

// ---------------------------------------------------------------- activity

func (m *model) activityRows() []row {
	rows := make([]row, 0, len(m.acts))
	for _, a := range m.acts {
		target := a.Target
		if i := strings.LastIndex(target, "-r-"); i >= 0 {
			target = "replica " + target[i+1:]
		}
		pvc := a.PVC
		if pvc == "" {
			pvc = a.Volume
		}
		prog := sDim.Render("waiting for engine")
		if a.Progress >= 0 {
			c := cInfo
			if a.Done {
				c = cOK
			}
			if a.Error != "" {
				c = cErr
			}
			prog = bar(m.shownPct(a.Key(), a.Progress), 16, c) + fmt.Sprintf(" %3d%%", a.Progress)
		}
		speed, eta := "", ""
		if !a.Done && a.Progress >= 0 {
			if r := m.rates.Get(a.Key(), a.Progress, a.Total); r.Valid {
				speed, eta = format.Rate(r.BytesPerSec), "~"+format.Duration(r.ETA)
			}
		}
		state := dash(a.State)
		if a.Error != "" {
			state = fg(cErr, a.Error)
		}
		vol := a.Volume
		rows = append(rows, row{
			cells: []string{fg(cInfo, a.Kind), pvc, sDim.Render(target), dash(a.Node), prog, speed, eta, state},
			open:  func() tea.Cmd { return m.openDetail(vol) },
			ref:   volRef(vol),
		})
	}
	return rows
}

func nrepCellDisk(have bool, n int) string {
	if !have {
		return sDim.Render("…")
	}
	return fmt.Sprint(n)
}

// backupCell shows the age of the last backup; red when the backup job's
// schedule says it is late (jobs are known once the Health tab was loaded).
func (m *model) backupCell(v *longhorn.VolumeView) string {
	last := v.Volume.Status.LastBackupAt
	if last == "" {
		return sDim.Render("-")
	}
	age := format.Age(last, time.Now())
	if m.backupLate[v.Name] {
		return fg(cErr, age)
	}
	return sDim.Render(age)
}

func (m *model) markCell(vol string) string {
	if m.marked[vol] {
		return sKey.Render("✓")
	}
	return " "
}
