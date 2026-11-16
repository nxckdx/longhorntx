package cli

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/nxckdx/longhorntx/internal/format"
	"github.com/nxckdx/longhorntx/internal/longhorn"
)

func snapshotsCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:     "snapshots VOLUME",
		Aliases: []string{"snapshot", "snap"},
		Short:   "List the snapshots of a volume",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := g.checkOutput(); err != nil {
				return err
			}
			c, err := g.client()
			if err != nil {
				return err
			}
			defer g.reportTraffic(c)
			v, err := resolveVolume(cmd.Context(), c, args[0])
			if err != nil {
				return err
			}
			snaps, err := c.Snapshots(cmd.Context(), v.Name)
			if err != nil {
				return err
			}
			longhorn.SortSnapshots(snaps)
			if g.output == "json" {
				return writeJSON(os.Stdout, snaps)
			}
			now := time.Now()
			w := newTab(os.Stdout)
			fmt.Fprintln(w, "NAME\tCREATED\tSIZE\tTYPE\tREADY\tPARENT")
			for _, s := range snaps {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%v\t%s\n", s.Name, dash(format.Age(s.Status.CreationTime, now)),
					format.Bytes(int64(s.Status.Size)), longhorn.SnapshotKind(s), s.Status.ReadyToUse, dash(s.Status.Parent))
			}
			return w.Flush()
		},
	}
}

func backupsCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:     "backups VOLUME",
		Aliases: []string{"backup", "bk"},
		Short:   "List the backups of a volume",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := g.checkOutput(); err != nil {
				return err
			}
			c, err := g.client()
			if err != nil {
				return err
			}
			defer g.reportTraffic(c)
			v, err := resolveVolume(cmd.Context(), c, args[0])
			if err != nil {
				return err
			}
			bks, err := c.Backups(cmd.Context(), v.Name)
			if err != nil {
				return err
			}
			longhorn.SortBackups(bks)
			if g.output == "json" {
				return writeJSON(os.Stdout, bks)
			}
			now := time.Now()
			w := newTab(os.Stdout)
			fmt.Fprintln(w, "NAME\tSTATE\tPROGRESS\tCREATED\tSIZE\tUPLOADED\tTARGET\tSNAPSHOT")
			for _, b := range bks {
				fmt.Fprintf(w, "%s\t%s\t%d%%\t%s\t%s\t%s\t%s\t%s\n", b.Name, dash(b.Status.State), b.Status.Progress,
					dash(format.Age(longhorn.BackupTime(b), now)), format.Bytes(int64(b.Status.Size)),
					format.Bytes(int64(b.Status.NewlyUploadDataSize)), dash(b.Status.BackupTargetName), dash(b.Status.SnapshotName))
			}
			return w.Flush()
		},
	}
}

func eventsCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:     "events VOLUME",
		Aliases: []string{"event", "ev"},
		Short:   "Show events of a volume, its engine, replicas and PVC",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := g.checkOutput(); err != nil {
				return err
			}
			c, err := g.client()
			if err != nil {
				return err
			}
			defer g.reportTraffic(c)
			ctx := cmd.Context()
			v, err := resolveVolume(ctx, c, args[0])
			if err != nil {
				return err
			}
			reps, err := c.Replicas(ctx, v.Name)
			if err != nil {
				return err
			}
			engs, err := c.Engines(ctx, v.Name)
			if err != nil {
				return err
			}
			vv := longhorn.BuildVolumeView(v, reps, nil, nil)
			evs, err := c.Events(ctx, longhorn.VolumeEventRefs(c.Namespace, &vv, engs))
			if err != nil {
				return err
			}
			if g.output == "json" {
				return writeJSON(os.Stdout, evs)
			}
			if len(evs) == 0 {
				fmt.Println("no events (Kubernetes keeps events for 1 hour by default)")
				return nil
			}
			now := time.Now()
			w := newTab(os.Stdout)
			fmt.Fprintln(w, "AGE\tTYPE\tREASON\tOBJECT\tMESSAGE")
			for _, e := range evs {
				age := format.Age(e.Time.Format(time.RFC3339), now)
				if e.Count > 1 {
					age += fmt.Sprintf(" (x%d)", e.Count)
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", age, e.Type, e.Reason, e.Object, e.Message)
			}
			return w.Flush()
		},
	}
}

func settingsCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:     "settings [FILTER]",
		Aliases: []string{"setting"},
		Short:   "List Longhorn settings (optionally filtered by name)",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := g.checkOutput(); err != nil {
				return err
			}
			c, err := g.client()
			if err != nil {
				return err
			}
			defer g.reportTraffic(c)
			sets, err := c.Settings(cmd.Context())
			if err != nil {
				return err
			}
			sort.Slice(sets, func(i, j int) bool { return sets[i].Name < sets[j].Name })
			if len(args) == 1 {
				f := strings.ToLower(args[0])
				var out []*longhorn.Setting
				for _, s := range sets {
					if strings.Contains(s.Name, f) {
						out = append(out, s)
					}
				}
				sets = out
			}
			if g.output == "json" {
				m := map[string]string{}
				for _, s := range sets {
					m[s.Name] = s.Value
				}
				return writeJSON(os.Stdout, m)
			}
			w := newTab(os.Stdout)
			fmt.Fprintln(w, "NAME\tVALUE")
			for _, s := range sets {
				val := s.Value
				if g.output != "wide" {
					val = format.Short(val, 80)
				}
				fmt.Fprintf(w, "%s\t%s\n", s.Name, dash(val))
			}
			return w.Flush()
		},
	}
}

func jobsCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:     "jobs [VOLUME]",
		Aliases: []string{"recurringjobs", "recurring-jobs", "rj"},
		Short:   "List recurring jobs (or the jobs that apply to a volume)",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := g.checkOutput(); err != nil {
				return err
			}
			c, err := g.client()
			if err != nil {
				return err
			}
			defer g.reportTraffic(c)
			jobs, err := c.RecurringJobs(cmd.Context())
			if err != nil {
				return err
			}
			sort.Slice(jobs, func(i, j int) bool { return jobs[i].Name < jobs[j].Name })
			if len(args) == 1 {
				v, err := resolveVolume(cmd.Context(), c, args[0])
				if err != nil {
					return err
				}
				jobs = longhorn.VolumeRecurringJobs(v, jobs)
			}
			if g.output == "json" {
				return writeJSON(os.Stdout, jobs)
			}
			w := newTab(os.Stdout)
			fmt.Fprintln(w, "NAME\tTASK\tCRON\tRETAIN\tCONCURRENCY\tGROUPS\tRUNS")
			for _, j := range jobs {
				fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%d\t%s\t%d\n", j.Name, j.Spec.Task, j.Spec.Cron, j.Spec.Retain,
					j.Spec.Concurrency, dash(strings.Join(j.Spec.Groups, ",")), j.Status.ExecutionCount)
			}
			return w.Flush()
		},
	}
}

func targetsCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:     "targets",
		Aliases: []string{"backuptargets", "bt"},
		Short:   "List backup targets",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := g.checkOutput(); err != nil {
				return err
			}
			c, err := g.client()
			if err != nil {
				return err
			}
			defer g.reportTraffic(c)
			ts, err := c.BackupTargets(cmd.Context())
			if err != nil {
				return err
			}
			if g.output == "json" {
				return writeJSON(os.Stdout, ts)
			}
			now := time.Now()
			w := newTab(os.Stdout)
			fmt.Fprintln(w, "NAME\tURL\tAVAILABLE\tLAST-SYNC\tPOLL\tMESSAGE")
			for _, t := range ts {
				msg := ""
				for _, cnd := range t.Status.Conditions {
					if cnd.Type == "Unavailable" && cnd.Status == "True" {
						msg = cnd.Message
					}
				}
				fmt.Fprintf(w, "%s\t%s\t%v\t%s\t%s\t%s\n", t.Name, dash(t.Spec.BackupTargetURL), t.Status.Available,
					dash(format.Age(t.Status.LastSyncedAt, now)), dash(t.Spec.PollInterval), dash(msg))
			}
			return w.Flush()
		},
	}
}
