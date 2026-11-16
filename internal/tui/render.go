package tui

import (
	"fmt"
	"strings"
	"time"

	"charm.land/lipgloss/v2"

	"github.com/nxckdx/longhorntx/internal/format"
	"github.com/nxckdx/longhorntx/internal/longhorn"
)

// ---------------------------------------------------------------- header / footer

func (m *model) viewHeader() string {
	l := contextStyle(m.ctxLabel()).Render(m.ctxLabel()) + " " + sBold.Render("lhtx")

	if m.store != nil {
		root := m.stack[0].kind
		deg, fault := 0, 0
		for _, v := range m.views {
			switch v.Health {
			case "degraded":
				deg++
			case "faulted":
				fault++
			}
		}
		vol := fmt.Sprintf("Volumes %d", len(m.views))
		if !m.synced(longhorn.ResVolumes) {
			vol = "Volumes …"
		}
		if fault > 0 {
			vol += fg(cErr, fmt.Sprintf(" ✖%d", fault))
		}
		if deg > 0 {
			vol += fg(cWarn, fmt.Sprintf(" ⚠%d", deg))
		}
		notReady := 0
		for _, n := range m.nodes {
			if !n.Ready {
				notReady++
			}
		}
		nodes := fmt.Sprintf("Nodes %d", len(m.nodes))
		if !m.synced(longhorn.ResNodes) {
			nodes = "Nodes …"
		}
		if notReady > 0 {
			nodes += fg(cErr, fmt.Sprintf(" ✖%d", notReady))
		}
		running := 0
		for _, a := range m.acts {
			if !a.Done {
				running++
			}
		}
		act := "Activity"
		if running > 0 {
			act += fg(cInfo, fmt.Sprintf(" ◐%d", running))
		}
		tabs := []struct {
			k     pageKind
			key   string
			label string
		}{{pVolumes, "1", vol}, {pNodes, "2", nodes}, {pActivity, "3", act}}
		for _, t := range tabs {
			if t.k == root {
				l += " " + sTabActive.Render(t.key+" "+t.label)
			} else {
				l += " " + sDim.Render(t.key) + " " + t.label
			}
		}
	}
	if m.lhVersion != "" {
		v := sDim.Render(" │ longhorn " + m.lhVersion)
		if m.versionErr != "" {
			v += " " + fg(cWarn, "("+m.versionErr+")")
		}
		l += v
	}

	r := ""
	if m.client != nil {
		r = m.loadingHint() + fmt.Sprintf("↓ %s  %s", format.Bytes(m.client.Cluster.Traffic.RX()), format.Rate(m.rxRate))
		if m.store != nil {
			if n := len(m.store.WatchedEngines()); n > 0 {
				r = sDim.Render(fmt.Sprintf("engines ⦿%d  ", n)) + r
			}
		}
	}
	gap := m.width - lipgloss.Width(l) - lipgloss.Width(r) - 1
	if gap < 1 {
		return fit(l, m.width)
	}
	return l + strings.Repeat(" ", gap) + r + " "
}

func (m *model) viewFooter() string {
	p := m.top()
	var keys [][2]string
	switch {
	case m.filtering:
		keys = [][2]string{{"enter", "apply"}, {"esc", "clear"}}
	case p.kind == pContexts:
		keys = [][2]string{{"↑↓", "select"}, {"enter", "switch"}, {"esc", "back"}}
	case p.kind == pHelp:
		keys = [][2]string{{"any key", "back"}}
	case p.kind == pDetail:
		keys = [][2]string{{"s", "snapshots"}, {"b", "backups"}, {"e", "events"}, {"J", "jobs"}, {"esc", "back"}}
		if p.panel != panelNone {
			keys = append(keys, [2]string{"ctrl+r", "reload"})
		}
	default:
		keys = [][2]string{{"enter", "open"}, {"/", "filter"}, {"1-3", "views"}, {"c", "context"}, {"?", "help"}}
		if len(m.stack) > 1 {
			keys = append(keys, [2]string{"esc", "back"})
		} else {
			keys = append(keys, [2]string{"q", "quit"})
		}
	}
	var b []string
	for _, k := range keys {
		b = append(b, sKey.Render("["+k[0]+"]")+" "+k[1])
	}
	s := " " + strings.Join(b, "  ")
	if m.store != nil {
		if err := m.store.Err(); err != nil {
			s += "  " + sErr.Render("⚠ "+err.Error())
		}
	}
	return fit(s, m.width)
}

// ---------------------------------------------------------------- volume detail

// detailTop renders title, info line and the replica boxes.
func (m *model) detailTop(p *page) string {
	v := m.findView(p.vol)
	if v == nil {
		return sDim.Render("\n  volume " + p.vol + " no longer exists")
	}
	var b strings.Builder

	title := " " + sBold.Render(v.Name)
	if v.PVC != "" {
		title += "  (" + v.PVC + ")"
	}
	title += "   " + format.Bytes(v.Size)
	st := fg(stateColor(v.State), v.State)
	if v.Node != "" {
		st += " → " + v.Node
	}
	title += "   " + st + "   "
	switch v.Health {
	case "healthy":
		title += fg(cOK, "✔ healthy")
	case "degraded":
		title += fg(cWarn, "⚠ degraded")
	case "faulted":
		title += fg(cErr, "✖ faulted")
	default:
		title += fg(cDim, v.Health)
	}
	b.WriteString(title + "\n")

	vol := v.Volume
	info := fmt.Sprintf(" actual %s · %d/%d replicas healthy · data engine %s · %s",
		format.Bytes(v.Actual), v.HealthyReplicas(), v.Desired, dash(v.Engine), strings.ToUpper(dash(vol.Spec.AccessMode)))
	if vol.Status.LastBackupAt != "" {
		info += " · last backup " + format.Age(vol.Status.LastBackupAt, time.Now()) + " ago"
	}
	b.WriteString(sDim.Render(info) + "\n")
	b.WriteString(sDim.Render(" "+strings.Repeat("─", max(m.width-2, 0))) + "\n")

	if len(v.Replicas) == 0 {
		b.WriteString(sDim.Render("  no replicas"))
		return b.String()
	}

	boxW := 30
	if m.width > 0 {
		perRow := max((m.width-1)/(boxW+1), 1)
		perRow = min(perRow, len(v.Replicas))
		boxW = min(max((m.width-1)/perRow-1, 24), 44)
	}
	contents := make([][]string, len(v.Replicas))
	for i, r := range v.Replicas {
		contents[i] = m.replicaLines(v, r, boxW)
	}
	perRow := max((m.width-1)/(boxW+1), 1)
	for i := 0; i < len(contents); i += perRow {
		end := min(i+perRow, len(contents))
		h := 0
		for _, c := range contents[i:end] {
			h = max(h, len(c))
		}
		row := make([]string, 0, 2*(end-i))
		for j, c := range contents[i:end] {
			for len(c) < h {
				c = append(c, "")
			}
			row = append(row, " ", box(v.Replicas[i+j].Node, c, boxW))
		}
		b.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, row...) + "\n")
	}
	if !v.EngineKnown {
		b.WriteString(sDim.Render(" waiting for engine data …") + "\n")
	}
	return strings.TrimSuffix(b.String(), "\n")
}

func (m *model) detailTopHeight() int {
	p := m.top()
	if p.kind != pDetail {
		return 0
	}
	return strings.Count(m.detailTop(p), "\n") + 1 + 2 // + blank + panel title
}

func (m *model) viewDetail(p *page) string {
	top := m.detailTop(p)
	var b strings.Builder
	b.WriteString(top + "\n\n")
	b.WriteString(m.panelTitle(p))
	if p.panel != panelNone && p.pdata != nil && !p.pdata.loading && p.pdata.err == nil {
		b.WriteString("\n" + m.renderList(p, m.panelCols(p), m.rowsFor(p), "nothing found"))
	}
	return b.String()
}

func (m *model) replicaLines(v *longhorn.VolumeView, r longhorn.ReplicaView, w int) []string {
	inner := w - 4
	var lines []string
	add := func(s string) { lines = append(lines, s) }

	// mount path (v1) or block device (v2); the disk name is only a fallback
	disk := r.Disk.Path
	if disk == "" {
		disk = dash(r.Disk.Name)
	}
	if r.Disk.Type == "block" {
		disk += sDim.Render(" (block)")
	}
	add(disk)

	var dot string
	switch r.Health {
	case longhorn.HealthHealthy:
		dot = fg(cOK, "● ") + fg(stateColor(r.State), r.State) + "   " + fg(cOK, "healthy")
	case longhorn.HealthRebuilding:
		dot = fg(cInfo, "◐ rebuilding")
	case longhorn.HealthFailed:
		dot = fg(cErr, "✖ failed")
	case longhorn.HealthStopped:
		dot = fg(cDim, "○ stopped")
	default:
		dot = fg(cDim, "? "+dash(r.State))
	}
	if r.Mode != "" {
		dot += sDim.Render("  " + r.Mode)
	}
	add(dot)

	if r.Rebuild != nil {
		key := "rebuild/" + r.Name
		pct := fmt.Sprintf(" %3d%%", r.Rebuild.Progress)
		add(bar(m.shownPct(key, r.Rebuild.Progress), inner-lipgloss.Width(pct), cInfo) + pct)
		if rt := m.rates.Get(key, r.Rebuild.Progress, v.Size); rt.Valid {
			add(sDim.Render(format.Rate(rt.BytesPerSec) + " · ~" + format.Duration(rt.ETA)))
		} else {
			add(sDim.Render("measuring speed …"))
		}
		if r.Rebuild.Error != "" {
			add(sErr.Render(r.Rebuild.Error))
		}
	} else if r.Health == longhorn.HealthRebuilding {
		add(sDim.Render("progress: waiting for engine"))
	} else if r.Replica.Spec.FailedAt != "" {
		add(sDim.Render("failed " + format.Age(r.Replica.Spec.FailedAt, time.Now()) + " ago"))
	}
	add(sDim.Render(shortReplica(r.Name)))
	return lines
}

// box draws a box with the title embedded in the top border, like the
// mockup: ┌─ node-1 ────┐
func box(title string, lines []string, w int) string {
	inner := w - 2
	t := "─ " + sBold.Render(title) + " "
	top := "┌" + t + strings.Repeat("─", max(inner-lipgloss.Width(t), 0)) + "┐"
	if lipgloss.Width(top) > w {
		top = "┌" + strings.Repeat("─", inner) + "┐"
	}
	out := []string{top}
	for _, l := range lines {
		out = append(out, "│ "+fit(l, inner-2)+" │")
	}
	out = append(out, "└"+strings.Repeat("─", inner)+"┘")
	return strings.Join(out, "\n")
}

func shortReplica(name string) string {
	if i := strings.LastIndex(name, "-r-"); i >= 0 {
		return "replica " + name[i+1:]
	}
	return name
}

// ---------------------------------------------------------------- contexts / help

func (m *model) viewContexts() string {
	var b strings.Builder
	b.WriteString("\n " + sBold.Render("Switch kubeconfig context") + "\n\n")
	for i, n := range m.ctxNames {
		mark := "  "
		if n == m.kctx {
			mark = fg(cOK, "● ")
		}
		line := mark + contextStyle(n).Render(n)
		if i == m.ctxCursor {
			line = sKey.Render("▶ ") + line
		} else {
			line = "  " + line
		}
		b.WriteString(" " + line + "\n")
	}
	return b.String()
}

func (m *model) viewHelp() string {
	return `
 lhtx – low-traffic Longhorn TUI (read-only)

 Views
   1  Volumes      2  Nodes → disks → replicas      3  Activity (rebuilds, backups, …)

 Navigation
   ↑/k ↓/j        move              g/G       top / bottom
   enter / l      open              esc / h   back
   /              filter            c         switch context
   q              back / quit       ?         this help

 Volume details
   s snapshots   b backups   e events   J recurring jobs   tab next panel
   ctrl+r reload panel (panels are loaded on demand, not watched)

 Traffic
   Volumes, replicas and nodes are watched permanently. Engines (chatty)
   are only watched for the volume shown in the detail view and for
   volumes that are rebuilding. Backups are watched "from now", without
   the initial list. The header shows bytes received on the wire.

 press any key`
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
