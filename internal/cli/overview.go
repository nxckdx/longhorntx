package cli

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/nxckdx/longhorntx/internal/format"
	"github.com/nxckdx/longhorntx/internal/kube"
	"github.com/nxckdx/longhorntx/internal/longhorn"
)

// clusterSummary is one line of `lhtx overview`.
type clusterSummary struct {
	Context string `json:"context"`
	Error   string `json:"error,omitempty"`
	// NotInstalled: the cluster answered but has no longhorn.io CRDs. Not
	// an error (many kubeconfigs mix clusters with and without Longhorn).
	NotInstalled bool              `json:"notInstalled,omitempty"`
	Version      string            `json:"longhornVersion,omitempty"`
	Volumes      int               `json:"volumes"`
	Attached     int               `json:"attached"`
	Degraded     int               `json:"degraded"`
	Faulted      int               `json:"faulted"`
	Rebuilding   int               `json:"rebuilding"`
	Nodes        int               `json:"nodes"`
	NodesReady   int               `json:"nodesReady"`
	Critical     int               `json:"critical"`
	Warnings     int               `json:"warnings"`
	Worst        longhorn.Severity `json:"worst"`
	Top          string            `json:"top,omitempty"` // most severe finding
	Traffic      int64             `json:"trafficBytes"`
	Took         time.Duration     `json:"took"`
}

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
				names = all
			}
			sums := make([]clusterSummary, len(names))
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

func summarizeCluster(ctx context.Context, g *globals, name string) (s clusterSummary) {
	start := time.Now()
	s.Context = name
	var c *longhorn.Client
	var err error
	if g.demo {
		c, err = g.client()
	} else {
		ko := g.kube
		ko.Context = name
		var cl *kube.Cluster
		if cl, err = kube.Connect(ko); err == nil {
			c, err = longhorn.NewClient(cl, g.namespace)
		}
	}
	if err != nil {
		s.Error = err.Error()
		return s
	}
	defer func() {
		s.Traffic = c.Cluster.Traffic.RX() + c.Cluster.Traffic.TX()
		s.Took = time.Since(start).Round(100 * time.Millisecond)
	}()
	in, err := loadDoctorInput(ctx, c)
	if err != nil {
		if notInstalled(err) {
			s.NotInstalled = true
			return s
		}
		s.Error = shortError(err)
		return s
	}
	s.Version, _ = c.LonghornVersion(ctx)
	s.Volumes, s.Nodes = len(in.Volumes), len(in.Nodes)
	for _, v := range in.Volumes {
		if v.Status.State == "attached" {
			s.Attached++
		}
		switch v.Status.Robustness {
		case "degraded":
			s.Degraded++
		case "faulted":
			s.Faulted++
		}
	}
	s.Rebuilding = len(longhorn.RebuildingVolumes(in.Replicas))
	for _, n := range in.Nodes {
		if n.Ready() {
			s.NodesReady++
		}
	}
	fs := longhorn.Diagnose(in)
	s.Worst = longhorn.Worst(fs)
	sort.SliceStable(fs, func(i, j int) bool { return fs[i].Severity > fs[j].Severity })
	for _, f := range fs {
		switch f.Severity {
		case longhorn.SevCritical:
			s.Critical++
		case longhorn.SevWarning:
			s.Warnings++
		}
	}
	if len(fs) > 0 && fs[0].Severity > longhorn.SevInfo {
		s.Top = format.Short(fs[0].Object+": "+fs[0].Message, 80)
	}
	return s
}

// notInstalled: listing longhorn.io volumes gives 404 when the CRDs are
// missing (a missing namespace would give an empty list instead).
func notInstalled(err error) bool {
	return apierrors.IsNotFound(err) ||
		strings.Contains(err.Error(), "the server could not find the requested resource")
}

// shortError trims client-go's verbose errors for a one-line table.
func shortError(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "context deadline exceeded"):
		return "timeout"
	}
	if len(msg) > 120 {
		msg = msg[:117] + "…"
	}
	return msg
}

func printOverview(sums []clusterSummary, wide bool) {
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
