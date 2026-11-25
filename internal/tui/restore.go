package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nxckdx/longhorntx/internal/longhorn"
	"github.com/nxckdx/longhorntx/internal/switchover"
)

// Restore (write mode): r in a backup list restores the selected backup,
// r on the backup volumes page the newest one – into a NEW volume, with a
// PV/PVC if wanted. Loading the plan (backup volume, settings) is async;
// the form's submit checks for existing objects before the preview.

type restorePlanMsg struct {
	client *longhorn.Client
	plan   *longhorn.RestorePlan
	err    error
}

// startRestore loads the plan for a backup, or for the newest backup of
// volume when b is nil.
func (m *model) startRestore(b *longhorn.Backup, volume string) tea.Cmd {
	if !m.requireRW() || m.client == nil {
		return nil
	}
	c, ctx := m.client, m.ctx
	m.setFlash("preparing restore …", false)
	return func() tea.Msg {
		if b == nil {
			bks, err := c.Backups(ctx, volume)
			if err != nil {
				return restorePlanMsg{client: c, err: err}
			}
			if b = longhorn.LatestBackup(bks); b == nil {
				return restorePlanMsg{client: c, err: fmt.Errorf("no completed backup of %s", volume)}
			}
		}
		p, err := c.PlanRestore(ctx, b, time.Now())
		return restorePlanMsg{client: c, plan: p, err: err}
	}
}

func (m *model) restoreForm(msg restorePlanMsg) {
	if msg.client != m.client {
		return
	}
	if msg.err != nil {
		m.setFlash("restore: "+msg.err.Error(), true)
		return
	}
	m.flash = "" // "preparing restore …" is done
	p, c := msg.plan, msg.client
	b := p.Backup
	pvc := ""
	if p.Spec.PVCNamespace != "" {
		pvc = p.Spec.PVCNamespace + "/" + p.Spec.Name
	}
	// switching is offered when the backed-up volume still has its PVC
	srcPVC := ""
	if vv := m.findView(b.Volume()); vv != nil {
		srcPVC = vv.PVC
		p.Spec.Replicas = vv.Volume.Spec.NumberOfReplicas // like the volume it may replace
	}
	const newOnly = "new volume (+ PVC below)"
	switchTo := "switch " + srcPVC + " to it"
	fields := []formField{
		{key: "name", label: "new volume", value: p.Spec.Name, hint: "also the PV name"},
		{key: "replicas", label: "replicas", value: fmt.Sprint(p.Spec.Replicas)},
		{key: "pvc", label: "PVC", value: pvc, hint: "namespace/name, empty = volume only"},
		{key: "sc", label: "storage class", value: p.Spec.StorageClass, hint: "of PV and PVC"},
	}
	if srcPVC != "" {
		fields = append(fields, formField{key: "then", label: "then", value: newOnly, options: []string{newOnly, switchTo},
			hint: "←→: switch = pods frozen, PVC re-created on the restored data, then unfrozen"})
	}
	f := &form{title: fmt.Sprintf("Restore %s (%s) into a new volume", b.Name, backupWhen(b)), fields: fields}
	f.live = func(v map[string]string) []string {
		if v["then"] == switchTo {
			return []string{"enter shows every step of the switch before anything happens"}
		}
		return []string{"the original volume and its workload are not touched"}
	}
	f.submitBatch = func(v map[string]string) (string, []*longhorn.Action, error) {
		if v["then"] == switchTo {
			return "", nil, m.planRestoreSwitch(c, p, srcPVC, v)
		}
		p.Spec.Name = strings.TrimSpace(v["name"])
		n, err := strconv.Atoi(strings.TrimSpace(v["replicas"]))
		if err != nil {
			return "", nil, fmt.Errorf("replicas: a number")
		}
		p.Spec.Replicas = n
		p.Spec.StorageClass = strings.TrimSpace(v["sc"])
		p.Spec.PVCNamespace, p.Spec.PVCName = "", ""
		if s := strings.TrimSpace(v["pvc"]); s != "" {
			ns, name, ok := strings.Cut(s, "/")
			if !ok || ns == "" || name == "" {
				return "", nil, fmt.Errorf("PVC: namespace/name")
			}
			p.Spec.PVCNamespace, p.Spec.PVCName = ns, name
		}
		// a few GETs (volume, PV, PVC exist?) – short, so done inline
		ctx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
		defer cancel()
		acts, err := c.BuildRestore(ctx, p)
		if err != nil {
			return "", nil, err
		}
		return acts[0].Title, acts, nil
	}
	m.openForm(f)
}

func backupWhen(b *longhorn.Backup) string {
	t, err := time.Parse(time.RFC3339, longhorn.BackupTime(b))
	if err != nil {
		return "?"
	}
	return t.Local().Format("2006-01-02 15:04")
}

// restoreKey: r on the backup volumes page and in backup lists.
func (m *model) restoreKey(p *page, key string) (tea.Cmd, bool) {
	if key == "C" && p.kind == pDetail && p.panel == panelSnapshots {
		rows := m.rowsFor(p)
		vv := m.findView(p.vol)
		if p.cursor < len(rows) && vv != nil {
			if s, ok := rows[p.cursor].ref.(*longhorn.VolumeSnapshot); ok {
				return m.startClone(vv.Volume, s.Name), true
			}
		}
		return nil, true
	}
	if key != "r" {
		return nil, false
	}
	rows := m.rowsFor(p)
	if p.cursor >= len(rows) {
		return nil, false
	}
	switch {
	case p.kind == pBackupVols:
		if v, ok := rows[p.cursor].ref.(longhorn.BackupVolumeView); ok {
			return m.startRestore(nil, v.Volume), true
		}
	case p.kind == pBackupList || (p.kind == pDetail && p.panel == panelBackups):
		if b, ok := rows[p.cursor].ref.(*longhorn.Backup); ok {
			return m.startRestore(b, ""), true
		}
	}
	return nil, false
}

// ---------------------------------------------------------------- clone (C)

type clonePlanMsg struct {
	client   *longhorn.Client
	src      *longhorn.Volume
	snapshot string
	spec     longhorn.RestoreSpec
}

// startClone loads the defaults (one GET of a setting) and opens the form.
func (m *model) startClone(src *longhorn.Volume, snapshot string) tea.Cmd {
	if !m.requireRW() || m.client == nil {
		return nil
	}
	c, ctx := m.client, m.ctx
	return func() tea.Msg {
		return clonePlanMsg{client: c, src: src, snapshot: snapshot, spec: c.CloneDefaults(ctx, src, time.Now())}
	}
}

func (m *model) cloneForm(msg clonePlanMsg) {
	if msg.client != m.client {
		return
	}
	c, s := msg.client, msg.spec
	pvc := ""
	if s.PVCNamespace != "" {
		pvc = s.PVCNamespace + "/" + s.Name
	}
	what := "current data"
	if msg.snapshot != "" {
		what = "snapshot " + msg.snapshot
	}
	f := &form{title: "Clone " + what + " of " + volLabelOf(msg.src) + " into a new volume", fields: []formField{
		{key: "name", label: "new volume", value: s.Name, hint: "also the PV name"},
		{key: "replicas", label: "replicas", value: fmt.Sprint(s.Replicas)},
		{key: "pvc", label: "PVC", value: pvc, hint: "namespace/name, empty = volume only"},
		{key: "sc", label: "storage class", value: s.StorageClass, hint: "of PV and PVC"},
	}}
	f.live = func(map[string]string) []string {
		return []string{"the source volume is not changed – mount the clone next to it to look at the old data"}
	}
	f.submitBatch = func(v map[string]string) (string, []*longhorn.Action, error) {
		s.Name = strings.TrimSpace(v["name"])
		n, err := strconv.Atoi(strings.TrimSpace(v["replicas"]))
		if err != nil {
			return "", nil, fmt.Errorf("replicas: a number")
		}
		s.Replicas, s.StorageClass = n, strings.TrimSpace(v["sc"])
		s.PVCNamespace, s.PVCName = "", ""
		if p := strings.TrimSpace(v["pvc"]); p != "" {
			ns, name, ok := strings.Cut(p, "/")
			if !ok || ns == "" || name == "" {
				return "", nil, fmt.Errorf("PVC: namespace/name")
			}
			s.PVCNamespace, s.PVCName = ns, name
		}
		ctx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
		defer cancel()
		acts, err := c.BuildClone(ctx, msg.src, msg.snapshot, s)
		if err != nil {
			return "", nil, err
		}
		return acts[0].Title, acts, nil
	}
	m.openForm(f)
}

// planRestoreSwitch plans restore + switch and opens the step preview.
func (m *model) planRestoreSwitch(c *longhorn.Client, p *longhorn.RestorePlan, srcPVC string, v map[string]string) error {
	p.Spec.Name = strings.TrimSpace(v["name"])
	n, err := strconv.Atoi(strings.TrimSpace(v["replicas"]))
	if err != nil {
		return fmt.Errorf("replicas: a number")
	}
	p.Spec.Replicas, p.Spec.PVCNamespace, p.Spec.PVCName = n, "", ""
	ctx, cancel := context.WithTimeout(m.ctx, 20*time.Second)
	defer cancel()
	acts, err := c.BuildRestore(ctx, p)
	if err != nil {
		return err
	}
	ns, pvc, _ := strings.Cut(srcPVC, "/")
	sw, err := switchover.BuildPending(ctx, c, ns, pvc, p.Spec.Name, int64(p.Backup.Status.VolumeSize), p.Backup.Encrypted())
	if err != nil {
		return err
	}
	acts[0].Warnings = nil
	m.confirmSwitch(sw, acts[0])
	return errFormDone
}
