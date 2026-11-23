package tui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nxckdx/longhorntx/internal/format"
	"github.com/nxckdx/longhorntx/internal/longhorn"
)

// Backups tab (6): the backup volumes – every volume with backups in the
// backup store, including deleted ones. Loaded when the tab opens (one
// LIST), joined with the live volume list on every render. enter lists the
// backups of one volume (pBackupList, like the detail panel).

type backupVolsMsg struct {
	client *longhorn.Client
	bvs    []*longhorn.BackupVolume
	err    error
}

func (m *model) loadBackupVols() tea.Cmd {
	if m.client == nil {
		return nil
	}
	c, ctx := m.client, m.ctx
	m.bvLoading = true
	return func() tea.Msg {
		bvs, err := c.BackupVolumes(ctx)
		return backupVolsMsg{client: c, bvs: bvs, err: err}
	}
}

func (m *model) applyBackupVols(msg backupVolsMsg) {
	if msg.client != m.client {
		return
	}
	m.bvLoading, m.bvErr = false, msg.err
	if msg.err == nil {
		m.bvRaw, m.bvAt = msg.bvs, time.Now()
	}
	m.clamp()
}

func (m *model) backupVolViews() []longhorn.BackupVolumeView {
	return longhorn.BuildBackupVolumes(m.bvRaw, m.state.Volumes)
}

func (m *model) backupVolRows() []row {
	views := m.backupVolViews()
	rows := make([]row, 0, len(views))
	now := time.Now()
	for _, v := range views {
		vol := sDim.Render(v.Volume)
		if !v.VolExists {
			vol = fg(cWarn, "deleted") + " " + sDim.Render(v.Volume)
		}
		last := format.Age(v.LastAt, now)
		if v.Message != "" {
			last += " " + fg(cWarn, "⚠")
		}
		r := row{cells: []string{dash(v.PVC), v.Target, last, format.Bytes(v.Size), format.Bytes(v.Stored), vol}, ref: v}
		v := v
		r.open = func() tea.Cmd {
			p := &page{kind: pBackupList, vol: v.Volume, panel: panelBackups}
			m.push(p)
			return m.loadPanel(p)
		}
		rows = append(rows, r)
	}
	return rows
}

func (m *model) backupVolsTitle() string {
	views := m.backupVolViews()
	gone := 0
	for _, v := range views {
		if !v.VolExists {
			gone++
		}
	}
	t := " " + sBold.Render(fmt.Sprintf("Backup volumes %d", len(views)))
	if gone > 0 {
		t += " " + fg(cWarn, fmt.Sprintf("%d of deleted volumes", gone))
	}
	t += sDim.Render("  volumes with backups in the backup store · enter: backups")
	switch {
	case m.bvLoading:
		t += sDim.Render(" · loading …")
	case m.bvErr != nil:
		t += "  " + sErr.Render(m.bvErr.Error())
	case !m.bvAt.IsZero():
		t += sDim.Render(fmt.Sprintf(" · loaded %s ago · ctrl+r reload", format.Age(m.bvAt.Format(time.RFC3339), time.Now())))
	}
	return t
}

// backupListTitle heads the backups of one backup volume.
func (m *model) backupListTitle(p *page) string {
	name := p.vol
	exists := false
	for _, v := range m.backupVolViews() {
		if v.Volume == p.vol {
			exists = v.VolExists
			if v.PVC != "" {
				name = v.PVC
			}
		}
	}
	t := " " + sBold.Render("Backups of "+name)
	if !exists {
		t += " " + fg(cWarn, "(volume deleted)")
	}
	if d := p.pdata; d != nil {
		switch {
		case d.loading:
			t += sDim.Render("  loading …")
		case d.err != nil:
			t += "  " + sErr.Render(d.err.Error())
		default:
			t += sDim.Render(fmt.Sprintf("  %d · loaded %s ago · ctrl+r reload", len(d.backups),
				format.Age(d.loadedAt.Format(time.RFC3339), time.Now())))
		}
	}
	if name != p.vol {
		t += sDim.Render("  " + p.vol)
	}
	return strings.TrimRight(t, " ")
}
