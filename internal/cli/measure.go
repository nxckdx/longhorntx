package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/watch"

	"github.com/nxckdx/longhorntx/internal/format"
	"github.com/nxckdx/longhorntx/internal/longhorn"
)

type measurement struct {
	Resource    string        `json:"resource"`
	Items       int           `json:"items"`
	ListTime    time.Duration `json:"listTimeNs"`
	ListBytes   int64         `json:"listBytes"`
	WatchEvents int           `json:"watchEvents"`
	WatchBytes  int64         `json:"watchBytes"`
	Error       string        `json:"error,omitempty"`
}

func measureCmd(g *globals) *cobra.Command {
	var dur time.Duration
	var withBackups, diff bool
	cmd := &cobra.Command{
		Use:   "measure",
		Short: "Measure startup time and watch traffic per resource",
		Long: `Measure what lhtx (and any Kubernetes client) costs on this cluster.

Phase 1 lists each resource once, the way the TUI does at startup, and
records time and bytes on the wire (TLS, gzip included; managedFields
included, unlike kubectl output).
Phase 2 watches every resource for --duration, starting "now" (no initial
list), and counts events and bytes.

Each resource uses its own connection so bytes can be attributed exactly.
The backup LIST is skipped unless --backups is set (it can be several MB);
backups are always included in the watch phase.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := g.checkOutput(); err != nil {
				return err
			}
			ctx := cmd.Context()
			res := []string{"volumes", "replicas", "nodes", "engines", "settings", "snapshots", "backups"}
			ms := make([]*measurement, len(res))
			stats := make([]*longhorn.FieldStats, len(res))
			rvs := make([]string, len(res))
			clients := make([]*longhorn.Client, len(res))

			fmt.Fprintf(os.Stderr, "phase 1: listing %s …\n", strings.Join(res, ", "))
			for i, r := range res {
				ms[i] = &measurement{Resource: r}
				c, err := g.client() // own connection + counter per resource
				if err != nil {
					return err
				}
				clients[i] = c
				if r == "backups" && !withBackups {
					// only the resourceVersion, for the watch
					rvs[i] = latestRV(ctx, c, r)
					ms[i].ListBytes = -1
					continue
				}
				// warm up the connection so the TLS handshake is not counted
				_, _ = c.LatestResourceVersion(ctx, "settings")
				base := c.Cluster.Traffic.RX()
				start := time.Now()
				n, rv, err := c.ListCount(ctx, r)
				ms[i].ListTime = time.Since(start)
				ms[i].ListBytes = c.Cluster.Traffic.RX() - base
				ms[i].Items, rvs[i] = n, rv
				if err != nil {
					ms[i].Error = err.Error()
				}
			}

			fmt.Fprintf(os.Stderr, "phase 2: watching for %s (ctrl+c ends early) …\n", dur)
			wctx, cancel := context.WithTimeout(ctx, dur)
			defer cancel()
			var wg sync.WaitGroup
			for i := range res {
				if rvs[i] == "" {
					continue
				}
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					c := clients[i]
					base := c.Cluster.Traffic.RX()
					w, err := c.WatchFrom(wctx, res[i], rvs[i])
					if err != nil {
						ms[i].Error = err.Error()
						return
					}
					defer w.Stop()
					last := map[string]map[string]any{} // for --diff
					stats[i] = &longhorn.FieldStats{}
					for {
						select {
						case <-wctx.Done():
							ms[i].WatchBytes = c.Cluster.Traffic.RX() - base
							return
						case ev, ok := <-w.ResultChan():
							if !ok {
								ms[i].WatchBytes = c.Cluster.Traffic.RX() - base
								return
							}
							if ev.Type != watch.Bookmark {
								ms[i].WatchEvents++
							}
							if u, ok := ev.Object.(*unstructured.Unstructured); diff && ok {
								switch ev.Type {
								case watch.Modified:
									if prev := last[u.GetName()]; prev != nil {
										stats[i].Add(longhorn.ChangedPaths(prev, u.Object))
									}
									last[u.GetName()] = u.Object
								case watch.Added:
									last[u.GetName()] = u.Object
								case watch.Deleted:
									delete(last, u.GetName())
								}
							}
						}
					}
				}(i)
			}
			start := time.Now()
			wg.Wait()
			elapsed := time.Since(start)

			if g.output == "json" {
				out := map[string]any{"context": clients[0].Cluster.Context, "watchSeconds": elapsed.Seconds(), "resources": ms}
				if diff {
					d := map[string]any{}
					for i, r := range res {
						if stats[i] != nil && stats[i].Updates > 0 {
							d[r] = stats[i]
						}
					}
					out["changedFields"] = d
				}
				return writeJSON(os.Stdout, out)
			}
			perMin := func(b int64) string {
				return format.Bytes(int64(float64(b) / elapsed.Minutes()))
			}
			fmt.Printf("\ncontext %s · watched %s\n\n", clients[0].Cluster.Context, elapsed.Round(time.Second))
			w := newTab(os.Stdout)
			fmt.Fprintln(w, "RESOURCE\tITEMS\tLIST TIME\tLIST BYTES\tWATCH EVENTS\tWATCH BYTES\tPER MINUTE")
			var startBytes, idle int64
			var startTime time.Duration
			for _, m := range ms {
				lb, lt, items := "skipped", "-", "-"
				if m.ListBytes >= 0 {
					lb, lt, items = format.Bytes(m.ListBytes), m.ListTime.Round(time.Millisecond).String(), fmt.Sprint(m.Items)
				}
				line := fmt.Sprintf("%s\t%s\t%s\t%s\t%d\t%s\t%s", m.Resource, items, lt, lb, m.WatchEvents, format.Bytes(m.WatchBytes), perMin(m.WatchBytes))
				if m.Error != "" {
					line += "\t⚠ " + m.Error
				}
				fmt.Fprintln(w, line)
				switch m.Resource {
				case "volumes", "replicas", "nodes":
					startBytes += m.ListBytes
					startTime = max(startTime, m.ListTime)
					idle += m.WatchBytes
				case "backups":
					idle += m.WatchBytes
				}
			}
			if err := w.Flush(); err != nil {
				return err
			}
			fmt.Printf("\nlhtx TUI estimate: startup %s (lists of volumes, replicas, nodes; longest %s),\n", format.Bytes(startBytes), startTime.Round(time.Millisecond))
			fmt.Printf("idle %s/min (volume list). Engines add up to %s/min while watched\n", perMin(idle), perMin(engineBytes(ms)))
			fmt.Println("(lhtx only watches engines for an open detail view or a rebuilding volume).")
			if diff {
				fmt.Println("\nFields changed per update (compared with the previous version of the same object;")
				fmt.Println("objects need at least two updates during the watch to count):")
				for i, r := range res {
					if stats[i] == nil || stats[i].Updates == 0 {
						continue
					}
					fmt.Printf("\n%s – %d updates compared\n", r, stats[i].Updates)
					for _, l := range stats[i].Top(12) {
						fmt.Println("  " + l)
					}
				}
			}
			return nil
		},
	}
	cmd.Flags().DurationVarP(&dur, "duration", "d", time.Minute, "how long to watch")
	cmd.Flags().BoolVar(&withBackups, "backups", false, "also LIST all backups (can be several MB)")
	cmd.Flags().BoolVar(&diff, "diff", false, "show which fields change in watch events (compares consecutive versions)")
	return cmd
}

func engineBytes(ms []*measurement) int64 {
	for _, m := range ms {
		if m.Resource == "engines" {
			return m.WatchBytes
		}
	}
	return 0
}

// latestRV returns a collection's current resourceVersion via LIST limit=1.
func latestRV(ctx context.Context, c *longhorn.Client, res string) string {
	rv, err := c.LatestResourceVersion(ctx, res)
	if err != nil {
		return ""
	}
	return rv
}
