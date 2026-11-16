package tui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nxckdx/longhorntx/internal/format"
	"github.com/nxckdx/longhorntx/internal/longhorn"
)

// Panels show data that is not watched: they are loaded once when opened
// (one small, label/field-selected LIST) and reloaded with ctrl+r.

type panelData struct {
	kind     panelKind
	loading  bool
	err      error
	loadedAt time.Time
	snaps    []*longhorn.VolumeSnapshot
	backups  []*longhorn.Backup
	events   []longhorn.Event
	jobs     []*longhorn.RecurringJob
}

type panelMsg struct {
	vol  string
	data *panelData
}

var panelNames = map[panelKind]string{
	panelSnapshots: "Snapshots",
	panelBackups:   "Backups",
	panelEvents:    "Events",
	panelJobs:      "Recurring jobs",
}

var panelKeys = map[string]panelKind{"s": panelSnapshots, "b": panelBackups, "e": panelEvents, "J": panelJobs}

// detailKey handles keys specific to the detail page.
func (m *model) detailKey(p *page, key string) (tea.Cmd, bool) {
	if k, ok := panelKeys[key]; ok {
		if p.panel == k {
			p.panel, p.pdata = panelNone, nil
			return nil, true
		}
		p.panel, p.cursor, p.offset = k, 0, 0
		return m.loadPanel(p), true
	}
	switch key {
	case "tab":
		next := p.panel%panelJobs + 1
		p.panel, p.cursor, p.offset = next, 0, 0
		return m.loadPanel(p), true
	case "ctrl+r":
		if p.panel != panelNone {
			return m.loadPanel(p), true
		}
		return nil, true
	}
	return nil, false
}

func (m *model) loadPanel(p *page) tea.Cmd {
	if m.client == nil {
		return nil
	}
	kind, vol := p.panel, p.vol
	p.pdata = &panelData{kind: kind, loading: true}
	c := m.client
	ctx := m.ctx
	var refs []longhorn.ObjectRef
	var volume *longhorn.Volume
	if vv := m.findView(vol); vv != nil {
		refs = longhorn.VolumeEventRefs(c.Namespace, vv, m.state.Engines)
		volume = vv.Volume
	}
	return func() tea.Msg {
		d := &panelData{kind: kind, loadedAt: time.Now()}
		switch kind {
		case panelSnapshots:
			d.snaps, d.err = c.Snapshots(ctx, vol)
			longhorn.SortSnapshots(d.snaps)
			// newest first is more useful on screen
			for i, j := 0, len(d.snaps)-1; i < j; i, j = i+1, j-1 {
				d.snaps[i], d.snaps[j] = d.snaps[j], d.snaps[i]
			}
		case panelBackups:
			d.backups, d.err = c.Backups(ctx, vol)
			longhorn.SortBackups(d.backups)
		case panelEvents:
			d.events, d.err = c.Events(ctx, refs)
		case panelJobs:
			var jobs []*longhorn.RecurringJob
			jobs, d.err = c.RecurringJobs(ctx)
			if volume != nil {
				d.jobs = longhorn.VolumeRecurringJobs(volume, jobs)
			}
		}
		return panelMsg{vol: vol, data: d}
	}
}

func (m *model) applyPanel(msg panelMsg) {
	for _, p := range m.stack {
		if p.kind == pDetail && p.vol == msg.vol && p.panel == msg.data.kind {
			p.pdata = msg.data
		}
	}
	m.clamp()
}

func (m *model) panelCols(p *page) []col {
	switch p.panel {
	case panelSnapshots:
		return []col{{"NAME", 0, 1}, {"CREATED", 9, 0}, {"SIZE", 10, 0}, {"TYPE", 10, 0}, {"READY", 6, 0}}
	case panelBackups:
		return []col{{"NAME", 0, 1}, {"STATE", 11, 0}, {"PROGRESS", 18, 0}, {"CREATED", 9, 0}, {"SIZE", 10, 0}, {"UPLOADED", 10, 0}, {"TARGET", 10, 0}}
	case panelEvents:
		return []col{{"AGE", 9, 0}, {"TYPE", 8, 0}, {"REASON", 22, 0}, {"OBJECT", 0, 1}, {"MESSAGE", 0, 3}}
	case panelJobs:
		return []col{{"NAME", 0, 1}, {"TASK", 18, 0}, {"CRON", 14, 0}, {"RETAIN", 7, 0}, {"GROUPS", 0, 1}, {"RUNS", 6, 0}}
	}
	return nil
}

func (m *model) panelRows(p *page) []row {
	d := p.pdata
	if d == nil || d.loading || d.err != nil {
		return nil
	}
	now := time.Now()
	var rows []row
	switch p.panel {
	case panelSnapshots:
		for _, s := range d.snaps {
			kind := longhorn.SnapshotKind(s)
			if kind == "removed" {
				kind = fg(cWarn, kind)
			}
			ready := fg(cOK, "✔")
			if !s.Status.ReadyToUse {
				ready = fg(cWarn, "…")
			}
			rows = append(rows, row{cells: []string{s.Name, format.Age(s.Status.CreationTime, now), format.Bytes(int64(s.Status.Size)), kind, ready}, ref: s})
		}
	case panelBackups:
		for _, b := range d.backups {
			st := b.Status.State
			switch st {
			case "Completed":
				st = fg(cOK, st)
			case "Error":
				st = fg(cErr, st)
			default:
				st = fg(cInfo, st)
			}
			rows = append(rows, row{cells: []string{
				b.Name, st, bar(float64(b.Status.Progress), 10, cInfo) + fmt.Sprintf(" %3d%%", b.Status.Progress),
				format.Age(longhorn.BackupTime(b), now), format.Bytes(int64(b.Status.Size)),
				format.Bytes(int64(b.Status.NewlyUploadDataSize)), dash(b.Status.BackupTargetName),
			}})
		}
	case panelEvents:
		for _, e := range d.events {
			age := format.Age(e.Time.Format(time.RFC3339), now)
			if e.Count > 1 {
				age += fmt.Sprintf(" ×%d", e.Count)
			}
			typ := e.Type
			if typ == "Warning" {
				typ = fg(cWarn, typ)
			}
			rows = append(rows, row{cells: []string{age, typ, e.Reason, sDim.Render(e.Object), e.Message}})
		}
	case panelJobs:
		for _, j := range d.jobs {
			rows = append(rows, row{cells: []string{j.Name, j.Spec.Task, j.Spec.Cron, fmt.Sprint(j.Spec.Retain),
				dash(strings.Join(j.Spec.Groups, ",")), fmt.Sprint(j.Status.ExecutionCount)}})
		}
	}
	return rows
}

// panelTitle is the line above the panel list.
func (m *model) panelTitle(p *page) string {
	var tabs []string
	for _, k := range []panelKind{panelSnapshots, panelBackups, panelEvents, panelJobs} {
		key := map[panelKind]string{panelSnapshots: "s", panelBackups: "b", panelEvents: "e", panelJobs: "J"}[k]
		label := "[" + key + "] " + panelNames[k]
		if k == p.panel {
			label = sTabActive.Render(" " + panelNames[k] + " ")
		} else {
			label = sDim.Render(label)
		}
		tabs = append(tabs, label)
	}
	s := " " + strings.Join(tabs, "  ")
	if d := p.pdata; d != nil {
		switch {
		case d.loading:
			s += sDim.Render("   loading …")
		case d.err != nil:
			s += "   " + sErr.Render(d.err.Error())
		default:
			n := len(m.panelRows(p))
			s += sDim.Render(fmt.Sprintf("   %d · loaded %s ago · ctrl+r reload", n, format.Age(d.loadedAt.Format(time.RFC3339), time.Now())))
			if p.panel == panelEvents && n == 0 {
				s += sDim.Render(" · events expire after 1h")
			}
		}
	}
	return s
}
