package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/nxckdx/longhorntx/internal/format"
	"github.com/nxckdx/longhorntx/internal/longhorn"
)

func rebuildsCmd(g *globals) *cobra.Command {
	var watchFlag bool
	cmd := &cobra.Command{
		Use:     "rebuilds",
		Aliases: []string{"activity", "act"},
		Short:   "Show running rebuilds, purges, restores and backups",
		Long: `Show long-running operations across the cluster.

Without -w: one-shot view of rebuilds (and purges/restores of the affected
volumes). Engines are only read for volumes that are rebuilding.

With -w: live view with speed and ETA. Additionally watches backups "from
now on": backups that are already running show up with their next progress
update.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := g.checkOutput(); err != nil {
				return err
			}
			c, err := g.client()
			if err != nil {
				return err
			}
			defer g.reportTraffic(c)
			if watchFlag {
				return watchActivity(cmd.Context(), c, g.output == "json")
			}
			acts, err := oneShotActivity(cmd.Context(), c)
			if err != nil {
				return err
			}
			if g.output == "json" {
				return writeJSON(os.Stdout, acts)
			}
			if len(acts) == 0 {
				fmt.Println("no rebuilds running")
				return nil
			}
			return printActivities(os.Stdout, acts, nil)
		},
	}
	cmd.Flags().BoolVarP(&watchFlag, "watch", "w", false, "keep watching (live view)")
	return cmd
}

func oneShotActivity(ctx context.Context, c *longhorn.Client) ([]longhorn.Activity, error) {
	var st longhorn.State
	var err error
	if st.Volumes, err = c.Volumes(ctx); err != nil {
		return nil, err
	}
	if st.Replicas, err = c.Replicas(ctx, ""); err != nil {
		return nil, err
	}
	if st.Nodes, err = c.Nodes(ctx); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, r := range st.Replicas {
		if r.LikelyRebuilding() && !seen[r.Spec.VolumeName] {
			seen[r.Spec.VolumeName] = true
			engs, err := c.Engines(ctx, r.Spec.VolumeName)
			if err != nil {
				return nil, err
			}
			st.Engines = append(st.Engines, engs...)
		}
	}
	return longhorn.BuildActivities(longhorn.Build(st), st.Engines, nil), nil
}

func watchActivity(ctx context.Context, c *longhorn.Client, jsonOut bool) error {
	s := longhorn.NewStore(c)
	s.WatchBackups = true
	s.Start(ctx)
	defer s.Stop()
	if err := s.WaitSynced(ctx, 2*time.Minute); err != nil {
		return err
	}
	rates := longhorn.NewRateTracker()
	tty := !jsonOut && isTTY()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	last := "\x00"
	for {
		st := s.State()
		acts := longhorn.BuildActivities(longhorn.Build(st), st.Engines, s.ActiveBackups())
		now := time.Now()
		keep := map[string]bool{}
		for _, a := range acts {
			if a.Progress >= 0 && !a.Done {
				keep[a.Key()] = true
				rates.Observe(a.Key(), a.Progress, a.Total, now)
			}
		}
		rates.Forget(keep)

		// Only print when the content changed (ETA ticks count as change in
		// a terminal while something runs).
		var body strings.Builder
		if jsonOut {
			_ = writeJSON(&body, acts)
		} else if len(acts) > 0 {
			_ = printActivities(&body, acts, rates)
		}
		changed := body.String() != last || (tty && len(acts) > 0)
		last = body.String()
		if !changed {
			// nothing new
		} else if jsonOut {
			fmt.Print(body.String())
		} else {
			var b strings.Builder
			fmt.Fprintf(&b, "%s · %s · ↓ %s\n\n", c.Cluster.Context, now.Format("15:04:05"), format.Bytes(c.Cluster.Traffic.RX()))
			if len(acts) == 0 {
				b.WriteString("no rebuilds, purges or backups running\n")
			} else {
				b.WriteString(body.String())
			}
			if err := s.Err(); err != nil {
				fmt.Fprintf(&b, "\n⚠ %v\n", err)
			}
			if tty {
				fmt.Print("\033[H\033[2J")
			}
			fmt.Print(b.String())
			if !tty {
				fmt.Println("---")
			}
		}

		// redraw on change, and once per second while something runs (ETA)
		select {
		case <-ctx.Done():
			return nil
		case <-s.Changed():
		case <-tick.C:
			if len(acts) == 0 {
				select {
				case <-ctx.Done():
					return nil
				case <-s.Changed():
				}
			}
		}
	}
}

func printActivities(w io.Writer, acts []longhorn.Activity, rates *longhorn.RateTracker) error {
	tw := newTab(w)
	fmt.Fprintln(tw, "KIND\tPVC\tTARGET\tNODE\tPROGRESS\tSPEED\tETA\tSTATE")
	for _, a := range acts {
		prog := "?"
		if a.Progress >= 0 {
			prog = fmt.Sprintf("%s %3d%%", textBar(a.Progress, 12), a.Progress)
		}
		speed, eta := "-", "-"
		if rates != nil && a.Progress >= 0 && !a.Done {
			if r := rates.Get(a.Key(), a.Progress, a.Total); r.Valid {
				speed, eta = format.Rate(r.BytesPerSec), format.Duration(r.ETA)
			}
		}
		state := dash(a.State)
		if a.Error != "" {
			state += ": " + a.Error
		}
		target := a.Target
		if i := strings.LastIndex(target, "-r-"); i >= 0 {
			target = "replica " + target[i+1:]
		}
		pvc := a.PVC
		if pvc == "" {
			pvc = a.Volume
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", a.Kind, pvc, target, dash(a.Node), prog, speed, eta, state)
	}
	return tw.Flush()
}

func textBar(pct, w int) string {
	full := pct * w / 100
	return strings.Repeat("█", full) + strings.Repeat("░", w-full)
}

func isTTY() bool { return term.IsTerminal(int(os.Stdout.Fd())) }
