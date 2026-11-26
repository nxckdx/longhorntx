package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/nxckdx/longhorntx/internal/format"
	"github.com/nxckdx/longhorntx/internal/longhorn"
)

func historyCmd(g *globals) *cobra.Command {
	var since time.Duration
	var kinds []string
	var bad bool
	cmd := &cobra.Command{
		Use:   "history [VOLUME]",
		Short: "What happened to a volume (or notable events cluster-wide) – kept by the lhtx agent",
		Long: `Longhorn keeps no history (its events expire after an hour). The lhtx agent
records rebuilds and how long they took, failed replicas, robustness changes,
completed and failed backups, expansions and engine upgrades: the last 50 per
volume, and the last 300 notable ones (failures, degradations, finished
rebuilds) cluster-wide. Needs the agent (lhtx agent --help).

  lhtx history                      # notable events in the cluster
  lhtx history --bad --since 7d     # what went wrong this week
  lhtx history shop/db-data         # everything about one volume`,
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: g.completeVolumes,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := g.checkOutput(); err != nil {
				return err
			}
			c, err := g.client()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			name, cluster := longhorn.ClusterHistory, true
			if len(args) == 1 {
				v, err := resolveVolume(ctx, c, args[0])
				if err != nil {
					return err
				}
				name, cluster = v.Name, false
			}
			ev, err := c.History(ctx, name)
			if errors.Is(err, longhorn.ErrNoHistory) && !cluster {
				fmt.Println("nothing recorded for this volume yet (the lhtx agent records changes from when it runs)")
				return nil
			}
			if err != nil {
				return err
			}
			var out []longhorn.HistoryEvent
			for _, e := range ev {
				if since > 0 && time.Since(e.At) > since {
					continue
				}
				if bad && !e.Bad() {
					continue
				}
				if len(kinds) > 0 && !contains(kinds, e.Kind) {
					continue
				}
				out = append(out, e)
			}
			if g.output == "json" {
				return writeJSON(os.Stdout, out)
			}
			if len(out) == 0 {
				fmt.Println("no events")
				return nil
			}
			w := newTab(os.Stdout)
			hdr := "WHEN\tKIND\tWHAT\tTOOK"
			if cluster {
				hdr = "WHEN\tPVC / VOLUME\tKIND\tWHAT\tTOOK"
			}
			fmt.Fprintln(w, hdr)
			for _, e := range out {
				when := e.At.Local().Format("2006-01-02 15:04") + " (" + format.Age(e.At.Format(time.RFC3339), time.Now()) + ")"
				took := "-"
				if e.Took > 0 {
					took = e.Took.String()
				}
				kind := e.Kind
				if e.Bad() {
					kind = "✖ " + kind
				}
				if cluster {
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", when, oneLine(dash(firstNonEmpty(e.PVC, e.Volume)), 48), kind, e.Text, took) // full name: -o json
				} else {
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", when, kind, e.Text, took)
				}
			}
			return w.Flush()
		},
	}
	cmd.Flags().DurationVar(&since, "since", 0, "only events of the last DURATION (e.g. 24h, 168h)")
	cmd.Flags().StringSliceVar(&kinds, "kind", nil, "only these kinds: "+strings.Join([]string{longhorn.HistRobustness, longhorn.HistRebuildStart,
		longhorn.HistRebuildDone, longhorn.HistReplicaFailed, longhorn.HistBackup, longhorn.HistBackupFailed, longhorn.HistExpanded, longhorn.HistEngine}, ", "))
	cmd.Flags().BoolVar(&bad, "bad", false, "only failures and degradations")
	return cmd
}

func contains(s []string, x string) bool {
	for _, v := range s {
		if v == x {
			return true
		}
	}
	return false
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
