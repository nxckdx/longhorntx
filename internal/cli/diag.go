package cli

import (
	"fmt"
	"os"
	"regexp"
	"time"

	"github.com/spf13/cobra"

	"github.com/nxckdx/longhorntx/internal/format"
	"github.com/nxckdx/longhorntx/internal/longhorn"
)

func diagCmd(g *globals) *cobra.Command {
	var out string
	opt := longhorn.DiagOptions{Since: 6 * time.Hour, LimitBytes: 5 << 20}
	cmd := &cobra.Command{
		Use:   "diag VOLUME",
		Short: "Write a diagnostics bundle (tar.gz) for a volume",
		Long: `Collect everything needed to analyse (or report upstream) a problem with
one volume, read-only:

  summary.txt     replicas, engine rebuild/purge status, mode changes,
                  snapshots incl. pending removals, health findings
  *.yaml          volume, engines, replicas, snapshots, involved nodes
                  (managedFields stripped)
  events.txt      events of volume, engine, replicas and PVC
  logs/           instance-manager and longhorn-manager logs from the
                  involved nodes, only lines mentioning the volume

Logs are limited by --since and --log-limit per container to keep traffic low.`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: g.completeVolumes,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := g.client()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			v, err := resolveVolume(ctx, c, args[0])
			if err != nil {
				return err
			}
			if out == "" {
				name := regexp.MustCompile(`[^A-Za-z0-9._-]+`).ReplaceAllString(v.PVC(), "_")
				if name == "" {
					name = v.Name
				}
				out = fmt.Sprintf("lhtx-diag-%s-%s.tar.gz", name, time.Now().Format("20060102-150405"))
			}
			f, err := os.OpenFile(out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err != nil {
				return err
			}
			opt.Version = Version
			res, err := c.CollectDiag(ctx, v, opt, f)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				_ = os.Remove(out)
				return err
			}
			st, _ := os.Stat(out)
			fmt.Printf("wrote %s (%s, %d files, %s received)\n", out, format.Bytes(st.Size()), len(res.Files), format.Bytes(c.Cluster.Traffic.RX()))
			for _, n := range res.Notes {
				fmt.Println("⚠ " + n)
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&out, "file", "f", "", "output file (default lhtx-diag-<pvc>-<time>.tar.gz)")
	cmd.Flags().DurationVar(&opt.Since, "since", opt.Since, "log window")
	cmd.Flags().Int64Var(&opt.LimitBytes, "log-limit", opt.LimitBytes, "max log bytes per container")
	cmd.Flags().BoolVar(&opt.NoLogs, "no-logs", false, "skip pod logs")
	cmd.Flags().BoolVar(&opt.FullLogs, "full-logs", false, "keep all log lines, not only those mentioning the volume")
	return cmd
}
