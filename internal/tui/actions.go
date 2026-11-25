package tui

import (
	"fmt"
	"sort"
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
	modalPick
	modalForm
)

type pickOption struct {
	label, value string
	disabled     string // reason, empty if selectable
}

type modal struct {
	kind   modalKind
	action *longhorn.Action

	// composite "snapshot + backup" (not a single request), one or more volumes
	backupVols []*longhorn.Volume
	// several independent actions confirmed at once (e.g. orphans)
	batch []*longhorn.Action

	// prompt (onSubmit opens the next dialog itself)
	label    string
	input    string
	onSubmit func(string) error

	// picker
	options []pickOption
	pick    int
	onPick  func(string)

	busyText string

	form *form // modalForm

	// onConfirm replaces applying the action (multi-step runs like a switch)
	onConfirm func() tea.Cmd
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

func (m *model) prompt(label, initial string, submit func(string) error) {
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
	if cmd, ok := m.jobsKey(p, key); ok {
		return cmd, true
	}
	if cmd, ok := m.targetsKey(p, key); ok {
		return cmd, true
	}
	if cmd, ok := m.restoreKey(p, key); ok {
		return cmd, true
	}
	if key == "Z" && (p.kind == pVolumes || p.kind == pDetail) {
		return m.freezeKey(p), true
	}
	if p.kind == pDetail && key == "w" {
		return m.switchToVolumeKey(p.vol), true
	}
	if p.kind == pHealth && key == "B" {
		var vols, detached []string
		for _, n := range m.lateBackupVolumes() {
			if vv := m.findView(n); vv != nil && vv.State != "attached" {
				detached = append(detached, dash(vv.PVC))
				continue
			}
			vols = append(vols, n)
		}
		if len(vols) == 0 {
			msg := "no attached volume with a late or missing backup"
			if len(detached) > 0 {
				msg += fmt.Sprintf(" (%d detached: Longhorn snapshots only attached volumes)", len(detached))
			}
			m.setFlash(msg, len(detached) > 0)
			return nil, true
		}
		cmd := m.volumeAction(vols, "B", nil)
		if len(detached) > 0 && m.modal != nil && m.modal.action != nil {
			m.modal.action.Warnings = append(m.modal.action.Warnings,
				fmt.Sprintf("skipped %d detached volumes (%s): Longhorn snapshots only attached volumes – attach them or let the recurring job do it (setting allow-recurring-job-while-volume-detached)",
					len(detached), strings.Join(detached, ", ")))
		}
		return cmd, true
	}
	if p.kind == pOrphans && (key == "X" || key == "ctrl+x") {
		if !m.requireRW() {
			return nil, true
		}
		ovs := m.orphanViews()
		if key == "ctrl+x" {
			var acts []*longhorn.Action
			risky := 0
			for _, ov := range ovs {
				if ov.Cleanable && !ov.InUse && ov.Risk != "" {
					risky++
				}
				if !ov.BulkDeletable() {
					continue
				}
				if a, err := longhorn.DeleteOrphan(ov); err == nil {
					acts = append(acts, a)
				}
			}
			if len(acts) == 0 {
				m.setFlash(fmt.Sprintf("nothing to delete in bulk (%d risky orphans: delete one by one with X)", risky), true)
				return nil, true
			}
			title := fmt.Sprintf("Delete %d cleanable orphans (data directories on the nodes)", len(acts))
			if risky > 0 {
				title += fmt.Sprintf(" – %d risky ones skipped", risky)
			}
			m.confirmBatch(title, acts)
			if risky > 0 {
				m.setFlash(fmt.Sprintf("%d risky orphans skipped (volume deleted/faulted/degraded): delete them one by one with X", risky), true)
			}
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
	if p.kind == pDetail && p.panel == panelJobs && key == "space" {
		if !m.requireRW() {
			return nil, true
		}
		rows := m.rowsFor(p)
		vv := m.findView(p.vol)
		if p.cursor >= len(rows) || vv == nil || p.pdata == nil {
			return nil, true
		}
		ref, ok := rows[p.cursor].ref.(longhorn.JobRef)
		if !ok {
			return nil, true
		}
		if !ref.Assigned(vv.Volume) && !ref.Group {
			for _, a := range longhorn.JobAssignments(vv.Volume, p.pdata.jobs) {
				if a.Ref == ref && a.How != "" {
					m.setFlash(ref.Name+" applies "+a.How+": remove the group to turn it off", true)
					return nil, true
				}
			}
		}
		// the PVC label check needs a GET; the preview warns if the PVC
		// is the source of the job labels
		c, ctx, v, jobs := m.client, m.ctx, vv.Volume, p.pdata.jobs
		m.modal = &modal{kind: modalBusy, busyText: "checking the PVC …"}
		return func() tea.Msg {
			var labels map[string]string
			if info, err := c.PVCInfo(ctx, v); err == nil && info != nil {
				labels = info.Labels
			}
			a, err := longhorn.SetJobAssignment(v, []longhorn.JobRef{ref}, !ref.Assigned(v), jobs, labels)
			return confirmMsg{action: a, err: err}
		}, true
	}
	if !strings.Contains("SBRXDCEUA", key) || len(key) != 1 {
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
		return m.volumeAction(m.targetVolumes(string(name), p), key, nil), true

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
		return m.volumeAction([]string{p.vol}, key, p), true

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

// targetVolumes: the marked volumes (volume list) or just cur.
func (m *model) targetVolumes(cur string, p *page) []string {
	if p.kind != pVolumes || len(m.marked) == 0 {
		return []string{cur}
	}
	var out []string
	for i := range m.views {
		if m.marked[m.views[i].Name] {
			out = append(out, m.views[i].Name)
		}
	}
	return out
}

// volumeAction: S snapshot, B backup, R replica count, U engine image,
// E expand, A attach/detach, X delete selected replica (details only).
// Works on one or many volumes (A: one).
func (m *model) volumeAction(vols []string, key string, detail *page) tea.Cmd {
	if !strings.Contains("SBRXUEA", key) {
		return nil
	}
	var views []*longhorn.VolumeView
	for _, n := range vols {
		if vv := m.findView(n); vv != nil {
			views = append(views, vv)
		}
	}
	if len(views) == 0 || !m.requireRW() {
		return nil
	}
	label := dash(views[0].PVC)
	if label == "-" {
		label = views[0].Name
	}
	if len(views) > 1 {
		label = fmt.Sprintf("%d volumes", len(views))
	}
	switch key {
	case "S":
		var acts []*longhorn.Action
		var skipped []string
		for _, vv := range views {
			if a, err := longhorn.CreateSnapshot(vv.Volume, ""); err != nil {
				skipped = append(skipped, err.Error())
			} else {
				acts = append(acts, a)
			}
		}
		m.confirmBatch("Create a snapshot of "+label, acts, skipped)
	case "B":
		var bvols []*longhorn.Volume
		var skipped, lines []string
		var warns []string
		for _, vv := range views {
			snapA, err := longhorn.CreateSnapshot(vv.Volume, "")
			if err == nil {
				_, err = longhorn.CreateBackup(vv.Volume, snapA.Name)
			}
			if err != nil {
				skipped = append(skipped, err.Error())
				continue
			}
			bvols = append(bvols, vv.Volume)
			lines = append(lines, fmt.Sprintf("%-40s → target %q", dash(vv.PVC), vv.Volume.Spec.BackupTargetName))
			warns = append(warns, snapA.Warnings...)
		}
		if len(bvols) == 0 {
			m.setFlash(firstOr(skipped, "nothing to back up"), true)
			return nil
		}
		sum := &longhorn.Action{
			Title: "Back up " + label + ": snapshot, wait until ready, then back up",
			Verb:  longhorn.VerbCreate, GVR: longhorn.GVRBackup, Name: fmt.Sprintf("%d backup(s)", len(bvols)),
			Lines: lines, Warnings: append(uniq(warns), skippedWarnings(skipped)...),
		}
		m.modal = &modal{kind: modalConfirm, backupVols: bvols, action: sum}
	case "R":
		initial := ""
		if len(views) == 1 {
			initial = strconv.Itoa(views[0].Volume.Spec.NumberOfReplicas)
		}
		m.prompt("Number of replicas for "+label, initial, func(s string) error {
			n, err := strconv.Atoi(strings.TrimSpace(s))
			if err != nil {
				return fmt.Errorf("not a number: %q", s)
			}
			var acts []*longhorn.Action
			var skipped []string
			for _, vv := range views {
				if as, err := longhorn.ReplicaCountActions(vv.Volume, n, m.state.Replicas); err != nil {
					skipped = append(skipped, dash(vv.PVC)+": "+err.Error())
				} else {
					acts = append(acts, as...)
				}
			}
			if w := longhorn.ReplicaCountWarning(n, m.state.Nodes); w != "" {
				for _, a := range acts {
					if a.Verb == longhorn.VerbPatch {
						a.Warnings = append(a.Warnings, w)
					}
				}
			}
			m.confirmBatch(fmt.Sprintf("Set replica count of %s to %d", label, n), acts, skipped)
			return nil
		})
	case "U":
		m.modal = &modal{kind: modalBusy, busyText: "loading engine images …"}
		return m.loadEngineEnv(views)
	case "A":
		if len(views) > 1 {
			m.setFlash("attach/detach works on one volume at a time", true)
			return nil
		}
		m.modal = &modal{kind: modalBusy, busyText: "reading attachment tickets …"}
		c, ctx, v := m.client, m.ctx, views[0].Volume
		return func() tea.Msg {
			va, err := c.VolumeAttachment(ctx, v.Name)
			return attachMsg{vol: v, va: va, err: err}
		}
	case "E":
		m.prompt("New size for "+label+"  (e.g. 20Gi, or +5Gi to grow by)", "+", func(in string) error {
			if strings.TrimSpace(in) == "+" || strings.TrimSpace(in) == "" {
				return fmt.Errorf("no size given")
			}
			m.modal = &modal{kind: modalBusy, busyText: "reading PVCs and StorageClasses …"}
			m.afterSubmit = m.loadExpand(views, in, label)
			return nil
		})
	case "X":
		if detail == nil || len(views[0].Replicas) == 0 {
			m.setFlash("open the volume and select a replica with [ and ]", true)
			return nil
		}
		m.deleteReplica(views[0].Replicas[min(detail.sel, len(views[0].Replicas)-1)].Name)
	}
	return nil
}

// confirmMsg opens the confirmation for an action built in the background.
type confirmMsg struct {
	action *longhorn.Action
	err    error
}

type attachMsg struct {
	vol *longhorn.Volume
	va  *longhorn.VolumeAttachment
	err error
}

// openAttach detaches a manually attached volume, or asks for the node
// and mode to attach it to.
func (m *model) openAttach(msg attachMsg) {
	m.modal = nil
	if msg.err != nil {
		m.setFlash("volume attachment: "+msg.err.Error(), true)
		return
	}
	if _, ok := msg.va.Spec.AttachmentTickets[longhorn.UITicketID]; ok {
		m.confirm(longhorn.DetachVolume(msg.vol, msg.va))
		return
	}
	var opts []pickOption
	for _, n := range m.state.Nodes {
		o := pickOption{label: n.Name, value: n.Name}
		switch {
		case !n.Ready():
			o.disabled = "node is not ready"
		case msg.vol.Status.CurrentNodeID != "" && msg.vol.Status.CurrentNodeID != n.Name:
			o.disabled = "volume is attached to " + msg.vol.Status.CurrentNodeID
		}
		if n.Name == msg.vol.Status.CurrentNodeID {
			o.label += "  ← attached here (" + strings.Join(msg.va.Tickets(), ", ") + ")"
		}
		opts = append(opts, o)
	}
	sort.Slice(opts, func(i, j int) bool { return (opts[i].disabled == "") && (opts[j].disabled != "") })
	m.modal = &modal{kind: modalPick, label: "Attach " + volLabelOf(msg.vol) + " to node", options: opts, onPick: func(node string) {
		var n *longhorn.Node
		for _, x := range m.state.Nodes {
			if x.Name == node {
				n = x
			}
		}
		m.modal = &modal{kind: modalPick, label: "Mode", options: []pickOption{
			{label: "normal (block device on the node)", value: "normal"},
			{label: "maintenance (no frontend, e.g. to revert a snapshot)", value: "maintenance"},
		}, onPick: func(mode string) {
			m.confirm(longhorn.AttachVolume(msg.vol, msg.va, n, mode == "maintenance"))
		}}
	}}
}

func volLabelOf(v *longhorn.Volume) string {
	if p := v.PVC(); p != "" {
		return p
	}
	return v.Name
}

type expandMsg struct {
	title   string
	acts    []*longhorn.Action
	skipped []string
}

// loadExpand reads each volume's PVC and StorageClass (needed to expand
// through the PVC) and builds the actions.
func (m *model) loadExpand(views []*longhorn.VolumeView, in, label string) tea.Cmd {
	c, ctx := m.client, m.ctx
	vols := make([]*longhorn.Volume, len(views))
	for i, vv := range views {
		vols[i] = vv.Volume
	}
	return func() tea.Msg {
		msg := expandMsg{title: "Expand " + label + " (" + strings.TrimSpace(in) + ")"}
		for _, v := range vols {
			who := v.Name
			if v.PVC() != "" {
				who = v.PVC()
			}
			size, err := longhorn.ParseSize(in, int64(v.Spec.Size))
			var pvc *longhorn.PVCInfo
			if err == nil {
				pvc, err = c.PVCInfo(ctx, v)
			}
			var a *longhorn.Action
			if err == nil {
				a, err = longhorn.ExpandVolume(v, pvc, size)
			}
			if err != nil {
				msg.skipped = append(msg.skipped, who+": "+err.Error())
				continue
			}
			msg.acts = append(msg.acts, a)
		}
		return msg
	}
}

type engineEnvMsg struct {
	vols []string
	env  longhorn.EngineUpgradeEnv
	err  error
}

func (m *model) loadEngineEnv(views []*longhorn.VolumeView) tea.Cmd {
	c, ctx := m.client, m.ctx
	names := make([]string, len(views))
	for i, v := range views {
		names[i] = v.Name
	}
	return func() tea.Msg {
		env, err := c.LoadEngineUpgradeEnv(ctx)
		return engineEnvMsg{vols: names, env: env, err: err}
	}
}

// openEnginePicker lists the engine images; picking one builds the
// upgrade actions for all target volumes.
func (m *model) openEnginePicker(msg engineEnvMsg) {
	m.modal = nil
	if msg.err != nil {
		m.setFlash("engine images: "+msg.err.Error(), true)
		return
	}
	current := map[string]int{}
	for _, n := range msg.vols {
		if vv := m.findView(n); vv != nil {
			current[vv.Volume.Spec.Image]++
		}
	}
	var opts []pickOption
	for _, ei := range msg.env.Images {
		l := fmt.Sprintf("%-10s %-55s %-10s %3d refs", dash(ei.Status.Version), ei.Spec.Image, ei.Status.State, ei.Status.RefCount)
		if ei.Spec.Image == msg.env.DefaultImage {
			l += "  default"
		}
		if n := current[ei.Spec.Image]; n > 0 {
			l += fmt.Sprintf("  ← current (%d)", n)
		}
		o := pickOption{label: l, value: ei.Spec.Image}
		switch {
		case ei.Status.Incompatible || ei.Status.State == "incompatible":
			o.disabled = "incompatible with this Longhorn version"
		case ei.Status.State != "deployed":
			o.disabled = "engine image is " + ei.Status.State
		}
		opts = append(opts, o)
	}
	if len(opts) == 0 {
		m.setFlash("no engine images found", true)
		return
	}
	label := "Engine image for " + plural(len(msg.vols), "volume")
	if msg.env.AutoLimit > 0 {
		label += "  (automatic engine upgrade is on: only the default image is allowed)"
	}
	m.modal = &modal{kind: modalPick, label: label, options: opts, onPick: func(img string) {
		var acts []*longhorn.Action
		var skipped []string
		for _, n := range msg.vols {
			vv := m.findView(n)
			if vv == nil {
				continue
			}
			var reps []*longhorn.Replica
			for _, r := range m.state.Replicas {
				if r.Spec.VolumeName == n {
					reps = append(reps, r)
				}
			}
			if a, err := longhorn.UpgradeEngine(vv.Volume, reps, img, msg.env); err != nil {
				skipped = append(skipped, err.Error())
			} else {
				acts = append(acts, a)
			}
		}
		m.confirmBatch("Upgrade engine of "+plural(len(msg.vols), "volume"), acts, skipped)
	}}
}

func plural(n int, what string) string {
	if n == 1 {
		return "1 " + what
	}
	return fmt.Sprintf("%d %ss", n, what)
}

func firstOr(s []string, def string) string {
	if len(s) > 0 {
		return s[0]
	}
	return def
}

func uniq(s []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range s {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

func skippedWarnings(skipped []string) []string {
	var out []string
	for i, s := range skipped {
		if i == 5 {
			out = append(out, fmt.Sprintf("… %d more skipped", len(skipped)-5))
			break
		}
		out = append(out, "skipped: "+s)
	}
	return out
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
	case modalForm:
		return m.formKey(k)
	case modalPrompt:
		switch key {
		case "esc":
			m.modal = nil
		case "enter":
			m.modal = nil
			if err := md.onSubmit(md.input); err != nil {
				m.setFlash(err.Error(), true)
			}
			// a submit that has to load something first leaves a command
			cmd := m.afterSubmit
			m.afterSubmit = nil
			return cmd
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
	if md.kind == modalPick {
		switch key {
		case "esc", "q":
			m.modal = nil
		case "up", "k":
			md.pick = max(md.pick-1, 0)
		case "down", "j":
			md.pick = min(md.pick+1, len(md.options)-1)
		case "enter":
			if md.pick < len(md.options) {
				o := md.options[md.pick]
				if o.disabled != "" {
					m.setFlash(o.disabled, true)
					return nil
				}
				m.modal = nil
				md.onPick(o.value)
				cmd := m.afterSubmit
				m.afterSubmit = nil
				return cmd
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
		if md.onConfirm != nil {
			m.setFlash("no server dry run for a multi-step switch – its steps depend on each other", true)
			return nil
		}
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
		if md.onConfirm != nil {
			return md.onConfirm()
		}
		if len(md.backupVols) > 0 {
			vols := md.backupVols
			m.modal = &modal{kind: modalBusy, busyText: fmt.Sprintf("creating snapshots, waiting until ready, then starting %d backup(s) …", len(vols))}
			c, ctx := m.client, m.ctx
			return func() tea.Msg {
				var failed []string
				for _, v := range vols {
					if _, err := c.SnapshotAndBackup(ctx, v, func(string) {}); err != nil {
						failed = append(failed, v.Name+": "+err.Error())
					}
				}
				var err error
				if len(failed) > 0 {
					err = fmt.Errorf("%d of %d failed – %s", len(failed), len(vols), failed[0])
				}
				return actionDoneMsg{title: fmt.Sprintf("%d backup(s) started (see 3 Activity)", len(vols)-len(failed)), err: err}
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
func (m *model) confirmBatch(title string, acts []*longhorn.Action, skipped ...[]string) {
	var skip []string
	for _, sk := range skipped {
		skip = append(skip, sk...)
	}
	if len(acts) == 0 {
		m.setFlash(firstOr(skip, "nothing to do"), true)
		return
	}
	if len(acts) == 1 && len(skip) == 0 {
		m.modal = &modal{kind: modalConfirm, action: acts[0]}
		return
	}
	sum := &longhorn.Action{Title: title, Verb: acts[0].Verb, GVR: acts[0].GVR, Namespace: acts[0].Namespace, Name: fmt.Sprintf("%d objects", len(acts))}
	var warns []string
	kinds := map[string]bool{}
	for _, a := range acts {
		if a.Namespace != sum.Namespace || a.GVR != sum.GVR {
			sum.Namespace = "(several)"
		}
		kinds[a.GVR.Resource] = true
		if len(kinds) > 1 {
			sum.Header = fmt.Sprintf("%s %d objects", sum.Verb, len(acts))
		}
		sum.Destructive = sum.Destructive || a.Destructive
		warns = append(warns, a.Warnings...)
		sum.Lines = append(sum.Lines, a.Summary())
	}
	sum.Warnings = append(uniq(warns), skippedWarnings(skip)...)
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
		if p.kind == pJobs {
			return m.loadJobs()
		}
		if p.kind == pTargets {
			return m.loadTargets()
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
				m.prompt("New value for "+s.Name, s.Value, func(v string) error {
					m.confirm(longhorn.SetSetting(s, v))
					return nil
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
	case modalForm:
		b.WriteString(m.viewForm(md.form, w))
	case modalPick:
		b.WriteString(sBold.Render(md.label) + "\n\n")
		for i, o := range md.options {
			l := fit(o.label, w-10)
			switch {
			case o.disabled != "":
				l = sDim.Render(l + "  (" + o.disabled + ")")
			case i == md.pick:
				l = sKey.Render(l)
			}
			mark := "  "
			if i == md.pick {
				mark = sKey.Render("▶ ")
			}
			b.WriteString(mark + l + "\n")
		}
		b.WriteString("\n" + sDim.Render("[↑↓] select  [enter] continue  [esc] cancel"))
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
		if md.onConfirm != nil {
			b.WriteString(yes + "   " + sKey.Render("[esc]") + " cancel")
		} else {
			b.WriteString(yes + "   " + sKey.Render("[d]") + " server dry run   " + sKey.Render("[esc]") + " cancel")
		}
	}
	border := cBorder
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
	return " " + lipgloss.NewStyle().Background(cErr).Foreground(cBadgeFg).Bold(true).Padding(0, 1).Render("RW")
}
