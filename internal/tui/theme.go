package tui

import (
	"fmt"
	"image/color"
	"sort"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/nxckdx/longhorntx/internal/config"
)

// Themes: every colour of the TUI. "default" uses the 256-colour palette
// (works everywhere); the others use true colour, which lipgloss degrades
// on terminals without it. Select with --theme, `theme:` in the config
// file (globally or per context), or pick one with T (live preview).

type Theme struct {
	Dim, Faint, OK, Warn, Err, Info, Accent   string
	HeaderBg, SelectedBg, BarFg, TabBg, TabFg string
	BadgeFg, ProdBg, Border                   string
	Palette                                   []string // context badge backgrounds
}

var builtinThemes = map[string]Theme{
	"default": {
		Dim: "244", Faint: "238", OK: "42", Warn: "214", Err: "196", Info: "39", Accent: "212",
		HeaderBg: "236", SelectedBg: "237", BarFg: "252", TabBg: "238", TabFg: "231",
		BadgeFg: "231", ProdBg: "160", Border: "39",
		Palette: []string{"25", "28", "90", "130", "31", "94", "60", "66"},
	},
	"catppuccin": { // Mocha
		Dim: "#7f849c", Faint: "#45475a", OK: "#a6e3a1", Warn: "#fab387", Err: "#f38ba8", Info: "#89b4fa", Accent: "#f5c2e7",
		HeaderBg: "#313244", SelectedBg: "#45475a", BarFg: "#cdd6f4", TabBg: "#585b70", TabFg: "#cdd6f4",
		BadgeFg: "#11111b", ProdBg: "#f38ba8", Border: "#89b4fa",
		Palette: []string{"#89b4fa", "#a6e3a1", "#cba6f7", "#fab387", "#94e2d5", "#f9e2af", "#b4befe", "#74c7ec"},
	},
	"dracula": {
		Dim: "#6272a4", Faint: "#44475a", OK: "#50fa7b", Warn: "#ffb86c", Err: "#ff5555", Info: "#8be9fd", Accent: "#ff79c6",
		HeaderBg: "#343746", SelectedBg: "#44475a", BarFg: "#f8f8f2", TabBg: "#6272a4", TabFg: "#f8f8f2",
		BadgeFg: "#282a36", ProdBg: "#ff5555", Border: "#bd93f9",
		Palette: []string{"#bd93f9", "#50fa7b", "#ff79c6", "#ffb86c", "#8be9fd", "#f1fa8c", "#6272a4", "#a4ffff"},
	},
	"nord": {
		Dim: "#7b88a1", Faint: "#434c5e", OK: "#a3be8c", Warn: "#ebcb8b", Err: "#bf616a", Info: "#88c0d0", Accent: "#b48ead",
		HeaderBg: "#3b4252", SelectedBg: "#434c5e", BarFg: "#eceff4", TabBg: "#4c566a", TabFg: "#eceff4",
		BadgeFg: "#2e3440", ProdBg: "#bf616a", Border: "#88c0d0",
		Palette: []string{"#81a1c1", "#a3be8c", "#b48ead", "#d08770", "#88c0d0", "#ebcb8b", "#5e81ac", "#8fbcbb"},
	},
	"gruvbox": {
		Dim: "#928374", Faint: "#504945", OK: "#b8bb26", Warn: "#fabd2f", Err: "#fb4934", Info: "#83a598", Accent: "#d3869b",
		HeaderBg: "#3c3836", SelectedBg: "#504945", BarFg: "#ebdbb2", TabBg: "#665c54", TabFg: "#fbf1c7",
		BadgeFg: "#1d2021", ProdBg: "#fb4934", Border: "#83a598",
		Palette: []string{"#83a598", "#b8bb26", "#d3869b", "#fe8019", "#8ec07c", "#fabd2f", "#458588", "#689d6a"},
	},
	"light": { // Catppuccin Latte, for light terminal backgrounds
		Dim: "#6c6f85", Faint: "#bcc0cc", OK: "#40a02b", Warn: "#df8e1d", Err: "#d20f39", Info: "#1e66f5", Accent: "#ea76cb",
		HeaderBg: "#e6e9ef", SelectedBg: "#ccd0da", BarFg: "#4c4f69", TabBg: "#bcc0cc", TabFg: "#4c4f69",
		BadgeFg: "#eff1f5", ProdBg: "#d20f39", Border: "#1e66f5",
		Palette: []string{"#1e66f5", "#40a02b", "#8839ef", "#fe640b", "#179299", "#df8e1d", "#7287fd", "#209fb5"},
	},
}

// themes: built-in plus the config file's (RegisterThemes).
var themes = builtinThemes

// BuiltinThemeNames lists the built-in themes, default first.
func BuiltinThemeNames() []string { return sortedNames(builtinThemes) }

// ThemeNames lists all themes incl. the config file's, default first.
func ThemeNames() []string { return sortedNames(themes) }

func sortedNames(m map[string]Theme) []string {
	names := []string{"default"}
	for n := range m {
		if n != "default" {
			names = append(names, n)
		}
	}
	sort.Strings(names[1:])
	return names
}

// RegisterThemes adds the config file's themes: each starts from its base
// (a built-in; default: the built-in of the same name, else "default") and
// overrides the colours it sets. Values were validated by config.Parse.
func RegisterThemes(defs map[string]config.ThemeDef) {
	all := map[string]Theme{}
	for n, t := range builtinThemes {
		all[n] = t
	}
	for name, d := range defs {
		base := string(d.Base)
		if base == "" {
			base = "default"
			if _, ok := builtinThemes[name]; ok {
				base = name
			}
		}
		t := builtinThemes[base]
		set := func(dst *string, v *config.Str) {
			if v != nil {
				*dst = string(*v)
			}
		}
		set(&t.Dim, d.Dim)
		set(&t.Faint, d.Faint)
		set(&t.OK, d.OK)
		set(&t.Warn, d.Warn)
		set(&t.Err, d.Error)
		set(&t.Info, d.Info)
		set(&t.Accent, d.Accent)
		set(&t.HeaderBg, d.HeaderBg)
		set(&t.SelectedBg, d.SelectedBg)
		set(&t.BarFg, d.BarFg)
		set(&t.TabBg, d.TabBg)
		set(&t.TabFg, d.TabFg)
		set(&t.BadgeFg, d.BadgeFg)
		set(&t.ProdBg, d.ProdBg)
		set(&t.Border, d.Border)
		if len(d.Palette) > 0 {
			t.Palette = make([]string, len(d.Palette))
			for i, c := range d.Palette {
				t.Palette[i] = string(c)
			}
		}
		all[name] = t
	}
	themes = all
}

// ThemeSwatch renders a theme's main colours for `lhtx config themes`.
func ThemeSwatch(name string) string {
	t, ok := themes[name]
	if !ok {
		return ""
	}
	sw := func(c, label string) string {
		return lipgloss.NewStyle().Foreground(lipgloss.Color(c)).Render("■ " + label)
	}
	bg := func(bgc, fgc, label string) string {
		return lipgloss.NewStyle().Background(lipgloss.Color(bgc)).Foreground(lipgloss.Color(fgc)).Padding(0, 1).Render(label)
	}
	out := sw(t.OK, "ok") + " " + sw(t.Warn, "warn") + " " + sw(t.Err, "error") + " " + sw(t.Info, "info") + " " +
		sw(t.Accent, "accent") + " " + sw(t.Dim, "dim") + "  " + bg(t.TabBg, t.TabFg, "tab") + " " + bg(t.ProdBg, t.BadgeFg, "prod")
	for i, c := range t.Palette {
		if i == 4 {
			break
		}
		out += " " + bg(c, t.BadgeFg, "ctx")
	}
	return out
}

var (
	curTheme                    = "default"
	cBadgeFg, cProdBg, cBorder  color.Color
	cSelectedBg, cTabBg, cTabFg color.Color
	contextPalette              []string
)

// applyTheme switches all colours and styles; unknown names keep the
// current theme and return false.
func applyTheme(name string) bool {
	if name == "" {
		name = "default"
	}
	t, ok := themes[name]
	if !ok {
		return false
	}
	curTheme = name
	c := func(s string) color.Color { return lipgloss.Color(s) }
	cDim, cFaint, cOK, cWarn, cErr, cInfo, cAccent = c(t.Dim), c(t.Faint), c(t.OK), c(t.Warn), c(t.Err), c(t.Info), c(t.Accent)
	cHeaderB, cSelectedBg, cTabBg, cTabFg = c(t.HeaderBg), c(t.SelectedBg), c(t.TabBg), c(t.TabFg)
	cBadgeFg, cProdBg, cBorder = c(t.BadgeFg), c(t.ProdBg), c(t.Border)
	contextPalette = t.Palette

	sDim = lipgloss.NewStyle().Foreground(cDim)
	sHead = lipgloss.NewStyle().Bold(true).Foreground(cDim)
	sSelected = lipgloss.NewStyle().Background(cSelectedBg).Bold(true)
	sBar = lipgloss.NewStyle().Background(cHeaderB).Foreground(c(t.BarFg))
	sKey = lipgloss.NewStyle().Foreground(cAccent).Bold(true)
	sErr = lipgloss.NewStyle().Foreground(cErr).Bold(true)
	sTabActive = lipgloss.NewStyle().Background(cTabBg).Foreground(cTabFg).Bold(true).Padding(0, 1)
	return true
}

func init() { applyTheme("default") }

// ---------------------------------------------------------------- picker (T)

// openThemePicker shows all themes; moving the cursor previews a theme,
// enter keeps it, esc goes back to the one before.
func (m *model) openThemePicker() {
	m.themeBefore = curTheme
	m.themeCursor = 0
	for i, n := range ThemeNames() {
		if n == curTheme {
			m.themeCursor = i
		}
	}
	m.push(&page{kind: pThemes})
}

func (m *model) themeKey(key string) {
	names := ThemeNames()
	switch key {
	case "up", "k":
		m.themeCursor = max(m.themeCursor-1, 0)
	case "down", "j":
		m.themeCursor = min(m.themeCursor+1, len(names)-1)
	case "enter":
		m.themePicked = true
		m.pop()
		m.setFlash("theme: "+curTheme+" for this session · keep it: theme: "+curTheme+" in "+config.DefaultPath(), false)
		return
	case "esc", "q", "T":
		applyTheme(m.themeBefore)
		m.pop()
		return
	default:
		return
	}
	applyTheme(names[m.themeCursor]) // live preview
}

func (m *model) viewThemes() string {
	var b strings.Builder
	b.WriteString("\n " + sBold.Render("Theme") + sDim.Render("  ↑↓ preview · enter keep · esc back to "+m.themeBefore) + "\n\n")
	names := ThemeNames()
	w := 0
	for _, n := range names {
		w = max(w, len(n))
	}
	for i, n := range names {
		cur := "  "
		if i == m.themeCursor {
			cur = sKey.Render("▶ ")
		}
		label := fmt.Sprintf("%-*s", w, n)
		if i == m.themeCursor {
			label = sBold.Render(label)
		}
		b.WriteString(fit(" "+cur+label+"  "+ThemeSwatch(n), m.width) + "\n")
	}
	b.WriteString("\n " + sDim.Render("Own themes and single colours: themes: in the config file (lhtx config themes)") + "\n")
	return b.String()
}
