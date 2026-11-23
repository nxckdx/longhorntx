package tui

import (
	"hash/fnv"
	"image/color"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

var (
	cDim     = lipgloss.Color("244")
	cFaint   = lipgloss.Color("238")
	cOK      = lipgloss.Color("42")
	cWarn    = lipgloss.Color("214")
	cErr     = lipgloss.Color("196")
	cInfo    = lipgloss.Color("39")
	cAccent  = lipgloss.Color("212")
	cHeaderB = lipgloss.Color("236")

	sDim      = lipgloss.NewStyle().Foreground(cDim)
	sBold     = lipgloss.NewStyle().Bold(true)
	sHead     = lipgloss.NewStyle().Bold(true).Foreground(cDim)
	sSelected = lipgloss.NewStyle().Background(lipgloss.Color("237")).Bold(true)
	sBar      = lipgloss.NewStyle().Background(cHeaderB).Foreground(lipgloss.Color("252"))
	sKey      = lipgloss.NewStyle().Foreground(cAccent).Bold(true)
	sErr      = lipgloss.NewStyle().Foreground(cErr).Bold(true)
)

// contextPalette: background colours for the context badge. A context keeps
// its colour across runs (hash of the name), so customers are recognisable.
var contextPalette = []string{"25", "28", "90", "130", "31", "94", "60", "66"}

func contextStyle(name string) lipgloss.Style {
	bg := lipgloss.Color(contextPalette[hash(name)%uint32(len(contextPalette))])
	l := strings.ToLower(name)
	if strings.Contains(l, "prod") || strings.Contains(l, "prd") {
		bg = lipgloss.Color("160")
	}
	if c := contextColor(name); c != "" {
		bg = lipgloss.Color(c)
	}
	return lipgloss.NewStyle().Background(bg).Foreground(lipgloss.Color("231")).Bold(true).Padding(0, 1)
}

func hash(s string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return h.Sum32()
}

func robustnessColor(r string) color.Color {
	switch r {
	case "healthy":
		return cOK
	case "degraded":
		return cWarn
	case "faulted":
		return cErr
	}
	return cDim
}

func stateColor(s string) color.Color {
	switch s {
	case "attached", "running":
		return cOK
	case "attaching", "detaching", "starting", "stopping", "creating":
		return cInfo
	case "error":
		return cErr
	}
	return cDim
}

func fg(c color.Color, s string) string { return lipgloss.NewStyle().Foreground(c).Render(s) }

// fit truncates or pads s (which may contain ANSI) to exactly w cells.
func fit(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if lipgloss.Width(s) > w {
		s = ansi.Truncate(s, w, "…")
	}
	if pad := w - lipgloss.Width(s); pad > 0 {
		s += strings.Repeat(" ", pad)
	}
	return s
}

// bar renders a progress bar with sub-cell precision (eighth blocks), so
// interpolated values animate smoothly.
func bar(pct float64, w int, c color.Color) string {
	if w < 1 {
		return ""
	}
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	eighths := int(pct / 100 * float64(w*8))
	full, rem := eighths/8, eighths%8
	partials := []string{"", "▏", "▎", "▍", "▌", "▋", "▊", "▉"}
	s := strings.Repeat("█", full)
	empty := w - full
	if rem > 0 {
		s += partials[rem]
		empty--
	}
	return fg(c, s) + fg(cFaint, strings.Repeat("░", max(empty, 0)))
}

var sTabActive = lipgloss.NewStyle().Background(lipgloss.Color("238")).Foreground(lipgloss.Color("231")).Bold(true).Padding(0, 1)
