package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nxckdx/longhorntx/internal/config"
)

// lhtx settings page (","): the effective config of the connected context
// and where each value comes from. enter changes a value for this context
// or for all (defaults); lhtx writes config.yaml (comments stay) and
// applies it right away.

type prefRef struct{ key string }

var prefHelp = map[string]string{
	"refresh":       "auto | watch | 30s …",
	"autoPollRate":  "updates/min before auto polls, 0 = never",
	"readOnly":      "false = start in write mode",
	"theme":         "default, catppuccin, … or own",
	"color":         "context badge: auto, red, … or 0-255",
	"trafficBudget": "e.g. 200MiB, 0 = off",
	"overview":      "include in lhtx overview",
	"namespace":     "Longhorn namespace",
}

func (m *model) cfg() *config.File {
	if m.opts.Config == nil {
		m.opts.Config = &config.File{}
	}
	return m.opts.Config
}

func (m *model) prefValue(e config.Effective, key string) string {
	switch key {
	case "refresh":
		return e.Refresh
	case "autoPollRate":
		return fmt.Sprint(e.AutoPollRate)
	case "readOnly":
		return fmt.Sprint(e.ReadOnly)
	case "theme":
		if e.Theme == "" {
			return "default"
		}
		return e.Theme
	case "color":
		if e.Color == "" {
			return "auto"
		}
		return e.Color
	case "trafficBudget":
		if e.TrafficBudget == 0 {
			return "0"
		}
		return fmt.Sprint(e.TrafficBudget)
	case "overview":
		return fmt.Sprint(e.Overview)
	case "namespace":
		return e.Namespace
	}
	return ""
}

// flagOverride names a command line flag that beats the file for key.
func (m *model) flagOverride(key string) string {
	switch {
	case key == "refresh" && m.opts.RefreshSet:
		return "--refresh / LHTX_REFRESH"
	case key == "readOnly" && m.opts.RWSet:
		return "--rw"
	case key == "namespace" && m.opts.NamespaceSet:
		return "-n"
	case key == "theme" && m.opts.Theme != "":
		return "--theme"
	}
	return ""
}

func (m *model) prefRows() []row {
	e := m.cfg().For(m.kctx)
	rows := make([]row, 0, len(config.Keys))
	for _, k := range config.Keys {
		from := e.Source[k]
		if f := m.flagOverride(k); f != "" {
			from = fg(cWarn, "flag "+f) + sDim.Render(" (beats the file)")
		} else if from == "built-in" {
			from = sDim.Render(from)
		}
		k := k
		rows = append(rows, row{cells: []string{k, m.prefValue(e, k), from, sDim.Render(prefHelp[k])}, ref: prefRef{k},
			open: func() tea.Cmd { m.editPref(k); return nil }})
	}
	return rows
}

func (m *model) prefsTitle() string {
	path := config.DefaultPath()
	t := " " + sBold.Render("lhtx settings") + sDim.Render("  for context "+m.ctxLabel()+" · "+path+" · enter change · lhtx config themes")
	if m.cfg().Path == "" {
		t += sDim.Render(" · no file yet: the first change creates it")
	}
	return t
}

// editPref asks for the value, then where to store it.
func (m *model) editPref(key string) {
	e := m.cfg().For(m.kctx)
	cur := m.prefValue(e, key)
	if key == "trafficBudget" && e.TrafficBudget > 0 {
		cur = formatBudget(e.TrafficBudget)
	}
	m.prompt(fmt.Sprintf("%s (%s) – empty resets to the inherited value", key, prefHelp[key]), cur, func(v string) error {
		v = strings.TrimSpace(v)
		opts := []pickOption{
			{label: "only for context " + m.ctxLabel(), value: m.kctx},
			{label: "for all contexts (defaults)", value: ""},
		}
		if m.kctx == "" {
			opts = opts[1:]
		}
		m.modal = &modal{kind: modalPick, label: fmt.Sprintf("Set %s = %q", key, v), options: opts, onPick: func(ctx string) {
			m.savePref(ctx, key, v)
		}}
		return nil
	})
}

func formatBudget(n int64) string {
	switch {
	case n%(1<<30) == 0:
		return fmt.Sprintf("%dGiB", n>>30)
	case n%(1<<20) == 0:
		return fmt.Sprintf("%dMiB", n>>20)
	}
	return fmt.Sprint(n)
}

func (m *model) savePref(ctx, key, value string) {
	path := config.DefaultPath()
	if err := config.Set(path, ctx, key, value); err != nil {
		m.setFlash(err.Error(), true)
		return
	}
	f, err := config.Load(path)
	if err != nil {
		m.setFlash(err.Error(), true)
		return
	}
	m.opts.Config = f
	RegisterThemes(f.Themes)
	contextColor = func(name string) string {
		c, _ := config.ColorCode(f.For(name).Color)
		return c
	}
	where := "defaults"
	if ctx != "" {
		where = "context " + ctx
	}
	m.setFlash(fmt.Sprintf("saved %s for %s", key, where), false)
	eff := f.For(m.kctx)
	if m.flagOverride(key) != "" {
		m.setFlash(fmt.Sprintf("saved %s for %s – the command line flag still wins in this session", key, where), false)
		return
	}
	m.eff = eff
	switch key {
	case "refresh", "autoPollRate", "namespace":
		m.afterSubmit = m.switchContext(m.kctx) // reconnect to apply
	case "theme":
		m.themePicked = false
		applyTheme(eff.Theme)
	case "readOnly":
		m.rw = !eff.ReadOnly
	case "trafficBudget":
		m.budget, m.budgetWarned = eff.TrafficBudget, false
	}
}
