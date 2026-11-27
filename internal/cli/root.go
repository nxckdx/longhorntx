// Package cli implements the lhtx command line.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/nxckdx/longhorntx/internal/config"
	"github.com/nxckdx/longhorntx/internal/demo"
	"github.com/nxckdx/longhorntx/internal/kube"
	"github.com/nxckdx/longhorntx/internal/longhorn"
	"github.com/nxckdx/longhorntx/internal/tui"
)

// Version is set via -ldflags.
var Version = "dev"

type globals struct {
	kube      kube.Options
	namespace string
	output    string
	verbose   bool

	rw         bool
	yes        bool
	dryRunFlag string
	refresh    string
	demo       bool
	theme      string

	cfg *config.File
	// explicit command line values (they beat the config file)
	rwSet, refreshSet, namespaceSet bool
}

// loadConfig reads the config file and applies the settings of the
// selected context to everything not given on the command line.
func (g *globals) loadConfig(cmd *cobra.Command) error {
	config.ValidRefresh = func(s string) error { _, err := longhorn.ParseRefresh(s); return err }
	config.ValidTheme = validTheme
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		return err
	}
	g.cfg = cfg
	tui.RegisterThemes(cfg.Themes)
	f := cmd.Flags()
	g.rwSet = f.Changed("rw")
	g.refreshSet = f.Changed("refresh") || os.Getenv("LHTX_REFRESH") != ""
	g.namespaceSet = f.Changed("namespace")
	if _, own := cfg.Themes[g.theme]; g.theme != "" && !own {
		if err := validTheme(g.theme); err != nil {
			return err
		}
	}
	eff := cfg.For(g.contextName())
	if !g.rwSet && !eff.ReadOnly {
		g.rw = true
	}
	if !g.refreshSet {
		g.refresh = eff.Refresh
	}
	if !g.namespaceSet {
		g.namespace = eff.Namespace
	}
	return nil
}

func validTheme(s string) error {
	for _, n := range tui.BuiltinThemeNames() {
		if s == n || s == "" {
			return nil
		}
	}
	return fmt.Errorf("unknown theme %q (%s, or one under themes: in the config file)", s, strings.Join(tui.BuiltinThemeNames(), ", "))
}

// contextName is the selected kubeconfig context (local, no traffic).
func (g *globals) contextName() string {
	if g.demo {
		return demo.Context
	}
	if g.kube.Context != "" {
		return g.kube.Context
	}
	_, cur, _ := g.kube.Contexts()
	return cur
}

// namespaceFor gives the Longhorn namespace of another context (overview).
func (g *globals) namespaceFor(kctx string) string {
	if g.namespaceSet || g.cfg == nil {
		return g.namespace
	}
	return g.cfg.For(kctx).Namespace
}

func (g *globals) refreshMode() (time.Duration, error) { return longhorn.ParseRefresh(g.refresh) }

func (g *globals) client() (*longhorn.Client, error) {
	if g.demo {
		return demo.NewClient(context.Background()), nil
	}
	cl, err := kube.Connect(g.kube)
	if err != nil {
		return nil, err
	}
	return longhorn.NewClient(cl, g.namespace)
}

// reportTraffic prints wire bytes to stderr with -v.
func (g *globals) reportTraffic(c *longhorn.Client) {
	if g.verbose {
		t := c.Cluster.Traffic
		fmt.Fprintf(os.Stderr, "traffic: %d B received, %d B sent\n", t.RX(), t.TX())
	}
}

func NewRoot() *cobra.Command {
	g := &globals{}
	name := "lhtx"
	if strings.HasPrefix(filepath.Base(os.Args[0]), "kubectl-") {
		name = "kubectl lhtx"
	}
	root := &cobra.Command{
		Use:   name,
		Short: "Low-traffic terminal UI and CLI for Longhorn",
		Long: `lhtx shows Longhorn volumes, replicas and rebuilds using only the Kubernetes API.
Without a subcommand it starts the interactive TUI.`,
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			r, err := g.refreshMode()
			if err != nil {
				return err
			}
			return tui.Run(cmd.Context(), tui.Options{Kube: g.kube, Namespace: g.namespace, Version: Version, RW: g.rw, Refresh: r, Demo: g.demo,
				Config: g.cfg, RWSet: g.rwSet, RefreshSet: g.refreshSet, NamespaceSet: g.namespaceSet, Theme: g.theme})
		},
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			switch {
			case cmd.Name() == "config" || (cmd.Parent() != nil && cmd.Parent().Name() == "config"):
				return nil // must work with a broken file (loads it itself)
			case cmd.Name() == cobra.ShellCompRequestCmd || cmd.Name() == cobra.ShellCompNoDescRequestCmd ||
				cmd.Name() == "completion" || (cmd.Parent() != nil && cmd.Parent().Name() == "completion"):
				_ = g.loadConfig(cmd) // a broken file must not break the shell
				return nil
			}
			return g.loadConfig(cmd)
		},
	}
	f := root.PersistentFlags()
	f.StringVar(&g.kube.Kubeconfig, "kubeconfig", "", "path to the kubeconfig file")
	f.StringVar(&g.kube.Context, "context", "", "kubeconfig context to use")
	f.StringVarP(&g.namespace, "namespace", "n", longhorn.DefaultNamespace, "Longhorn namespace")
	f.StringVarP(&g.output, "output", "o", "table", "output format: table|wide|json")
	f.BoolVarP(&g.verbose, "verbose", "v", false, "print network traffic used to stderr")
	f.BoolVar(&g.demo, "demo", false, "use a simulated in-memory cluster with made-up data (try lhtx without a cluster)")
	f.BoolVar(&g.rw, "rw", false, "allow changes (lhtx is read-only by default)")
	f.BoolVarP(&g.yes, "yes", "y", false, "do not ask for confirmation (with --rw)")
	f.StringVar(&g.dryRunFlag, "dry-run", "", "show (client) or validate on the server (server) without changing anything")
	f.Lookup("dry-run").NoOptDefVal = "client"
	defRefresh := os.Getenv("LHTX_REFRESH")
	if defRefresh == "" {
		defRefresh = "auto"
	}
	f.StringVar(&g.theme, "theme", "", "TUI colours: "+strings.Join(tui.BuiltinThemeNames(), ", ")+" or one from the config file")
	_ = root.RegisterFlagCompletionFunc("theme", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return tui.ThemeNames(), cobra.ShellCompDirectiveNoFileComp
	})
	f.StringVar(&g.refresh, "refresh", defRefresh, "volume updates: auto, watch (live), agent (digests from the lhtx agent) or a poll interval like 30s (env LHTX_REFRESH)")

	root.AddCommand(
		volumesCmd(g), replicasCmd(g), nodesCmd(g), diskCmd(g), rebuildsCmd(g),
		snapshotsCmd(g), backupsCmd(g), backupVolumesCmd(g), matrixCmd(g), eventsCmd(g), jobsCmd(g), settingsCmd(g), targetsCmd(g), orphansCmd(g), engineImagesCmd(g),
		measureCmd(g), doctorCmd(g), overviewCmd(g), drainCheckCmd(g), diagCmd(g), contextsCmd(g),
		versionCmd(g), updateCmd(g), configCmd(g), agentCmd(g), historyCmd(g), capacityCmd(g), mountsCmd(g), rwxCmd(g), copyCmd(),
	)
	root.AddCommand(writeCmds(g)...)
	registerCompletions(root, g)
	return root
}

func Execute(ctx context.Context) int {
	if err := NewRoot().ExecuteContext(ctx); err != nil {
		var ee exitError
		if errors.As(err, &ee) {
			return ee.code
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return 0
}

func (g *globals) checkOutput() error {
	switch g.output {
	case "table", "wide", "json":
		return nil
	}
	return fmt.Errorf("unknown output format %q (table|wide|json)", g.output)
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func newTab(w io.Writer) *tabwriter.Writer {
	return tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
