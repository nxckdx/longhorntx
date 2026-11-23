package cli

import (
	"context"
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
			// chain order, newest first; "│ " marks branches left by a revert
			lines := longhorn.SnapshotTree(snaps)
			now := time.Now()
			w := newTab(os.Stdout)
			fmt.Fprintln(w, "NAME\tCREATED\tSIZE\tTYPE\tREADY\tPARENT")
			for _, l := range lines {
				s := l.Snap
				name := l.Prefix() + s.Name
				if l.Head {
					name += " ← head"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%v\t%s\n", name, dash(format.Age(s.Status.CreationTime, now)),
					format.Bytes(int64(s.Status.Size)), longhorn.SnapshotKind(s), s.Status.ReadyToUse, dash(s.Status.Parent))
			}
			if err := w.Flush(); err != nil {
				return err
			}
			if longhorn.Branched(lines) {
				fmt.Println("\n│ = branch from an earlier revert (not part of the current chain)")
			}
			return nil
		},
	}
}

func backupsCmd(g *globals) *cobra.Command {
	var maxAge time.Duration
	cmd := &cobra.Command{
		Use:     "backups [VOLUME]",
		Aliases: []string{"backup", "bk"},
		Short:   "Backup age per volume, or the backups of one volume",
		Long: `Without VOLUME: the age of the last backup of every volume, compared
with the schedule of the backup recurring job that applies to it (late =
older than twice the job's interval). Reads only volumes and recurring jobs.
--max-age also checks volumes that are backed up without a recurring job.

With VOLUME: the backups of that volume.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := g.checkOutput(); err != nil {
				return err
			}
			c, err := g.client()
			if err != nil {
				return err
			}
			if len(args) == 0 {
				defer g.reportTraffic(c)
				return backupOverview(cmd, g, c, maxAge)
			}
			defer g.reportTraffic(c)
			volName := ""
			if v, err := resolveVolume(cmd.Context(), c, args[0]); err == nil {
				volName = v.Name
			} else if volName = deletedBackupVolume(cmd, c, args[0]); volName == "" {
				return err
			}
			bks, err := c.Backups(cmd.Context(), volName)
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
	cmd.Flags().DurationVar(&maxAge, "max-age", 0, "expected max age for volumes without a backup job (e.g. 36h)")
	return cmd
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
	cmd := &cobra.Command{
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
	cmd.AddCommand(jobAssignCmd(g, true), jobAssignCmd(g, false))
	return cmd
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

func backupOverview(cmd *cobra.Command, g *globals, c *longhorn.Client, maxAge time.Duration) error {
	ctx := cmd.Context()
	vols, err := c.Volumes(ctx)
	if err != nil {
		return err
	}
	jobs, err := c.RecurringJobs(ctx)
	if err != nil {
		return err
	}
	list := longhorn.BackupOverview(vols, jobs, time.Now(), maxAge)
	rank := map[string]int{"never": 0, "late": 1, "ok": 2, "unchecked": 3}
	sort.SliceStable(list, func(i, j int) bool { return rank[list[i].Status] < rank[list[j].Status] })
	if g.output == "json" {
		return writeJSON(os.Stdout, list)
	}
	count := map[string]int{}
	w := newTab(os.Stdout)
	fmt.Fprintln(w, "STATUS\tPVC\tLAST BACKUP\tJOB\tINTERVAL\tEXPECTED WITHIN")
	for _, b := range list {
		count[b.Status]++
		age, iv, exp := "-", "-", "-"
		if b.Last != "" {
			age = format.Age(b.Last, time.Now()) + " ago"
		}
		if b.Interval > 0 {
			iv = b.Interval.String()
		}
		if b.MaxAge > 0 {
			exp = b.MaxAge.String()
		}
		pvc := b.PVC
		if pvc == "" {
			pvc = b.Volume
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", strings.ToUpper(b.Status), pvc, age, dash(b.Job), iv, exp)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	fmt.Printf("\n%d ok, %d late, %d never, %d unchecked (no backup job", count["ok"], count["late"], count["never"], count["unchecked"])
	if maxAge == 0 {
		fmt.Print("; use --max-age to check those too")
	}
	fmt.Println(")")
	return nil
}

func orphansCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:     "orphans",
		Aliases: []string{"orphan"},
		Short:   "List orphaned replica data and instances",
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
			ovs, err := loadOrphans(cmd.Context(), c)
			if err != nil {
				return err
			}
			if g.output == "json" {
				return writeJSON(os.Stdout, ovs)
			}
			if len(ovs) == 0 {
				fmt.Println("no orphans")
				return nil
			}
			now := time.Now()
			w := newTab(os.Stdout)
			hdr := "NODE\tTYPE\tPATH\tVOLUME\tAGE\tCLEANABLE\tRISK"
			if g.output == "wide" {
				hdr += "\tNAME"
			}
			fmt.Fprintln(w, hdr)
			for _, ov := range ovs {
				path := ov.DiskPath + "/replicas/" + ov.DataName
				if ov.DataName == "" {
					path = "-"
				}
				vol := "deleted"
				if ov.VolExists {
					vol = ov.PVC
					if vol == "" {
						vol = ov.Volume
					}
				}
				clean := fmt.Sprint(ov.Cleanable)
				if ov.InUse {
					clean = "IN USE"
				}
				line := fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%s\t%s", ov.Node, ov.Type, path, vol, format.Age(ov.Created, now), clean, dash(ov.Risk))
				if g.output == "wide" {
					line += "\t" + ov.Name
				}
				fmt.Fprintln(w, line)
			}
			return w.Flush()
		},
	}
}

func loadOrphans(ctx context.Context, c *longhorn.Client) ([]longhorn.OrphanView, error) {
	ors, err := c.Orphans(ctx)
	if err != nil {
		return nil, err
	}
	vols, err := c.Volumes(ctx)
	if err != nil {
		return nil, err
	}
	reps, err := c.Replicas(ctx, "")
	if err != nil {
		return nil, err
	}
	return longhorn.BuildOrphans(ors, vols, reps), nil
}

func engineImagesCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:     "engineimages",
		Aliases: []string{"engineimage", "ei"},
		Short:   "List engine images with state and how many volumes use them",
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
			env, err := c.LoadEngineUpgradeEnv(cmd.Context())
			if err != nil {
				return err
			}
			if g.output == "json" {
				return writeJSON(os.Stdout, env)
			}
			w := newTab(os.Stdout)
			fmt.Fprintln(w, "VERSION\tIMAGE\tSTATE\tREFS\tNODES\tDEFAULT")
			for _, ei := range env.Images {
				ready := 0
				for _, ok := range ei.Status.NodeDeploymentMap {
					if ok {
						ready++
					}
				}
				def := ""
				if ei.Spec.Image == env.DefaultImage {
					def = "yes"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%d/%d\t%s\n", dash(ei.Status.Version), ei.Spec.Image, ei.Status.State, ei.Status.RefCount, ready, len(ei.Status.NodeDeploymentMap), def)
			}
			if err := w.Flush(); err != nil {
				return err
			}
			if env.AutoLimit > 0 {
				fmt.Printf("\nautomatic engine upgrade is on (%d per node): volumes move to the default image by themselves\n", env.AutoLimit)
			}
			return nil
		},
	}
}

// jobAssignCmd: lhtx jobs add|remove VOLUME JOB|group:NAME...
func jobAssignCmd(g *globals, assign bool) *cobra.Command {
	use, short := "add", "Assign recurring jobs or groups to a volume (needs --rw)"
	if !assign {
		use, short = "remove", "Remove recurring jobs or groups from a volume (needs --rw)"
	}
	return &cobra.Command{
		Use:   use + " VOLUME JOB|group:NAME...",
		Short: short,
		Long: short + `.

Sets or removes the volume labels recurring-job.longhorn.io/<job> and
recurring-job-group.longhorn.io/<group>, like the web UI. Jobs that apply
through a group can only be removed by removing the group.`,
		Args: cobra.MinimumNArgs(2),
		ValidArgsFunction: func(cmd *cobra.Command, args []string, tc string) ([]string, cobra.ShellCompDirective) {
			if len(args) == 0 {
				return g.completeVolumes(cmd, args, tc)
			}
			return g.completeJobs(cmd, args, tc)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, err := g.client()
			if err != nil {
				return err
			}
			v, err := resolveVolume(ctx, c, args[0])
			if err != nil {
				return err
			}
			jobs, err := c.RecurringJobs(ctx)
			if err != nil {
				return err
			}
			var pvcLabels map[string]string
			if info, err := c.PVCInfo(ctx, v); err == nil && info != nil {
				pvcLabels = info.Labels
			}
			var refs []longhorn.JobRef
			for _, a := range args[1:] {
				refs = append(refs, longhorn.ParseJobRef(a))
			}
			act, err := longhorn.SetJobAssignment(v, refs, assign, jobs, pvcLabels)
			if err != nil {
				return err
			}
			return g.runAction(ctx, c, act)
		},
	}
}

func backupVolumesCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:     "backupvolumes",
		Aliases: []string{"backupvolume", "bv"},
		Short:   "Volumes with backups in the backup store, also deleted ones",
		Long: `One line per volume and backup target that has backups: last backup,
size, data stored in the backup store. Volumes that no longer exist come
first – their backups are what a restore would use. The PVC of a deleted
volume is the one recorded with its last backup.

Traffic: one LIST of backup volumes and one of volumes.`,
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
			ctx := cmd.Context()
			bvs, err := c.BackupVolumes(ctx)
			if err != nil {
				return err
			}
			vols, err := c.Volumes(ctx)
			if err != nil {
				return err
			}
			views := longhorn.BuildBackupVolumes(bvs, vols)
			if g.output == "json" {
				return writeJSON(os.Stdout, views)
			}
			if len(views) == 0 {
				fmt.Println("no backup volumes")
				return nil
			}
			now := time.Now()
			w := newTab(os.Stdout)
			hdr := "PVC\tVOLUME\tTARGET\tLAST BACKUP\tSIZE\tSTORED"
			if g.output == "wide" {
				hdr += "\tLAST\tNAME\tMESSAGE"
			}
			fmt.Fprintln(w, hdr)
			for _, v := range views {
				vol := v.Volume
				if !v.VolExists {
					vol += " (deleted)"
				}
				line := fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%s", dash(v.PVC), vol, v.Target, dash(format.Age(v.LastAt, now)),
					format.Bytes(v.Size), format.Bytes(v.Stored))
				if g.output == "wide" {
					line += fmt.Sprintf("\t%s\t%s\t%s", dash(v.Last), v.Name, dash(v.Message))
				}
				fmt.Fprintln(w, line)
			}
			return w.Flush()
		},
	}
}

// deletedBackupVolume finds a volume that no longer exists by its backup
// volume: volume name, namespace/pvc or a unique bare PVC name as recorded
// with the last backup. "" if none matches.
func deletedBackupVolume(cmd *cobra.Command, c *longhorn.Client, arg string) string {
	bvs, err := c.BackupVolumes(cmd.Context())
	if err != nil {
		return ""
	}
	found := map[string]bool{}
	for _, bv := range bvs {
		pvc := bv.LastPVC()
		_, bare, _ := strings.Cut(pvc, "/")
		if bv.Volume() == arg || (pvc != "" && (pvc == arg || bare == arg)) {
			found[bv.Volume()] = true
		}
	}
	if len(found) != 1 {
		return ""
	}
	for v := range found {
		fmt.Fprintf(os.Stderr, "volume %s no longer exists – showing its backups\n", v)
		return v
	}
	return ""
}
