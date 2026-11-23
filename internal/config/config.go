// Package config reads ~/.config/lhtx/config.yaml: global defaults plus
// overrides per kubeconfig context (exact name or glob like "customer-*").
//
// Precedence: command line flag / environment > context > defaults > built-in.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/yaml"
)

// Settings are the configurable values; nil = not set at this level.
type Settings struct {
	// Refresh of the volume list: auto, watch or a poll interval like 30s.
	Refresh *Str `json:"refresh,omitempty"`
	// AutoPollRate: with refresh auto, switch from watch to poll above this
	// many volume updates per minute (0 = never).
	AutoPollRate *int `json:"autoPollRate,omitempty"`
	// ReadOnly false starts the TUI in write mode and lets the CLI change
	// things without --rw. Previews and confirmations stay.
	ReadOnly *bool `json:"readOnly,omitempty"`
	// Theme of the TUI: default, catppuccin, dracula, nord, gruvbox, light.
	Theme *Str `json:"theme,omitempty"`
	// Color of the context badge: a name (red, green, …) or 0–255.
	Color *Str `json:"color,omitempty"`
	// TrafficBudget per TUI session, e.g. 200MiB ("0" = off).
	TrafficBudget *Str `json:"trafficBudget,omitempty"`
	// Overview false leaves the context out of `lhtx overview`.
	Overview *bool `json:"overview,omitempty"`
	// Namespace Longhorn runs in.
	Namespace *Str `json:"namespace,omitempty"`
}

// ThemeDef overrides colours of a theme: "#rrggbb", "#rgb" or 0-255.
type ThemeDef struct {
	Base       Str   `json:"base,omitempty"`
	Dim        *Str  `json:"dim,omitempty"`        // secondary text
	Faint      *Str  `json:"faint,omitempty"`      // empty bar parts, unused cells
	OK         *Str  `json:"ok,omitempty"`         // healthy
	Warn       *Str  `json:"warn,omitempty"`       // degraded, warnings
	Error      *Str  `json:"error,omitempty"`      // faulted, errors, RW badge
	Info       *Str  `json:"info,omitempty"`       // rebuilding, progress
	Accent     *Str  `json:"accent,omitempty"`     // keys in the footer
	HeaderBg   *Str  `json:"headerBg,omitempty"`   // header bar
	SelectedBg *Str  `json:"selectedBg,omitempty"` // cursor row
	BarFg      *Str  `json:"barFg,omitempty"`      // text on the header bar
	TabBg      *Str  `json:"tabBg,omitempty"`      // active tab
	TabFg      *Str  `json:"tabFg,omitempty"`
	BadgeFg    *Str  `json:"badgeFg,omitempty"` // text on context/RW badges
	ProdBg     *Str  `json:"prodBg,omitempty"`  // badge of contexts named *prod*
	Border     *Str  `json:"border,omitempty"`  // dialog border
	Palette    []Str `json:"palette,omitempty"` // context badge backgrounds
}

// ValidColor accepts "#rrggbb", "#rgb" and 0-255.
func ValidColor(s string) error {
	if len(s) == 7 || len(s) == 4 {
		if s[0] == '#' {
			if _, err := strconv.ParseUint(s[1:], 16, 32); err == nil {
				return nil
			}
		}
	}
	if n, err := strconv.Atoi(s); err == nil && n >= 0 && n <= 255 {
		return nil
	}
	return fmt.Errorf("invalid colour %q (#rrggbb, #rgb or 0-255)", s)
}

func (t ThemeDef) validate() error {
	for _, c := range []*Str{t.Dim, t.Faint, t.OK, t.Warn, t.Error, t.Info, t.Accent, t.HeaderBg, t.SelectedBg,
		t.BarFg, t.TabBg, t.TabFg, t.BadgeFg, t.ProdBg, t.Border} {
		if c != nil {
			if err := ValidColor(string(*c)); err != nil {
				return err
			}
		}
	}
	for _, c := range t.Palette {
		if err := ValidColor(string(c)); err != nil {
			return fmt.Errorf("palette: %w", err)
		}
	}
	return nil
}

// Str is a string that may be written as a YAML number (color: 160,
// trafficBudget: 0).
type Str string

func (s *Str) UnmarshalJSON(b []byte) error {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	switch x := v.(type) {
	case string:
		*s = Str(x)
	case float64:
		*s = Str(strconv.FormatFloat(x, 'f', -1, 64))
	default:
		return fmt.Errorf("expected a string or number, got %s", b)
	}
	return nil
}

type File struct {
	Defaults Settings            `json:"defaults"`
	Contexts map[string]Settings `json:"contexts,omitempty"`
	// Themes: own themes or changes to the built-in ones; each starts from
	// Base (a built-in theme; default: the theme of the same name, else
	// "default") and overrides single colours.
	Themes map[string]ThemeDef `json:"themes,omitempty"`

	Path string `json:"-"` // where it was read from ("" = no file)
}

// Effective are the resolved values for one context.
type Effective struct {
	Context       string
	Refresh       string
	AutoPollRate  int
	ReadOnly      bool
	Color         string
	Theme         string
	TrafficBudget int64 // bytes, 0 = off
	Overview      bool
	Namespace     string
	// Source names where each value came from ("built-in", "defaults",
	// "contexts.<key>") for `lhtx config`.
	Source map[string]string
}

// Built-in values.
const (
	DefaultAutoPollRate = 120
	DefaultRefresh      = "auto"
	DefaultNamespace    = "longhorn-system"
)

// DefaultPath is $XDG_CONFIG_HOME/lhtx/config.yaml (~/.config/lhtx/…);
// LHTX_CONFIG overrides it.
func DefaultPath() string {
	if p := os.Getenv("LHTX_CONFIG"); p != "" {
		return p
	}
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "lhtx", "config.yaml")
}

// Load reads the file; a missing file gives an empty config.
func Load(p string) (*File, error) {
	f := &File{}
	if p == "" {
		return f, nil
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return nil, err
	}
	if err := Parse(b, f); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	f.Path = p
	return f, nil
}

// Parse decodes YAML strictly (unknown keys are errors, typos show up) and
// validates every value, so mistakes surface at start and not later.
func Parse(b []byte, f *File) error {
	if err := yaml.UnmarshalStrict(b, f); err != nil {
		// sigs.k8s.io/yaml goes through JSON; drop its wrapper text
		msg := err.Error()
		for _, p := range []string{"error unmarshaling JSON: ", "while decoding JSON: ", "json: "} {
			msg = strings.ReplaceAll(msg, p, "")
		}
		return errors.New(msg)
	}
	// themes first: settings may refer to them
	for name, t := range f.Themes {
		if err := t.validate(); err != nil {
			return fmt.Errorf("themes.%s: %w", name, err)
		}
		if t.Base != "" && ValidTheme(string(t.Base)) != nil {
			return fmt.Errorf("themes.%s: base %q is not a built-in theme", name, t.Base)
		}
	}
	themeOK := func(s string) error {
		if _, ok := f.Themes[s]; ok {
			return nil
		}
		return ValidTheme(s)
	}
	if err := f.Defaults.validate(themeOK); err != nil {
		return fmt.Errorf("defaults: %w", err)
	}
	for k, s := range f.Contexts {
		if _, err := path.Match(k, ""); err != nil {
			return fmt.Errorf("contexts.%s: bad pattern: %w", k, err)
		}
		if err := s.validate(themeOK); err != nil {
			return fmt.Errorf("contexts.%s: %w", k, err)
		}
	}
	return nil
}

func (s Settings) validate(themeOK func(string) error) error {
	if s.Refresh != nil {
		if err := ValidRefresh(string(*s.Refresh)); err != nil {
			return err
		}
	}
	if s.AutoPollRate != nil && *s.AutoPollRate < 0 {
		return fmt.Errorf("autoPollRate must be ≥ 0")
	}
	if s.Color != nil {
		if _, err := ColorCode(string(*s.Color)); err != nil {
			return err
		}
	}
	if s.Theme != nil {
		if err := themeOK(string(*s.Theme)); err != nil {
			return err
		}
	}
	if s.TrafficBudget != nil {
		if _, err := ParseBytes(string(*s.TrafficBudget)); err != nil {
			return fmt.Errorf("trafficBudget: %w", err)
		}
	}
	return nil
}

// ValidTheme checks built-in theme names; set by the caller (the TUI knows
// them). Themes defined in the file are accepted too.
var ValidTheme = func(string) error { return nil }

// ValidRefresh is set by the caller (longhorn.ParseRefresh) to avoid an
// import cycle; the default accepts anything.
var ValidRefresh = func(string) error { return nil }

// For resolves the settings of a context: defaults, then matching glob
// patterns (shorter first, so more specific ones win), then the exact name.
func (f *File) For(kctx string) Effective {
	e := Effective{Context: kctx, Refresh: DefaultRefresh, AutoPollRate: DefaultAutoPollRate, ReadOnly: true, Overview: true, Namespace: DefaultNamespace,
		Source: map[string]string{}}
	for _, k := range []string{"refresh", "autoPollRate", "readOnly", "color", "theme", "trafficBudget", "overview", "namespace"} {
		e.Source[k] = "built-in"
	}
	e.apply(f.Defaults, "defaults")
	var globs []string
	for k := range f.Contexts {
		if k != kctx && strings.ContainsAny(k, "*?[") {
			if ok, _ := path.Match(k, kctx); ok {
				globs = append(globs, k)
			}
		}
	}
	sort.Slice(globs, func(i, j int) bool {
		if len(globs[i]) != len(globs[j]) {
			return len(globs[i]) < len(globs[j])
		}
		return globs[i] < globs[j]
	})
	for _, k := range globs {
		e.apply(f.Contexts[k], "contexts."+k)
	}
	if s, ok := f.Contexts[kctx]; ok {
		e.apply(s, "contexts."+kctx)
	}
	return e
}

func (e *Effective) apply(s Settings, src string) {
	if s.Refresh != nil {
		e.Refresh, e.Source["refresh"] = string(*s.Refresh), src
	}
	if s.AutoPollRate != nil {
		e.AutoPollRate, e.Source["autoPollRate"] = *s.AutoPollRate, src
	}
	if s.ReadOnly != nil {
		e.ReadOnly, e.Source["readOnly"] = *s.ReadOnly, src
	}
	if s.Color != nil {
		e.Color, e.Source["color"] = string(*s.Color), src
	}
	if s.Theme != nil {
		e.Theme, e.Source["theme"] = string(*s.Theme), src
	}
	if s.TrafficBudget != nil {
		e.TrafficBudget, _ = ParseBytes(string(*s.TrafficBudget))
		e.Source["trafficBudget"] = src
	}
	if s.Overview != nil {
		e.Overview, e.Source["overview"] = *s.Overview, src
	}
	if s.Namespace != nil {
		e.Namespace, e.Source["namespace"] = string(*s.Namespace), src
	}
}

// ParseBytes parses "200MiB", "1.5GiB", "500M", "0".
func ParseBytes(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" {
		return 0, nil
	}
	// accept the common "MiB"/"MB" spellings besides Kubernetes' "Mi"/"M"
	q := strings.TrimSuffix(strings.TrimSuffix(s, "B"), "b")
	v, err := resource.ParseQuantity(q)
	if err != nil || v.Sign() < 0 {
		return 0, fmt.Errorf("invalid size %q (examples: 200MiB, 1GiB)", s)
	}
	return v.Value(), nil
}

var colorNames = map[string]string{
	"red": "160", "green": "28", "yellow": "136", "blue": "25", "magenta": "90",
	"purple": "90", "cyan": "31", "orange": "130", "gray": "240", "grey": "240",
}

// ColorCode maps a colour name or 256-colour code to the code ("" = auto).
func ColorCode(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" || s == "auto" || s == "default" {
		return "", nil
	}
	if c, ok := colorNames[s]; ok {
		return c, nil
	}
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err == nil && n >= 0 && n <= 255 && fmt.Sprint(n) == s {
		return s, nil
	}
	names := make([]string, 0, len(colorNames))
	for k := range colorNames {
		names = append(names, k)
	}
	sort.Strings(names)
	return "", fmt.Errorf("unknown color %q (%s or 0-255)", s, strings.Join(names, ", "))
}

// Example is written by `lhtx config init`.
const Example = `# lhtx configuration – https://github.com/nxckdx/longhorntx
# Precedence: flag/env > contexts > defaults > built-in.
# Show the effective values: lhtx config [--context NAME]

defaults:
  # volume list updates: auto (watch below 150 volumes, else poll 30s),
  # watch (live) or a poll interval like 30s / 1m
  refresh: auto
  # auto: switch from watch to poll when volumes change more often than
  # this per minute (busy clusters; 0 = never)
  autoPollRate: 120
  # false: TUI starts in write mode, CLI changes without --rw
  # (previews and confirmations stay)
  readOnly: true
  # TUI colours: default, catppuccin, dracula, nord, gruvbox, light
  # (T in the TUI cycles through them to try)
  theme: default
  # context badge colour: auto, red, green, yellow, blue, magenta, cyan,
  # orange, gray or 0-255 (auto: by name, red for names with prod/prd)
  color: auto
  # TUI: warn at 80 %, pause updates at 100 % of this much traffic per
  # session, e.g. 200MiB (0 = off)
  trafficBudget: 0
  # include in lhtx overview
  overview: true
  namespace: longhorn-system

# per kubeconfig context; keys are names or globs ("customer-*")
contexts: {}
#  homelab:
#    readOnly: false
#  "customer-*":
#    color: red
#    refresh: 30s
#    trafficBudget: 200MiB
#  kind-test:
#    overview: false
`
