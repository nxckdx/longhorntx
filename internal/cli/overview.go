package cli

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/nxckdx/longhorntx/internal/format"
	"github.com/nxckdx/longhorntx/internal/longhorn"
	"github.com/nxckdx/longhorntx/internal/overview"
)

func overviewCmd(g *globals) *cobra.Command {
	var contexts []string
	var timeout time.Duration
	var parallel int
	var ignoreNotInstalled bool
	cmd := &cobra.Command{
		Use:     "overview",
		Aliases: []string{"clusters"},
		Short:   "Health of Longhorn in all kubeconfig contexts at a glance",
		Long: `Runs the doctor checks against several kubeconfig contexts at once (all by
default, or --contexts a,b) and prints one line per cluster: volumes,
degraded/faulted, rebuilds, nodes ready, the worst finding. Clusters
without Longhorn show "no Longhorn" and do not count for the exit code
(--ignore-not-installed hides them); unreachable clusters are errors.

Traffic: one LIST of volumes, replicas and nodes per cluster (like doctor).

Contexts with "overview: false" in the config file are skipped unless named
in --contexts.

Exit codes like doctor (worst of all clusters): 0 ok, 1 warning, 2 critical,
3 any cluster failed.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := g.checkOutput(); err != nil {
				return err
			}
			names := contexts
			if g.demo {
				names = []string{"demo"}
			} else if len(names) == 0 {
				all, _, err := g.kube.Contexts()
				if err != nil {
					return err
				}
				names = nil
				for _, n := range all { // overview: false in the config
					if g.cfg == nil || g.cfg.For(n).Overview {
						names = append(names, n)
					}
				}
			}
			sums := make([]overview.Summary, len(names))
			sem := make(chan struct{}, max(parallel, 1))
			var wg sync.WaitGroup
			for i, name := range names {
				wg.Add(1)
				go func() {
					defer wg.Done()
					sem <- struct{}{}
					defer func() { <-sem }()
					ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
					defer cancel()
					sums[i] = summarizeCluster(ctx, g, name)
				}()
			}
			wg.Wait()
			if ignoreNotInstalled {
				kept := sums[:0]
				for _, s := range sums {
					if !s.NotInstalled {
						kept = append(kept, s)
					}
				}
				sums = kept
			}

			if g.output == "json" {
				if err := writeJSON(os.Stdout, sums); err != nil {
					return err
				}
			} else {
				printOverview(sums, g.output == "wide")
			}
			worst, failed := longhorn.SevInfo, false
			for _, s := range sums {
				if s.Error != "" {
					failed = true
				}
				worst = max(worst, s.Worst)
			}
			switch {
			case failed:
				return exitError{3}
			case worst == longhorn.SevCritical:
				return exitError{2}
			case worst == longhorn.SevWarning:
				return exitError{1}
			}
			return nil
		},
	}
	cmd.Flags().StringSliceVar(&contexts, "contexts", nil, "kubeconfig contexts to check (default: all)")
	cmd.Flags().DurationVar(&timeout, "timeout", 45*time.Second, "per cluster")
	cmd.Flags().IntVar(&parallel, "parallel", 4, "clusters checked at the same time")
	cmd.Flags().BoolVar(&ignoreNotInstalled, "ignore-not-installed", false, "hide clusters without Longhorn")
	_ = cmd.RegisterFlagCompletionFunc("contexts", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		names, _, _ := g.kube.Contexts()
		return names, cobra.ShellCompDirectiveNoFileComp
	})
	return cmd
}

func summarizeCluster(ctx context.Context, g *globals, name string) overview.Summary {
	if g.demo {
		c, err := g.client()
		if err != nil {
			return overview.Summary{Context: name, Error: err.Error()}
		}
		return overview.Summarize(ctx, c)
	}
	return overview.Check(ctx, g.kube, name, g.namespaceFor(name))
}

func printOverview(sums []overview.Summary, wide bool) {
	w := newTab(os.Stdout)
	hdr := "CONTEXT\tSTATUS\tLONGHORN\tVOLUMES\tATTACHED\tDEGRADED\tFAULTED\tREBUILDING\tNODES\tFINDINGS"
	if wide {
		hdr += "\tTRAFFIC\tTOOK"
	}
	fmt.Fprintln(w, hdr)
	for _, s := range sums {
		if s.Error != "" {
			fmt.Fprintf(w, "%s\tERROR\t%s\n", s.Context, s.Error)
			continue
		}
		if s.NotInstalled {
			fmt.Fprintf(w, "%s\tno Longhorn\t-\n", s.Context)
			continue
		}
		status := "OK"
		if s.Worst > longhorn.SevInfo {
			status = s.Worst.String()
		}
		findings := "-"
		if s.Critical+s.Warnings > 0 {
			findings = fmt.Sprintf("%d crit, %d warn – %s", s.Critical, s.Warnings, s.Top)
		}
		line := fmt.Sprintf("%s\t%s\t%s\t%d\t%d\t%d\t%d\t%d\t%d/%d\t%s", s.Context, status, dash(s.Version),
			s.Volumes, s.Attached, s.Degraded, s.Faulted, s.Rebuilding, s.NodesReady, s.Nodes, findings)
		if wide {
			line += fmt.Sprintf("\t%s\t%s", format.Bytes(s.Traffic), s.Took)
		}
		fmt.Fprintln(w, line)
	}
	_ = w.Flush()
}
