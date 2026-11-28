package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/nxckdx/longhorntx/internal/longhorn"
	"github.com/nxckdx/longhorntx/internal/shrink"
	"github.com/nxckdx/longhorntx/internal/switchover"
)

// exitError carries a process exit code without printing an error.
type exitError struct{ code int }

func (e exitError) Error() string { return fmt.Sprintf("exit %d", e.code) }

func doctorCmd(g *globals) *cobra.Command {
	var backupMaxAge time.Duration
	var minSev string
	cmd := &cobra.Command{
		Use:     "doctor",
		Aliases: []string{"health", "check"},
		Short:   "Check volumes, replicas, disks, backups for problems",
		Long: `Read-only health check. Reports what is wrong right now:
faulted/degraded volumes, single healthy replica, replicas sharing a disk,
nodes/disks not ready or full, SMART warnings, unavailable backup targets,
backups older than their recurring job schedule, slow rebuilds and a
suspected rebuild/snapshot-removal deadlock, orphans.

Engines and snapshots are only read for volumes that are rebuilding.

Exit codes (Nagios style): 0 ok, 1 warning, 2 critical, 3 error.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// any error means UNKNOWN (3) for monitoring
			err := runDoctor(cmd, g, backupMaxAge, minSev)
			var ee exitError
			if err != nil && !errors.As(err, &ee) {
				fmt.Fprintln(os.Stderr, "error:", err)
				return exitError{3}
			}
			return err
		},
	}
	cmd.Flags().DurationVar(&backupMaxAge, "backup-max-age", 0, "also flag volumes without a backup job whose last backup is older (e.g. 36h)")
	cmd.Flags().StringVar(&minSev, "min", "info", "lowest severity to show: info|warning|critical")
	return cmd
}

func runDoctor(cmd *cobra.Command, g *globals, backupMaxAge time.Duration, minSev string) error {
	if err := g.checkOutput(); err != nil {
		return err
	}
	floor, err := parseSeverity(minSev)
	if err != nil {
		return err
	}
	c, err := g.client()
	if err != nil {
		return err
	}
	defer g.reportTraffic(c)
	in, err := longhorn.LoadDoctorInput(cmd.Context(), c)
	if err != nil {
		return err
	}
	in.BackupMaxAge = backupMaxAge
	in.Extra = append(switchover.Checks(cmd.Context(), c, switchover.JournalDir()), c.EncryptionChecks(cmd.Context(), in.Volumes)...)
	in.Extra = append(in.Extra, c.HistoryChecks(cmd.Context())...)
	in.Extra = append(in.Extra, c.CapacityChecks(cmd.Context())...)
	in.Extra = append(in.Extra, c.RWXChecks(cmd.Context(), in.Volumes)...)
	in.Extra = append(in.Extra, shrink.LeftoverChecks(in.Volumes)...)
	in.Extra = append(in.Extra, longhorn.PlacementFindings(in.Volumes, in.Replicas, in.Nodes, func() longhorn.PlacementSettings { return c.PlacementSettings(cmd.Context()) })...)
	in.Extra = append(in.Extra, c.MountChecks(cmd.Context(), in.Volumes, longhorn.NodeNames(in.Nodes))...)
	all := longhorn.Diagnose(in)
	var fs []longhorn.Finding
	for _, f := range all {
		if f.Severity >= floor {
			fs = append(fs, f)
		}
	}

	if g.output == "json" {
		if err := writeJSON(os.Stdout, map[string]any{
			"context": c.Cluster.Context, "worst": longhorn.Worst(all), "findings": fs,
		}); err != nil {
			return err
		}
	} else {
		printFindings(c.Cluster.Context, len(in.Volumes), len(in.Nodes), all, fs, g.output == "wide")
	}
	switch longhorn.Worst(all) {
	case longhorn.SevCritical:
		return exitError{2}
	case longhorn.SevWarning:
		return exitError{1}
	}
	return nil
}

func parseSeverity(s string) (longhorn.Severity, error) {
	switch strings.ToLower(s) {
	case "", "info":
		return longhorn.SevInfo, nil
	case "warning", "warn":
		return longhorn.SevWarning, nil
	case "critical", "crit":
		return longhorn.SevCritical, nil
	}
	return 0, fmt.Errorf("invalid --min %q (info|warning|critical)", s)
}

func printFindings(ctxName string, vols, nodes int, all, shown []longhorn.Finding, wide bool) {
	count := map[longhorn.Severity]int{}
	for _, f := range all {
		count[f.Severity]++
	}
	status := "OK"
	if w := longhorn.Worst(all); w > longhorn.SevInfo {
		status = w.String()
	}
	fmt.Printf("%s · %d volumes, %d nodes · %s (%d critical, %d warnings, %d info)\n",
		ctxName, vols, nodes, status, count[longhorn.SevCritical], count[longhorn.SevWarning], count[longhorn.SevInfo])
	if len(shown) == 0 {
		return
	}
	fmt.Println()
	w := newTab(os.Stdout)
	fmt.Fprintln(w, "SEVERITY\tCHECK\tOBJECT\tMESSAGE")
	for _, f := range shown {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", f.Severity, f.Check, f.Object, f.Message)
		if wide && f.Hint != "" {
			fmt.Fprintf(w, "\t\t\t→ %s\n", f.Hint)
		}
	}
	_ = w.Flush()
	if !wide {
		for _, f := range shown {
			if f.Hint != "" {
				fmt.Println("\n(-o wide shows hints)")
				break
			}
		}
	}
}

func drainCheckCmd(g *globals) *cobra.Command {
	var minSev string
	cmd := &cobra.Command{
		Use:     "drain-check NODE",
		Aliases: []string{"drain"},
		Short:   "What happens to volumes if NODE goes away (reboot, drain, replacement)",
		Long: `Read-only check before node maintenance: volumes that lose their last
healthy replica (critical), are left with a single healthy replica or have a
rebuild running elsewhere (warning), and volumes attached on the node (info).
Also shows Longhorn's node-drain-policy setting.

Exit codes like doctor: 0 ok, 1 warning, 2 critical, 3 error.`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: g.completeNodeDisk,
		RunE: func(cmd *cobra.Command, args []string) error {
			err := runDrainCheck(cmd, g, strings.SplitN(args[0], ":", 2)[0], minSev)
			var ee exitError
			if err != nil && !errors.As(err, &ee) {
				fmt.Fprintln(os.Stderr, "error:", err)
				return exitError{3}
			}
			return err
		},
	}
	cmd.Flags().StringVar(&minSev, "min", "info", "lowest severity to show: info|warning|critical")
	_ = cmd.RegisterFlagCompletionFunc("min", cobra.FixedCompletions([]string{"info", "warning", "critical"}, cobra.ShellCompDirectiveNoFileComp))
	return cmd
}

func filterSeverity(fs []longhorn.Finding, floor longhorn.Severity) []longhorn.Finding {
	var out []longhorn.Finding
	for _, f := range fs {
		if f.Severity >= floor {
			out = append(out, f)
		}
	}
	return out
}

func runDrainCheck(cmd *cobra.Command, g *globals, node, minSev string) error {
	if err := g.checkOutput(); err != nil {
		return err
	}
	floor, err := parseSeverity(minSev)
	if err != nil {
		return err
	}
	c, err := g.client()
	if err != nil {
		return err
	}
	defer g.reportTraffic(c)
	ctx := cmd.Context()
	if _, err = c.Node(ctx, node); err != nil {
		return err
	}
	st, err := loadState(cmd, c)
	if err != nil {
		return err
	}
	policy, _ := c.Setting(ctx, "node-drain-policy")
	fs := longhorn.DrainCheck(st, node)
	if g.output == "json" {
		if err := writeJSON(os.Stdout, map[string]any{"node": node, "nodeDrainPolicy": policy, "worst": longhorn.Worst(fs), "findings": filterSeverity(fs, floor)}); err != nil {
			return err
		}
	} else {
		printFindings(c.Cluster.Context+" · drain check "+node, len(st.Volumes), len(st.Nodes), fs, filterSeverity(fs, floor), g.output == "wide")
		if policy != "" {
			fmt.Printf("\nLonghorn node-drain-policy: %s\n", policy)
		}
	}
	switch longhorn.Worst(fs) {
	case longhorn.SevCritical:
		return exitError{2}
	case longhorn.SevWarning:
		return exitError{1}
	}
	return nil
}
