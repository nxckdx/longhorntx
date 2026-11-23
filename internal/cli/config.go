package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/nxckdx/longhorntx/internal/config"
	"github.com/nxckdx/longhorntx/internal/format"
	"github.com/nxckdx/longhorntx/internal/tui"
)

func configCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Show the effective settings for a context (config file + flags)",
		Long: `lhtx reads ` + "`~/.config/lhtx/config.yaml`" + ` ($XDG_CONFIG_HOME, or LHTX_CONFIG):
defaults plus overrides per kubeconfig context (name or glob). Settings:
refresh, readOnly, color, trafficBudget, overview, namespace.

Precedence: flag/env > contexts > defaults > built-in.

  lhtx config                     effective values for the current context
  lhtx config --context NAME      … for another context
  lhtx config init                write a commented example file`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			p := config.DefaultPath()
			if err := g.loadConfig(cmd); err != nil {
				return err
			}
			if g.cfg.Path == "" {
				fmt.Printf("config file: %s (not present, built-in defaults; lhtx config init creates one)\n", p)
			} else {
				fmt.Printf("config file: %s\n", p)
			}
			kctx := g.contextName()
			eff := g.cfg.For(kctx)
			src := func(key string, set bool) string {
				if set {
					return "flag/env"
				}
				return eff.Source[key]
			}
			budget := "off"
			if eff.TrafficBudget > 0 {
				budget = format.Bytes(eff.TrafficBudget)
			}
			color := eff.Color
			if color == "" {
				color = "auto"
			}
			fmt.Printf("context:     %s\n\n", kctx)
			w := newTab(os.Stdout)
			fmt.Fprintln(w, "SETTING\tVALUE\tFROM")
			fmt.Fprintf(w, "refresh\t%s\t%s\n", g.refresh, src("refresh", g.refreshSet))
			fmt.Fprintf(w, "autoPollRate\t%d/min\t%s\n", eff.AutoPollRate, eff.Source["autoPollRate"])
			fmt.Fprintf(w, "readOnly\t%v\t%s\n", !g.rw, src("readOnly", g.rwSet))
			fmt.Fprintf(w, "namespace\t%s\t%s\n", g.namespace, src("namespace", g.namespaceSet))
			fmt.Fprintf(w, "color\t%s\t%s\n", color, eff.Source["color"])
			theme, tsrc := eff.Theme, eff.Source["theme"]
			if g.theme != "" {
				theme, tsrc = g.theme, "flag/env"
			}
			if theme == "" {
				theme = "default"
			}
			fmt.Fprintf(w, "theme\t%s\t%s\n", theme, tsrc)
			fmt.Fprintf(w, "trafficBudget\t%s\t%s\n", budget, eff.Source["trafficBudget"])
			fmt.Fprintf(w, "overview\t%v\t%s\n", eff.Overview, eff.Source["overview"])
			return w.Flush()
		},
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "init",
		Short: "Write a commented example config file (never overwrites)",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			p := config.DefaultPath()
			if p == "" {
				return fmt.Errorf("no home directory: set LHTX_CONFIG")
			}
			if _, err := os.Stat(p); err == nil {
				return fmt.Errorf("%s exists – edit it instead", p)
			} else if !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(p, []byte(config.Example), 0o644); err != nil {
				return err
			}
			fmt.Println("wrote", p)
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "themes",
		Short: "Show all themes with their colours and how to define your own",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := g.loadConfig(cmd); err != nil {
				return err
			}
			cur := g.cfg.For(g.contextName()).Theme
			if g.theme != "" {
				cur = g.theme
			}
			if cur == "" {
				cur = "default"
			}
			w := newTab(os.Stdout)
			for _, n := range tui.ThemeNames() {
				mark := "  "
				if n == cur {
					mark = "▶ "
				}
				own := ""
				if _, ok := g.cfg.Themes[n]; ok {
					own = " (config)"
				}
				fmt.Fprintf(w, "%s%s%s\t%s\n", mark, n, own, tui.ThemeSwatch(n))
			}
			if err := w.Flush(); err != nil {
				return err
			}
			fmt.Print(themeHelp)
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "path",
		Short: "Print the config file path",
		Args:  cobra.NoArgs,
		Run:   func(*cobra.Command, []string) { fmt.Println(config.DefaultPath()) },
	})
	return cmd
}

const themeHelp = `
Select: theme: NAME in the config file (defaults or per context), --theme,
or T in the TUI to try them. Own themes or tweaks go under themes: – each
starts from base (a built-in) and overrides single colours:

  themes:
    mine:
      base: nord
      accent: "#ff79c6"
      warn: "214"          # #rrggbb, #rgb or 0-255
    default:               # tweak a built-in: same name, no base needed
      ok: "#00ff87"

Colours: dim faint ok warn error info accent headerBg selectedBg barFg
tabBg tabFg badgeFg prodBg border, palette (list of context badge colours).
`
