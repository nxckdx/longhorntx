package tui

import (
	"testing"

	"github.com/nxckdx/longhorntx/internal/config"
)

func TestRegisterThemes(t *testing.T) {
	defer func() { themes = builtinThemes; applyTheme("default") }()
	acc, ok := config.Str("#ff79c6"), config.Str("#00ff87")
	RegisterThemes(map[string]config.ThemeDef{
		"mine":    {Base: "nord", Accent: &acc},
		"default": {OK: &ok},
	})
	if m := themes["mine"]; m.Accent != "#ff79c6" || m.OK != builtinThemes["nord"].OK {
		t.Errorf("mine: %+v", m)
	}
	if d := themes["default"]; d.OK != "#00ff87" || d.Warn != builtinThemes["default"].Warn {
		t.Errorf("default: %+v", d)
	}
	if builtinThemes["default"].OK == "#00ff87" {
		t.Error("built-in modified")
	}
	if !applyTheme("mine") || applyTheme("nope") || curTheme != "mine" {
		t.Error("applyTheme")
	}
	names := ThemeNames()
	if names[0] != "default" || len(names) != len(builtinThemes)+1 {
		t.Errorf("names %v", names)
	}
}
