package tui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/nxckdx/longhorntx/internal/longhorn"
)

// Write mode: off by default, toggled with W (or started with --rw). Every
// action shows a preview and needs confirmation; destructive ones need a
// capital Y. 'd' in the dialog runs a server-side dry run instead.

type modalKind int

const (
	modalConfirm modalKind = iota
	modalPrompt
	modalBusy
)

type modal struct {
	kind   modalKind
	action *longhorn.Action

	// composite "snapshot + backup" (not a single request)
	backupVol *longhorn.Volume
	// several independent actions confirmed at once (e.g. orphans)
	batch []*longhorn.Action

	// prompt
	label    string
	input    string
	onSubmit func(string) (*longhorn.Action, error)

	busyText string
}

type actionDoneMsg struct {
	title string
	dry   bool
	err   error
}

type settingsMsg struct {
	client   *longhorn.Client
	settings []*longhorn.Setting
	err      error
}

func (m *model) setFlash(s string, isErr bool) {
	m.flash, m.flashErr, m.flashAt = s, isErr, time.Now()
}

// requireRW returns false (and explains) if write mode is off.
func (m *model) requireRW() bool {
	if m.rw {
		return true
	}
	m.setFlash("read-only – press W to enable write mode", true)
	return false
}

func (m *model) confirm(a *longhorn.Action, err error) {
	if err != nil {
		m.setFlash(err.Error(), true)
		return
	}
	m.modal = &modal{kind: modalConfirm, action: a}
}

func (m *model) prompt(label, initial string, submit func(string) (*longhorn.Action, error)) {
	m.modal = &modal{kind: modalPrompt, label: label, input: initial, onSubmit: submit}
}

// ---------------------------------------------------------------- keys

// actionKey handles write-related keys. Returns handled=false if the key
// is not an action key on this page.
func (m *model) actionKey(p *page, key string) (tea.Cmd, bool) {
	if key == "W" {
		m.rw = !m.rw
		if m.rw {
			m.setFlash("write mode ON – actions ask for confirmation", false)
		} else {
			m.setFlash("write mode off", false)
		}
		return nil, true
	}
	if p.kind == pOrphans && (key == "X" || key == "ctrl+x") {
		if !m.requireRW() {
			return nil, true
		}
		ovs := m.orphanViews()
		if key == "ctrl+x" {
			var acts []*longhorn.Action
			for _, ov := range ovs {
				if !ov.Cleanable || ov.InUse {
					continue
				}
				if a, err := longhorn.DeleteOrphan(ov); err == nil {
					acts = append(acts, a)
				}
			}
			m.confirmBatch(fmt.Sprintf("Delete %d cleanable orphans (data directories on the nodes)", len(acts)), acts)
			return nil, true
		}
		rows := m.rowsFor(p)
		if p.cursor < len(rows) {
			if ov, ok := rows[p.cursor].ref.(longhorn.OrphanView); ok {
				m.confirm(longhorn.DeleteOrphan(ov))
			}
		}
		return nil, true
	}
	if !strings.Contains("SBRXDCE", key) || len(key) != 1 {
		return nil, false
	}
	rows := m.rowsFor(p)
	var ref any
	if p.cursor >= 0 && p.cursor < len(rows) {
		ref = rows[p.cursor].ref
	}

	switch p.kind {
	case pVolumes, pActivity:
		name, ok := ref.(volRef)
		if !ok {
			return nil, false
		}
		return m.volumeAction(string(name), key, nil), true

	case pDetail:
		if key == "D" || (key == "B" && p.panel == panelSnapshots) {
			if s, ok := ref.(*longhorn.VolumeSnapshot); ok {
				if !m.requireRW() {
					return nil, true
				}
				if key == "D" {
					m.confirm(longhorn.DeleteSnapshot(s))
				} else if vv := m.findView(p.vol); vv != nil {
					m.confirm(longhorn.CreateBackup(vv.Volume, s.Name))
				}
				return nil, true
			}
		}
		return m.volumeAction(p.vol, key, p), true

	case pDisk:
		if name, ok := ref.(replicaRef); ok && key == "X" {
			if m.requireRW() {
				m.deleteReplica(string(name))
			}
			return nil, true
		}

	case pNodes:
		nr, ok := ref.(nodeRef)
		if !ok || (key != "C" && key != "E") {
			return nil, false
		}
		if !m.requireRW() {
			return nil, true
		}
		n := m.nodeCR(nr.node)
		if n == nil {
			return nil, true
		}
		if key == "C" {
			enabled := n.Spec.AllowScheduling
			if nr.disk != "" {
				enabled = n.Spec.Disks[nr.disk].AllowScheduling
			}
			m.confirm(longhorn.SetScheduling(n, nr.disk, !enabled))
		} else {
			evicting := n.Spec.EvictionRequested
			if nr.disk != "" {
				evicting = n.Spec.Disks[nr.disk].EvictionRequested
			}
			m.confirm(longhorn.SetEviction(n, nr.disk, !evicting))
		}
		return nil, true
	}
	return nil, false
}

// volumeAction: S snapshot, B backup, R replica count, X delete selected replica.
func (m *model) volumeAction(vol, key string, detail *page) tea.Cmd {
	vv := m.findView(vol)
	if vv == nil || !strings.Contains("SBRX", key) {
		return nil
	}
	if !m.requireRW() {
		return nil
	}
	v := vv.Volume
	switch key {
	case "S":
		m.confirm(longhorn.CreateSnapshot(v, ""))
	case "B":
		snapA, err := longhorn.CreateSnapshot(v, "")
		if err == nil {
			_, err = longhorn.CreateBackup(v, snapA.Name)
		}
		if err != nil {
			m.setFlash(err.Error(), true)
			return nil
		}
		m.modal = &modal{kind: modalConfirm, backupVol: v, action: &longhorn.Action{
			Title: fmt.Sprintf("Back up %s: create a snapshot, wait until ready, then back it up to %q", dash(vv.PVC), v.Spec.BackupTargetName),
			Verb:  longhorn.VerbCreate, GVR: longhorn.GVRSnapshot, Name: snapA.Name, Object: snapA.Object,
			Warnings: append(snapA.Warnings, "server dry run checks only the snapshot step"),
		}}
	case "R":
		m.prompt(fmt.Sprintf("Number of replicas for %s (now %d)", dash(vv.PVC), v.Spec.NumberOfReplicas),
			strconv.Itoa(v.Spec.NumberOfReplicas), func(s string) (*longhorn.Action, error) {
				n, err := strconv.Atoi(strings.TrimSpace(s))
				if err != nil {
					return nil, fmt.Errorf("not a number: %q", s)
				}
				return longhorn.SetReplicaCount(v, n)
			})
	case "X":
		if detail == nil || len(vv.Replicas) == 0 {
			m.setFlash("open the volume and select a replica with [ and ]", true)
			return nil
		}
		m.deleteReplica(vv.Replicas[min(detail.sel, len(vv.Replicas)-1)].Name)
	}
	return nil
}

func (m *model) deleteReplica(name string) {
	var r *longhorn.Replica
	var siblings []*longhorn.Replica
	for _, x := range m.state.Replicas {
		if x.Name == name {
			r = x
		}
	}
	if r == nil {
		m.setFlash("replica "+name+" not found", true)
		return
	}
	for _, x := range m.state.Replicas {
		if x.Spec.VolumeName == r.Spec.VolumeName {
			siblings = append(siblings, x)
		}
	}
	var vol *longhorn.Volume
	if vv := m.findView(r.Spec.VolumeName); vv != nil {
		vol = vv.Volume
	}
	m.confirm(longhorn.DeleteReplica(r, vol, siblings))
}

func (m *model) nodeCR(name string) *longhorn.Node {
	for _, n := range m.state.Nodes {
		if n.Name == name {
			return n
		}
	}
	return nil
}

// modalKey handles keys while a dialog is open.
func (m *model) modalKey(k tea.KeyPressMsg) tea.Cmd {
	md := m.modal
	key := k.String()
	switch md.kind {
	case modalBusy:
		return nil
	case modalPrompt:
		switch key {
		case "esc":
			m.modal = nil
		case "enter":
			a, err := md.onSubmit(md.input)
			m.modal = nil
			m.confirm(a, err)
		case "backspace":
			if r := []rune(md.input); len(r) > 0 {
				md.input = string(r[:len(r)-1])
			}
		case "ctrl+u":
			md.input = ""
		default:
			if k.Text != "" {
				md.input += k.Text
			}
		}
		return nil
	}
	// confirm
	a := md.action
	switch key {
	case "esc", "n", "q":
		m.modal = nil
		m.setFlash("cancelled", false)
	case "d":
		m.modal = &modal{kind: modalBusy, busyText: "server dry run …"}
		if md.batch != nil {
			return m.runBatch(md.batch, a.Title, longhorn.DryRunServer)
		}
		return m.runAction(a, longhorn.DryRunServer)
	case "y", "Y":
		if a.Destructive && key != "Y" {
			m.setFlash("destructive action: press capital Y to confirm", true)
			return nil
		}
		if md.backupVol != nil {
			v := md.backupVol
			m.modal = &modal{kind: modalBusy, busyText: "creating snapshot, waiting until ready, then starting the backup …"}
			c, ctx := m.client, m.ctx
			return func() tea.Msg {
				name, err := c.SnapshotAndBackup(ctx, v, func(string) {})
				return actionDoneMsg{title: "backup " + name + " started (see 3 Activity)", err: err}
			}
		}
		m.modal = &modal{kind: modalBusy, busyText: "applying …"}
		if md.batch != nil {
			return m.runBatch(md.batch, a.Title, longhorn.DryRunNone)
		}
		return m.runAction(a, longhorn.DryRunNone)
	}
	return nil
}

// confirmBatch shows one dialog for several actions. The summary action
// carries title, warnings and the destructive flag; its preview lists the
// individual requests.
func (m *model) confirmBatch(title string, acts []*longhorn.Action) {
	if len(acts) == 0 {
		m.setFlash("nothing to do", true)
		return
	}
	if len(acts) == 1 {
		m.modal = &modal{kind: modalConfirm, action: acts[0]}
		return
	}
	sum := &longhorn.Action{Title: title, Destructive: true, Verb: longhorn.VerbDelete, GVR: acts[0].GVR, Name: fmt.Sprintf("%d objects", len(acts))}
	seen := map[string]bool{}
	for _, a := range acts {
		for _, w := range a.Warnings {
			if !seen[w] {
				seen[w] = true
				sum.Warnings = append(sum.Warnings, w)
			}
		}
		sum.Lines = append(sum.Lines, a.Summary())
	}
	m.modal = &modal{kind: modalConfirm, action: sum, batch: acts}
}

func (m *model) runBatch(acts []*longhorn.Action, title string, mode longhorn.DryRun) tea.Cmd {
	c, ctx := m.client, m.ctx
	return func() tea.Msg {
		var failed []string
		for _, a := range acts {
			if err := c.Apply(ctx, a, mode); err != nil {
				failed = append(failed, a.Name+": "+err.Error())
			}
		}
		var err error
		if len(failed) > 0 {
			err = fmt.Errorf("%d of %d failed – %s", len(failed), len(acts), failed[0])
		}
		return actionDoneMsg{title: fmt.Sprintf("%s (%d)", title, len(acts)-len(failed)), dry: mode == longhorn.DryRunServer, err: err}
	}
}

func (m *model) runAction(a *longhorn.Action, mode longhorn.DryRun) tea.Cmd {
	c, ctx := m.client, m.ctx
	return func() tea.Msg {
		err := c.Apply(ctx, a, mode)
		return actionDoneMsg{title: a.Title, dry: mode == longhorn.DryRunServer, err: err}
	}
}

func (m *model) applyActionDone(msg actionDoneMsg) tea.Cmd {
	m.modal = nil
	switch {
	case msg.err != nil:
		m.setFlash("✖ "+msg.err.Error(), true)
	case msg.dry:
		m.setFlash("✔ server dry run accepted: "+msg.title+" (nothing changed)", false)
	default:
		m.setFlash("✔ "+msg.title, false)
	}
	// refresh on-demand data that the action may have changed
	p := m.top()
	if !msg.dry && msg.err == nil {
		if p.kind == pDetail && p.panel != panelNone {
			return m.loadPanel(p)
		}
		if p.kind == pSettings {
			return m.loadSettings()
		}
	}
	return nil
}

// ---------------------------------------------------------------- settings page

func (m *model) loadSettings() tea.Cmd {
	if m.client == nil {
		return nil
	}
	c, ctx := m.client, m.ctx
	m.settingsLoading = true
	return func() tea.Msg {
		s, err := c.Settings(ctx)
		return settingsMsg{client: c, settings: longhorn.SortedSettings(s), err: err}
	}
}

func (m *model) settingsRows() []row {
	rows := make([]row, 0, len(m.settings))
	for _, s := range m.settings {
		s := s
		rows = append(rows, row{
			cells: []string{s.Name, s.Value},
			ref:   s,
			open: func() tea.Cmd {
				if !m.requireRW() {
					return nil
				}
				m.prompt("New value for "+s.Name, s.Value, func(v string) (*longhorn.Action, error) {
					return longhorn.SetSetting(s, v)
				})
				return nil
			},
		})
	}
	return rows
}

// ---------------------------------------------------------------- rendering

func (m *model) viewModal(bodyHeight int) string {
	md := m.modal
	w := min(max(m.width-8, 40), 110)
	var b strings.Builder
	switch md.kind {
	case modalBusy:
		b.WriteString(fg(cInfo, spinner(time.Since(m.flashAt))+" "+md.busyText))
	case modalPrompt:
		b.WriteString(sBold.Render(md.label) + "\n\n")
		b.WriteString("> " + md.input + "█\n\n")
		b.WriteString(sDim.Render("[enter] continue  [esc] cancel  [ctrl+u] clear"))
	case modalConfirm:
		a := md.action
		title := sBold.Render(a.Title)
		if a.Destructive {
			title = sErr.Render("⚠ " + a.Title)
		}
		b.WriteString(title + "\n" + sDim.Render("context "+m.ctxLabel()) + "\n\n")
		preview := a.Preview(m.client.Namespace)
		lines := strings.Split(preview, "\n")
		maxLines := max(bodyHeight-12-len(a.Warnings), 4)
		if len(lines) > maxLines {
			lines = append(lines[:maxLines], "  …")
		}
		for _, l := range lines {
			b.WriteString(sDim.Render(fit(l, w-8)) + "\n")
		}
		if len(a.Warnings) > 0 {
			b.WriteString("\n")
			for _, wn := range a.Warnings {
				b.WriteString(fg(cWarn, "⚠ "+wn) + "\n")
			}
		}
		b.WriteString("\n")
		yes := sKey.Render("[y]") + " apply"
		if a.Destructive {
			yes = sErr.Render("[Y]") + " apply (capital Y)"
		}
		b.WriteString(yes + "   " + sKey.Render("[d]") + " server dry run   " + sKey.Render("[esc]") + " cancel")
	}
	border := lipgloss.Color("39")
	if md.kind == modalConfirm && md.action.Destructive {
		border = cErr
	}
	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(border).Padding(1, 2).Width(w).Render(b.String())
	return lipgloss.Place(m.width, bodyHeight, lipgloss.Center, lipgloss.Center, box)
}

func (m *model) rwBadge() string {
	if !m.rw {
		return ""
	}
	return " " + lipgloss.NewStyle().Background(cErr).Foreground(lipgloss.Color("231")).Bold(true).Padding(0, 1).Render("RW")
}
