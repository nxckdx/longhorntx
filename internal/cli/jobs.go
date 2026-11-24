package cli

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/nxckdx/longhorntx/internal/longhorn"
)

// jobFlags are shared by jobs create and jobs edit.
type jobFlags struct {
	task, cron             string
	retain, concurrency    int
	groups, labels, params []string
}

func (f *jobFlags) register(cmd *cobra.Command, create bool) {
	fl := cmd.Flags()
	fl.StringVar(&f.task, "task", "", "snapshot, snapshot-force-create, snapshot-cleanup, snapshot-delete, backup, backup-force-create, filesystem-trim, system-backup")
	fl.StringVar(&f.cron, "cron", "", `schedule, 5 fields in UTC, e.g. "0 2 * * *" or @daily`)
	fl.IntVar(&f.retain, "retain", -1, "snapshots/backups to keep per volume (0 for cleanup/trim)")
	fl.IntVar(&f.concurrency, "concurrency", 0, "volumes processed at the same time")
	fl.StringSliceVar(&f.groups, "groups", nil, `groups (e.g. default = all volumes without own job labels); "" for none`)
	fl.StringSliceVar(&f.labels, "label", nil, "label for the created snapshots/backups, k=v (repeatable; k= removes)")
	fl.StringSliceVar(&f.params, "param", nil, "parameter, e.g. full-backup-interval=7 (repeatable; k= removes)")
	if create {
		_ = cmd.MarkFlagRequired("task")
		_ = cmd.MarkFlagRequired("cron")
	}
	_ = cmd.RegisterFlagCompletionFunc("task", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return longhorn.JobTasks, cobra.ShellCompDirectiveNoFileComp
	})
}

// apply sets the given flags on s (only those the user passed).
func (f *jobFlags) apply(cmd *cobra.Command, s *longhorn.JobSpec) error {
	fl := cmd.Flags()
	if fl.Changed("task") {
		s.Task = f.task
	}
	if fl.Changed("cron") {
		s.Cron = f.cron
	}
	if fl.Changed("retain") {
		s.Retain = f.retain
	}
	if fl.Changed("concurrency") {
		s.Concurrency = f.concurrency
	}
	if fl.Changed("groups") {
		s.Groups = nil
		for _, g := range f.groups {
			if g = strings.TrimSpace(g); g != "" {
				s.Groups = append(s.Groups, g)
			}
		}
	}
	var err error
	if s.Labels, err = applyKV(s.Labels, f.labels); err != nil {
		return err
	}
	s.Parameters, err = applyKV(s.Parameters, f.params)
	return err
}

func applyKV(m map[string]string, kvs []string) (map[string]string, error) {
	for _, kv := range kvs {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("expected key=value, got %q", kv)
		}
		if m == nil {
			m = map[string]string{}
		}
		if v == "" {
			delete(m, k)
		} else {
			m[k] = v
		}
	}
	return m, nil
}

func jobWriteCmds(g *globals) []*cobra.Command {
	var cf, ef jobFlags
	create := &cobra.Command{
		Use:   "create NAME --task T --cron C [--retain N] [--groups a,b]",
		Short: "Create a recurring job (needs --rw)",
		Long: `Create a RecurringJob like the web UI's "Recurring Job" page. The preview
shows the next runs (cron is evaluated in UTC by the cluster, shown in your
time zone). Defaults: concurrency 1, retain 1 (0 for cleanup/trim), no group.

  lhtx --rw jobs create nightly-backup --task backup --cron "0 2 * * *" --retain 14 --groups default`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := g.client()
			if err != nil {
				return err
			}
			jobs, err := c.RecurringJobs(cmd.Context())
			if err != nil {
				return err
			}
			s := longhorn.JobSpec{Name: args[0], Concurrency: 1, Retain: 1}
			if err := cf.apply(cmd, &s); err != nil {
				return err
			}
			if !cmd.Flags().Changed("retain") && (s.Task == "snapshot-cleanup" || s.Task == "filesystem-trim") {
				s.Retain = 0
			}
			a, err := longhorn.CreateJob(s, jobs)
			if err != nil {
				return err
			}
			return g.runAction(cmd.Context(), c, a)
		},
	}
	cf.register(create, true)

	edit := &cobra.Command{
		Use:               "edit NAME [--cron C] [--retain N] [--groups a,b] …",
		Short:             "Change a recurring job (needs --rw); only the given flags change",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: g.completeJobNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, j, vols, err := g.loadJob(cmd, args[0])
			if err != nil {
				return err
			}
			s := longhorn.SpecOf(j)
			if err := ef.apply(cmd, &s); err != nil {
				return err
			}
			a, err := longhorn.UpdateJob(j, s, vols)
			if err != nil {
				return err
			}
			return g.runAction(cmd.Context(), c, a)
		},
	}
	ef.register(edit, false)

	del := &cobra.Command{
		Use:               "delete NAME",
		Short:             "Delete a recurring job (needs --rw); shows the volumes that use it",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: g.completeJobNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, j, vols, err := g.loadJob(cmd, args[0])
			if err != nil {
				return err
			}
			a, err := longhorn.DeleteJob(j, vols)
			if err != nil {
				return err
			}
			return g.runAction(cmd.Context(), c, a)
		},
	}

	next := &cobra.Command{
		Use:   "next CRON",
		Short: "Show when a cron expression runs (UTC, like Longhorn) in your time zone",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if _, err := longhorn.ParseCron(args[0]); err != nil {
				return err
			}
			now := time.Now()
			w := newTab(os.Stdout)
			fmt.Fprintf(w, "WHEN (%s)\tDATE\tUTC\n", now.Format("MST"))
			for _, t := range longhorn.NextRuns(args[0], now, 5) {
				fmt.Fprintf(w, "%s\t%s\t%s\n", longhorn.FormatRuns([]time.Time{t}, now), t.Format("2006-01-02"), t.UTC().Format("15:04"))
			}
			return w.Flush()
		},
	}
	return []*cobra.Command{create, edit, del, next}
}

func (g *globals) loadJob(cmd *cobra.Command, name string) (*longhorn.Client, *longhorn.RecurringJob, []*longhorn.Volume, error) {
	c, err := g.client()
	if err != nil {
		return nil, nil, nil, err
	}
	j, err := c.RecurringJob(cmd.Context(), name)
	if err != nil {
		return nil, nil, nil, err
	}
	vols, err := c.Volumes(cmd.Context())
	if err != nil {
		return nil, nil, nil, err
	}
	return c, j, vols, nil
}

func (g *globals) completeJobNames(cmd *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	names := g.cachedNames("jobs", func(ctx context.Context, c *longhorn.Client) ([]string, error) {
		js, err := c.RecurringJobs(ctx)
		var out []string
		for _, j := range js {
			out = append(out, j.Name)
		}
		sort.Strings(out)
		return out, err
	})
	return names, cobra.ShellCompDirectiveNoFileComp
}
