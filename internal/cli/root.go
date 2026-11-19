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
}

func (g *globals) refreshMode() (time.Duration, error) { return longhorn.ParseRefresh(g.refresh) }

func (g *globals) client() (*longhorn.Client, error) {
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
			return tui.Run(cmd.Context(), tui.Options{Kube: g.kube, Namespace: g.namespace, Version: Version, RW: g.rw, Refresh: r})
		},
	}
	f := root.PersistentFlags()
	f.StringVar(&g.kube.Kubeconfig, "kubeconfig", "", "path to the kubeconfig file")
	f.StringVar(&g.kube.Context, "context", "", "kubeconfig context to use")
	f.StringVarP(&g.namespace, "namespace", "n", longhorn.DefaultNamespace, "Longhorn namespace")
	f.StringVarP(&g.output, "output", "o", "table", "output format: table|wide|json")
	f.BoolVarP(&g.verbose, "verbose", "v", false, "print network traffic used to stderr")
	f.BoolVar(&g.rw, "rw", false, "allow changes (lhtx is read-only by default)")
	f.BoolVarP(&g.yes, "yes", "y", false, "do not ask for confirmation (with --rw)")
	f.StringVar(&g.dryRunFlag, "dry-run", "", "show (client) or validate on the server (server) without changing anything")
	f.Lookup("dry-run").NoOptDefVal = "client"
	defRefresh := os.Getenv("LHTX_REFRESH")
	if defRefresh == "" {
		defRefresh = "auto"
	}
	f.StringVar(&g.refresh, "refresh", defRefresh, "volume updates: auto, watch (live) or a poll interval like 30s (env LHTX_REFRESH)")

	root.AddCommand(
		volumesCmd(g), replicasCmd(g), nodesCmd(g), diskCmd(g), rebuildsCmd(g),
		snapshotsCmd(g), backupsCmd(g), eventsCmd(g), jobsCmd(g), settingsCmd(g), targetsCmd(g), orphansCmd(g),
		measureCmd(g), doctorCmd(g), drainCheckCmd(g), diagCmd(g), contextsCmd(g),
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
