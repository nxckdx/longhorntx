package tui

import (
	"fmt"

	tea "charm.land/bubbletea/v2"

	"github.com/nxckdx/longhorntx/internal/config"
	"github.com/nxckdx/longhorntx/internal/format"
)

// Per-context settings from the config file (applied on every connect) and
// the traffic budget.

// contextColor returns the configured badge colour of a context ("" =
// automatic). Set by Run from the config file.
var contextColor = func(string) string { return "" }

func (m *model) applySettings(eff config.Effective) {
	m.eff = eff
	if !m.opts.RWSet {
		m.rw = !eff.ReadOnly
	}
	m.budget, m.budgetWarned, m.paused = eff.TrafficBudget, false, false
	if m.opts.Theme == "" && !m.themePicked {
		applyTheme(eff.Theme)
	}
}

// used is the traffic of the current connection (both directions).
func (m *model) used() int64 {
	if m.client == nil {
		return 0
	}
	return m.client.Cluster.Traffic.RX() + m.client.Cluster.Traffic.TX()
}

// checkBudget runs every second: warn at 80 %, stop all watches at 100 %.
// On-demand loads (snapshots, events, …) still work while paused.
func (m *model) checkBudget() {
	if m.budget <= 0 || m.paused || m.client == nil {
		return
	}
	u := m.used()
	switch {
	case u >= m.budget:
		m.paused = true
		m.shutdown()
		m.setFlash(fmt.Sprintf("traffic budget %s reached: updates paused · ctrl+r resumes", format.Bytes(m.budget)), true)
	case u >= m.budget*8/10 && !m.budgetWarned:
		m.budgetWarned = true
		m.setFlash(fmt.Sprintf("80 %% of the traffic budget used (%s of %s)", format.Bytes(u), format.Bytes(m.budget)), true)
	}
}

// resume reconnects after a budget pause; the new connection gets the
// budget again.
func (m *model) resume() tea.Cmd {
	m.setFlash("resumed: budget starts again", false)
	return m.switchContext(m.kctx)
}

// budgetHint for the header: "⏸ budget" when paused, usage from 80 %.
func (m *model) budgetHint() string {
	if m.budget <= 0 {
		return ""
	}
	if m.paused {
		return fg(cErr, fmt.Sprintf("⏸ paused: budget %s · ctrl+r", format.Bytes(m.budget))) + "  "
	}
	if u := m.used(); u >= m.budget*8/10 {
		return fg(cWarn, fmt.Sprintf("budget %d%%", u*100/m.budget)) + "  "
	}
	return ""
}
