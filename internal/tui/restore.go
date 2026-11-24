package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nxckdx/longhorntx/internal/longhorn"
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
	p, c := msg.plan, msg.client
	b := p.Backup
	pvc := ""
	if p.Spec.PVCNamespace != "" {
		pvc = p.Spec.PVCNamespace + "/" + p.Spec.Name
	}
	f := &form{title: fmt.Sprintf("Restore %s (%s) into a new volume", b.Name, backupWhen(b)), fields: []formField{
		{key: "name", label: "new volume", value: p.Spec.Name, hint: "also the PV name"},
		{key: "replicas", label: "replicas", value: fmt.Sprint(p.Spec.Replicas)},
		{key: "pvc", label: "PVC", value: pvc, hint: "namespace/name, empty = volume only"},
		{key: "sc", label: "storage class", value: p.Spec.StorageClass, hint: "of PV and PVC"},
	}}
	f.live = func(map[string]string) []string {
		return []string{"the original volume and its workload are not touched"}
	}
	f.submitBatch = func(v map[string]string) (string, []*longhorn.Action, error) {
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
