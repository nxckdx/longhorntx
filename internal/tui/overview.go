package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/nxckdx/longhorntx/internal/longhorn"
	"github.com/nxckdx/longhorntx/internal/overview"
)

// Overview in the context picker: o checks every context (like
// `lhtx overview`, 4 at a time) and shows the result next to its name.
// Costs one doctor per cluster, so it only runs on request.

type overviewMsg struct {
	run int
	sum overview.Summary
}

const overviewParallel = 4

func (m *model) startOverview() tea.Cmd {
	m.ovRun++
	run := m.ovRun
	m.ov, m.ovPending, m.ovAt = map[string]overview.Summary{}, map[string]bool{}, time.Now()
	sem := make(chan struct{}, overviewParallel)
	var cmds []tea.Cmd
	for _, name := range m.ctxNames {
		_, _, eff := m.opts.settingsFor(name)
		if !eff.Overview {
			continue
		}
		m.ovPending[name] = true
		name := name
		o, parent, demoClient := m.opts, m.ctx, m.client
		cmds = append(cmds, func() tea.Msg {
			sem <- struct{}{}
			defer func() { <-sem }()
			ctx, cancel := context.WithTimeout(parent, 45*time.Second)
			defer cancel()
			if o.Demo {
				return overviewMsg{run, overview.Summarize(ctx, demoClient)}
			}
			ns, _, _ := o.settingsFor(name)
			return overviewMsg{run, overview.Check(ctx, o.Kube, name, ns)}
		})
	}
	if len(cmds) == 0 {
		m.setFlash("all contexts have overview: false", true)
		return nil
	}
	m.setFlash(fmt.Sprintf("checking %d clusters …", len(cmds)), false)
	return tea.Batch(cmds...)
}

func (m *model) applyOverview(msg overviewMsg) {
	if msg.run != m.ovRun {
		return
	}
	m.ov[msg.sum.Context] = msg.sum
	delete(m.ovPending, msg.sum.Context)
	if len(m.ovPending) == 0 {
		m.ovAt = time.Now()
		m.setFlash("all clusters checked", false)
	}
}

// overviewCell renders the result for one context ("" if never checked).
func (m *model) overviewCell(name string) string {
	if m.ovPending[name] {
		return sDim.Render(spinner(time.Since(m.ovAt)) + " checking …")
	}
	s, ok := m.ov[name]
	if !ok {
		if m.ov != nil {
			return sDim.Render("skipped (overview: false)")
		}
		return ""
	}
	status := s.Status()
	var st string
	switch {
	case s.Error != "":
		return sErr.Render("ERROR") + " " + sDim.Render(s.Error)
	case s.NotInstalled:
		return sDim.Render("no Longhorn")
	case s.Worst == longhorn.SevCritical:
		st = sErr.Render(fmt.Sprintf("%-8s", status))
	case s.Worst == longhorn.SevWarning:
		st = fg(cWarn, fmt.Sprintf("%-8s", status))
	default:
		st = fg(cOK, fmt.Sprintf("%-8s", status))
	}
	parts := []string{fmt.Sprintf("%3d vol", s.Volumes), fmt.Sprintf("nodes %d/%d", s.NodesReady, s.Nodes)}
	if s.Degraded > 0 {
		parts = append(parts, fg(cWarn, fmt.Sprintf("%d degraded", s.Degraded)))
	}
	if s.Faulted > 0 {
		parts = append(parts, sErr.Render(fmt.Sprintf("%d faulted", s.Faulted)))
	}
	if s.Rebuilding > 0 {
		parts = append(parts, fg(cInfo, fmt.Sprintf("%d rebuilding", s.Rebuilding)))
	}
	if s.Agent != "" {
		parts = append(parts, "agent")
	}
	out := st + " " + sDim.Render(s.Version) + "  " + strings.Join(parts, " · ")
	if s.Top != "" {
		out += "  " + sDim.Render(s.Top)
	}
	return out
}

// contextLines renders the picker rows: badge, padded, then the overview.
func (m *model) contextLines() []string {
	w := 0
	for _, n := range m.ctxNames {
		w = max(w, lipgloss.Width(contextStyle(n).Render(n)))
	}
	lines := make([]string, 0, len(m.ctxNames))
	for i, n := range m.ctxNames {
		mark := "  "
		if n == m.kctx {
			mark = fg(cOK, "● ")
		}
		badge := contextStyle(n).Render(n)
		line := mark + badge + strings.Repeat(" ", w-lipgloss.Width(badge)) + "  " + m.overviewCell(n)
		if i == m.ctxCursor {
			line = sKey.Render("▶ ") + line
		} else {
			line = "  " + line
		}
		lines = append(lines, fit(" "+line, m.width))
	}
	return lines
}
