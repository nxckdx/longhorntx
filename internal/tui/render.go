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
	l := contextStyle(m.ctxLabel()).Render(m.ctxLabel()) + m.rwBadge() + " " + sBold.Render("lhtx")

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
		}{{pVolumes, "1", vol}, {pNodes, "2", nodes}, {pActivity, "3", act}, {pSettings, "4", "Settings"}, {pHealth, "5", m.healthBadge()}, {pBackupVols, "6", "Backups"}}
		if len(m.orphans) > 0 {
			tabs = append(tabs, struct {
				k     pageKind
				key   string
				label string
			}{pOrphans, "7", fg(cWarn, fmt.Sprintf("Orphans %d", len(m.orphans)))})
		}
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
		r = m.loadingHint() + m.budgetHint() + m.refreshHint() + fmt.Sprintf("↓ %s  %s", format.Bytes(m.client.Cluster.Traffic.RX()), format.Rate(m.rxRate))
		if m.store != nil {
			if n := len(m.store.WatchedEngines()); n > 0 {
				r = sDim.Render(fmt.Sprintf("engines ⦿%d  ", n)) + r
			}
		}
	}
	if m.newVersion != "" {
		r = fg(cOK, "↑ "+m.newVersion+" (lhtx update)") + "  " + r
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
	case p.kind == pJobs && m.rw:
		keys = [][2]string{{"n", "new job"}, {"enter", "edit"}, {"X", "delete"}, {"ctrl+r", "reload"}, {"esc", "back"}, {"W", "write mode off"}}
	case p.kind == pJobs:
		keys = [][2]string{{"W", "write mode to change"}, {"ctrl+r", "reload"}, {"/", "filter"}, {"esc", "back"}}
	case p.kind == pTargets && m.rw:
		keys = [][2]string{{"n", "new"}, {"enter", "edit"}, {"s", "sync now"}, {"X", "delete"}, {"esc", "back"}, {"W", "write mode off"}}
	case p.kind == pTargets:
		keys = [][2]string{{"W", "write mode to change"}, {"ctrl+r", "reload"}, {"esc", "back"}}
	case p.kind == pBackupVols:
		keys = [][2]string{{"enter", "backups"}, {"r", "restore newest"}, {"J", "recurring jobs"}, {"t", "targets"}, {"/", "filter"}, {"ctrl+r", "reload"}, {"1-7", "views"}, {"?", "help"}}
	case p.kind == pThemes:
		keys = [][2]string{{"↑↓", "preview"}, {"enter", "keep"}, {"esc", "back"}}
	case p.kind == pContexts:
		keys = [][2]string{{"↑↓", "select"}, {"enter", "switch"}, {"o", "check all clusters"}, {"esc", "back"}}
	case p.kind == pHelp:
		keys = [][2]string{{"any key", "back"}}
	case m.modal != nil:
		keys = nil
	case p.kind == pDetail:
		keys = [][2]string{{"s", "snapshots"}, {"b", "backups"}, {"e", "events"}, {"J", "jobs"}, {"esc", "back"}}
		if p.panel != panelNone {
			keys = append(keys, [2]string{"ctrl+r", "reload"})
		}
		if m.rw {
			keys = append(keys, [2]string{"S", "snapshot"}, [2]string{"B", "backup"}, [2]string{"R", "replicas"}, [2]string{"U", "engine"}, [2]string{"E", "expand"}, [2]string{"A", "attach"}, [2]string{"[ ]", "select"}, [2]string{"X", "del replica"})
			if p.panel == panelSnapshots {
				keys = append(keys, [2]string{"D", "del snapshot"})
			}
		}
	case m.rw && p.kind == pNodes:
		keys = [][2]string{{"C", "scheduling on/off"}, {"E", "evict on/off"}, {"i", "drain check"}, {"m", "replica matrix"}, {"enter", "open"}, {"W", "write mode off"}}
	case p.kind == pOrphans && m.rw:
		keys = [][2]string{{"X", "delete"}, {"ctrl+x", "delete all cleanable"}, {"enter", "open volume"}, {"/", "filter"}, {"W", "write mode off"}}
	case p.kind == pOrphans:
		keys = [][2]string{{"enter", "open volume"}, {"/", "filter"}, {"W", "write mode"}, {"1-7", "views"}, {"?", "help"}}
	case p.kind == pNodes:
		keys = [][2]string{{"enter", "open"}, {"i", "drain check"}, {"m", "replica matrix"}, {"/", "filter"}, {"1-7", "views"}, {"c", "context"}, {"?", "help"}, {"q", "quit"}}
	case p.kind == pVolumes && len(m.marked) > 0:
		keys = [][2]string{{"space", "toggle"}, {"ctrl+a", "all/none"}, {"esc", "clear"}}
		if m.rw {
			keys = append(keys, [2]string{"S", "snapshot"}, [2]string{"B", "backup"}, [2]string{"R", "replicas"}, [2]string{"U", "engine image"}, [2]string{"E", "expand"}, [2]string{"A", "attach/detach"})
		} else {
			keys = append(keys, [2]string{"W", "write mode for actions"})
		}
		s := " " + sKey.Render(fmt.Sprintf("✓ %d selected", len(m.marked)))
		for _, k := range keys {
			s += "  " + sKey.Render("["+k[0]+"]") + " " + k[1]
		}
		return fit(m.flashPrefix()+s, m.width)
	case !m.rw && p.kind == pVolumes:
		sortLbl := "problems first"
		if m.problemsFirst {
			sortLbl = "sort by PVC"
		}
		keys = [][2]string{{"enter", "open"}, {"/", "filter"}, {"p", sortLbl}, {"1-7", "views"}, {"c", "context"}, {"?", "help"}, {"q", "quit"}}
	case m.rw && (p.kind == pVolumes || p.kind == pActivity):
		keys = [][2]string{{"S", "snapshot"}, {"B", "backup"}, {"R", "replicas"}, {"U", "engine"}, {"E", "expand"}, {"A", "attach"}, {"space", "select"}, {"enter", "open"}, {"/", "filter"}, {"W", "write mode off"}}
	case m.rw && p.kind == pDisk:
		keys = [][2]string{{"X", "delete replica"}, {"enter", "open"}, {"esc", "back"}, {"W", "write mode off"}}
	default:
		keys = [][2]string{{"enter", "open"}, {"/", "filter"}, {"1-7", "views"}, {"c", "context"}, {"?", "help"}}
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
	s := m.flashPrefix() + " " + strings.Join(b, "  ")
	if m.store != nil && !m.paused { // a paused store reports its cancelled watches
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
			title := v.Replicas[i+j].Node
			if i+j == p.sel && len(v.Replicas) > 1 {
				title = "▶ " + title
			}
			row = append(row, " ", box(title, c, boxW))
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
	t := "\n " + sBold.Render("Switch kubeconfig context")
	switch {
	case len(m.ovPending) > 0:
		t += sDim.Render(fmt.Sprintf("  checking %d clusters …", len(m.ovPending)))
	case !m.ovAt.IsZero():
		t += sDim.Render(fmt.Sprintf("  checked %s ago · o check again", time.Since(m.ovAt).Round(time.Second)))
	default:
		t += sDim.Render("  o: check Longhorn health in all clusters (one doctor per cluster)")
	}
	b.WriteString(t + "\n\n")
	for _, l := range m.contextLines() {
		b.WriteString(l + "\n")
	}
	return b.String()
}

func (m *model) viewHelp() string {
	return `
 lhtx – low-traffic Longhorn TUI (read-only)

 Views
   1  Volumes   2  Nodes → disks → replicas   3  Activity   4  Settings   5  Health
   6  Backups (backup volumes, also of deleted volumes; enter: their backups)
      J there: recurring jobs, t: backup targets (write mode: n new,
      enter edit, X delete; targets also s sync now)
      r restore (write mode): newest backup of the backup volume, or the
      selected one in a backup list – into a new volume, optionally PV/PVC
   7  Orphans (only shown when there are any)

 Navigation
   ↑/k ↓/j        move              g/G       top / bottom
   enter / l      open              esc / h   back
   /              fuzzy filter      c         switch context (o there: health
                                              of all clusters, like lhtx overview)
                  (fzf syntax: space = and, 'exact, !not, ^start, end$)
   p              volume list: problems & activity first (toggle)
   i              nodes: drain check (what happens if the node goes away)
   m              nodes: replica matrix (volumes × nodes, shared node/disk first)
   q              back / quit       ?         this help
   T              choose a theme (live preview; keep: theme: NAME in the config)

 Volume details
   s snapshots   b backups   e events   J recurring jobs   tab next panel
   ctrl+r reload panel (panels are loaded on demand, not watched)

 Write mode (off by default; W toggles, or start with --rw)
   S snapshot   B backup (snapshot + backup)   R replica count   U engine image
   E expand (20Gi or +5Gi; through the PVC if there is one)
   A attach to a node / detach (manual ticket, compatible with the web UI)
   space (recurring jobs panel) assign / unassign the job or group
   space select volumes, ctrl+a select all (filtered), esc clear – S/B/R/U/E
   then act on all selected volumes
   X delete replica ([ ] selects in volume details)   D delete snapshot (in panel)
   C scheduling on/off, E eviction on/off (nodes view)   enter edit (settings)
   X delete orphan, ctrl+x delete all cleanable orphans (orphans tab)
   Every action shows the request first: y apply (Y for destructive ones),
   d server dry run (validated by Longhorn, nothing changed), esc cancel.

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

// refreshHint shows whether volumes are live or polled.
func (m *model) refreshHint() string {
	if m.store == nil || m.paused || !m.synced(longhorn.ResVolumes) {
		return ""
	}
	s := "live"
	if iv := m.store.RefreshInterval(); iv > 0 {
		s = fmt.Sprintf("⟳ %s · %s ago", iv, time.Since(m.store.LastRefresh()).Round(time.Second))
		if r := m.store.AutoSwitched(); r > 0 {
			s += fmt.Sprintf(" (auto: %d updates/min)", r)
		}
	}
	if n := m.store.AutoEnginesSkipped(); n > 0 {
		s = fg(cWarn, fmt.Sprintf("%d rebuilds w/o progress", n)) + sDim.Render(" · "+s)
	}
	return sDim.Render(s + "  ")
}

// flashPrefix renders a recent status message for the footer.
func (m *model) flashPrefix() string {
	if m.flash == "" || time.Since(m.flashAt) >= 8*time.Second {
		return ""
	}
	c := cOK
	if m.flashErr {
		c = cErr
	}
	return " " + fg(c, m.flash) + "  " + sDim.Render("│")
}
