package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/nxckdx/longhorntx/internal/longhorn"
)

// Write commands. Without --rw nothing is changed; --dry-run (client) shows
// the request, --dry-run=server lets the apiserver and Longhorn's webhooks
// validate it without persisting anything. Both work without --rw.

func (g *globals) dryRun() (longhorn.DryRun, error) { return longhorn.ParseDryRun(g.dryRunFlag) }

// runAction previews, confirms and applies one action.
func (g *globals) runAction(ctx context.Context, c *longhorn.Client, a *longhorn.Action) error {
	mode, err := g.dryRun()
	if err != nil {
		return err
	}
	printAction(c, a)
	switch mode {
	case longhorn.DryRunClient:
		fmt.Println("\n(dry run: nothing sent)")
		return nil
	case longhorn.DryRunServer:
		if err := c.Apply(ctx, a, longhorn.DryRunServer); err != nil {
			return fmt.Errorf("server dry run rejected: %w", err)
		}
		fmt.Println("\n✔ accepted by the server (dry run, nothing changed)")
		return nil
	}
	if err := g.confirm(a.Destructive, a.Name); err != nil {
		return err
	}
	if err := c.Apply(ctx, a, longhorn.DryRunNone); err != nil {
		return err
	}
	fmt.Println("✔ done")
	return nil
}

func printAction(c *longhorn.Client, a *longhorn.Action) {
	fmt.Printf("%s · %s\n\n%s\n", a.Title, c.Cluster.Context, a.Preview(c.Namespace))
	for _, w := range a.Warnings {
		fmt.Println("⚠ " + w)
	}
}

// confirm asks on the terminal. Destructive actions require typing the
// object name.
func (g *globals) confirm(destructive bool, name string) error {
	if !g.rw {
		return fmt.Errorf("lhtx is read-only by default: add --rw to apply this change (or --dry-run[=server] to test it)")
	}
	if g.yes {
		return nil
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return fmt.Errorf("no terminal for confirmation: add --yes to apply non-interactively")
	}
	in := bufio.NewReader(os.Stdin)
	if destructive {
		fmt.Printf("\nType the name %q to confirm: ", name)
		s, _ := in.ReadString('\n')
		if strings.TrimSpace(s) != name {
			return fmt.Errorf("aborted")
		}
		return nil
	}
	fmt.Print("\nApply? [y/N] ")
	s, _ := in.ReadString('\n')
	if s = strings.ToLower(strings.TrimSpace(s)); s != "y" && s != "yes" {
		return fmt.Errorf("aborted")
	}
	return nil
}

func createCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{Use: "create", Short: "Create snapshots and backups (needs --rw)"}

	var snapName string
	snap := &cobra.Command{
		Use:   "snapshot VOLUME",
		Short: "Create a snapshot of a volume",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := g.client()
			if err != nil {
				return err
			}
			v, err := resolveVolume(cmd.Context(), c, args[0])
			if err != nil {
				return err
			}
			a, err := longhorn.CreateSnapshot(v, snapName)
			if err != nil {
				return err
			}
			return g.runAction(cmd.Context(), c, a)
		},
	}
	snap.Flags().StringVar(&snapName, "name", "", "snapshot name (default: lhtx-<random>)")

	var fromSnap string
	backup := &cobra.Command{
		Use:   "backup VOLUME",
		Short: "Back up a volume (new snapshot + backup, or --snapshot)",
		Long: `Back up a volume to its backup target.

Without --snapshot this does what the web UI does: create a snapshot, wait
until it is ready, then back it up.`,
		Args: cobra.ExactArgs(1),
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
			if fromSnap != "" {
				a, err := longhorn.CreateBackup(v, fromSnap)
				if err != nil {
					return err
				}
				return g.runAction(ctx, c, a)
			}
			// two steps: preview both
			snapA, err := longhorn.CreateSnapshot(v, "")
			if err != nil {
				return err
			}
			bkA, err := longhorn.CreateBackup(v, snapA.Name)
			if err != nil {
				return err
			}
			mode, err := g.dryRun()
			if err != nil {
				return err
			}
			fmt.Printf("Back up %s · %s (2 steps)\n\n1. %s\n", v.Name, c.Cluster.Context, snapA.Preview(c.Namespace))
			fmt.Printf("2. (when the snapshot is ready)\n%s\n", bkA.Preview(c.Namespace))
			for _, w := range append(snapA.Warnings, bkA.Warnings...) {
				fmt.Println("⚠ " + w)
			}
			switch mode {
			case longhorn.DryRunClient:
				fmt.Println("\n(dry run: nothing sent)")
				return nil
			case longhorn.DryRunServer:
				if err := c.Apply(ctx, snapA, longhorn.DryRunServer); err != nil {
					return fmt.Errorf("server dry run rejected step 1: %w", err)
				}
				fmt.Println("\n✔ step 1 accepted by the server (dry run); step 2 needs the real snapshot and cannot be dry-run")
				return nil
			}
			if err := g.confirm(false, ""); err != nil {
				return err
			}
			name, err := c.SnapshotAndBackup(ctx, v, func(s string) { fmt.Println("… " + s) })
			if err != nil {
				return err
			}
			fmt.Printf("✔ backup %s started – follow it with: lhtx rebuilds -w\n", name)
			return nil
		},
	}
	backup.Flags().StringVar(&fromSnap, "snapshot", "", "back up an existing snapshot instead of creating one")

	cmd.AddCommand(snap, backup)
	return cmd
}

func deleteCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{Use: "delete", Short: "Delete snapshots and replicas (needs --rw)"}
	snap := &cobra.Command{
		Use:   "snapshot NAME",
		Short: "Delete a snapshot (see lhtx snapshots VOLUME)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := g.client()
			if err != nil {
				return err
			}
			s, err := c.SnapshotCR(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			a, err := longhorn.DeleteSnapshot(s)
			if err != nil {
				return err
			}
			return g.runAction(cmd.Context(), c, a)
		},
	}
	rep := &cobra.Command{
		Use:   "replica NAME",
		Short: "Delete a replica (Longhorn rebuilds it elsewhere)",
		Long:  "Delete a replica. Refused if it is the volume's only healthy replica.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, err := g.client()
			if err != nil {
				return err
			}
			r, err := c.Replica(ctx, args[0])
			if err != nil {
				return err
			}
			siblings, err := c.Replicas(ctx, r.Spec.VolumeName)
			if err != nil {
				return err
			}
			v, _ := c.Volume(ctx, r.Spec.VolumeName)
			a, err := longhorn.DeleteReplica(r, v, siblings)
			if err != nil {
				return err
			}
			return g.runAction(ctx, c, a)
		},
	}
	var allCleanable bool
	orph := &cobra.Command{
		Use:   "orphan [NAME]",
		Short: "Delete an orphan (Longhorn removes its data directory if cleanable)",
		Long: `Delete orphan CRs. For replica data that Longhorn marks DataCleanable,
Longhorn deletes <disk>/replicas/<dir> on the node. --all-cleanable deletes
every cleanable orphan; you confirm by typing their number.

--all-cleanable skips risky orphans: replica data of volumes that are
deleted, faulted, degraded or without a healthy replica. DataCleanable only
means Longhorn is able to delete the data – for such volumes it may be the
last usable copy. Delete those one by one by NAME after checking.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if (len(args) == 1) == allCleanable {
				return fmt.Errorf("give an orphan NAME or --all-cleanable")
			}
			c, err := g.client()
			if err != nil {
				return err
			}
			ovs, err := loadOrphans(ctx, c)
			if err != nil {
				return err
			}
			var acts []*longhorn.Action
			var risky []string
			for _, ov := range ovs {
				if len(args) == 1 && ov.Name != args[0] {
					continue
				}
				if allCleanable && ov.Cleanable && !ov.InUse && ov.Risk != "" {
					risky = append(risky, fmt.Sprintf("  %s (%s, %s)", ov.Name, dash(ov.Volume), ov.Risk))
				}
				if allCleanable && !ov.BulkDeletable() {
					continue
				}
				a, err := longhorn.DeleteOrphan(ov)
				if err != nil {
					return err
				}
				acts = append(acts, a)
			}
			if len(risky) > 0 {
				fmt.Fprintf(os.Stderr, "skipping %d risky orphans (delete one by one by NAME after checking):\n%s\n",
					len(risky), strings.Join(risky, "\n"))
			}
			switch len(acts) {
			case 0:
				if allCleanable {
					return fmt.Errorf("no orphan is safe to delete in bulk")
				}
				return fmt.Errorf("no matching orphan")
			case 1:
				return g.runAction(ctx, c, acts[0])
			}
			return g.runBatch(ctx, c, fmt.Sprintf("Delete %d cleanable orphans", len(acts)), acts)
		},
	}
	orph.Flags().BoolVar(&allCleanable, "all-cleanable", false, "delete all orphans Longhorn marks as cleanable")
	cmd.AddCommand(snap, rep, orph)
	return cmd
}

// runBatch previews and applies several actions with one confirmation
// (type their number).
func (g *globals) runBatch(ctx context.Context, c *longhorn.Client, title string, acts []*longhorn.Action) error {
	mode, err := g.dryRun()
	if err != nil {
		return err
	}
	fmt.Printf("%s · %s\n\n", title, c.Cluster.Context)
	var warns []string
	seen := map[string]bool{}
	for _, a := range acts {
		fmt.Printf("  %s %s/%s  %s\n", a.Verb, a.GVR.Resource, a.Name, a.Summary())
		for _, w := range a.Warnings {
			if !seen[w] {
				seen[w] = true
				warns = append(warns, w)
			}
		}
	}
	for _, w := range warns {
		fmt.Println("⚠ " + w)
	}
	switch mode {
	case longhorn.DryRunClient:
		fmt.Println("\n(dry run: nothing sent)")
		return nil
	case longhorn.DryRunServer:
		for _, a := range acts {
			if err := c.Apply(ctx, a, longhorn.DryRunServer); err != nil {
				return fmt.Errorf("server dry run rejected %s: %w", a.Name, err)
			}
		}
		fmt.Printf("\n✔ all %d accepted by the server (dry run, nothing changed)\n", len(acts))
		return nil
	}
	if err := g.confirm(true, fmt.Sprint(len(acts))); err != nil {
		return err
	}
	failed := 0
	for _, a := range acts {
		if err := c.Apply(ctx, a, longhorn.DryRunNone); err != nil {
			failed++
			fmt.Printf("✖ %s: %v\n", a.Name, err)
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d failed", failed, len(acts))
	}
	fmt.Printf("✔ %d done\n", len(acts))
	return nil
}

func setCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{Use: "set", Short: "Change replica count or settings (needs --rw)"}
	reps := &cobra.Command{
		Use:   "replicas VOLUME COUNT",
		Short: "Set the number of replicas of a volume",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			n, err := strconv.Atoi(args[1])
			if err != nil {
				return fmt.Errorf("COUNT must be a number")
			}
			c, err := g.client()
			if err != nil {
				return err
			}
			v, err := resolveVolume(cmd.Context(), c, args[0])
			if err != nil {
				return err
			}
			a, err := longhorn.SetReplicaCount(v, n)
			if err != nil {
				return err
			}
			if nodes, err := c.Nodes(cmd.Context()); err == nil {
				if w := longhorn.ReplicaCountWarning(n, nodes); w != "" {
					a.Warnings = append(a.Warnings, w)
				}
			}
			return g.runAction(cmd.Context(), c, a)
		},
	}
	setting := &cobra.Command{
		Use:   "setting NAME VALUE",
		Short: "Change a Longhorn setting (validated by Longhorn's webhook)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := g.client()
			if err != nil {
				return err
			}
			s, err := c.SettingCR(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			a, err := longhorn.SetSetting(s, args[1])
			if err != nil {
				return err
			}
			return g.runAction(cmd.Context(), c, a)
		},
	}
	cmd.AddCommand(reps, setting)
	return cmd
}

// nodeDiskCmd builds cordon / uncordon / evict.
func nodeDiskCmd(g *globals, use, short string, build func(n *longhorn.Node, disk string, cancel bool) (*longhorn.Action, error), withCancel bool) *cobra.Command {
	var cancel bool
	cmd := &cobra.Command{
		Use:   use + " NODE[:DISK]",
		Short: short,
		Long: short + `.

DISK may be the disk name (disk-1) or its path (/var/lib/longhorn).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			nodeName, diskSel, _ := strings.Cut(args[0], ":")
			c, err := g.client()
			if err != nil {
				return err
			}
			n, err := c.Node(cmd.Context(), nodeName)
			if err != nil {
				return err
			}
			disk := ""
			if diskSel != "" {
				for name, d := range n.Spec.Disks {
					if name == diskSel || d.Path == diskSel {
						disk = name
					}
				}
				if disk == "" {
					return fmt.Errorf("node %s has no disk %q", nodeName, diskSel)
				}
			}
			a, err := build(n, disk, cancel)
			if err != nil {
				return err
			}
			return g.runAction(cmd.Context(), c, a)
		},
	}
	if withCancel {
		cmd.Flags().BoolVar(&cancel, "cancel", false, "cancel a requested eviction")
	}
	return cmd
}

func upgradeEngineCmd(g *globals) *cobra.Command {
	var image string
	cmd := &cobra.Command{
		Use:   "upgrade-engine VOLUME... --image IMAGE",
		Short: "Switch volumes to another engine image (needs --rw)",
		Long: `Switch one or more volumes to another engine image, like "Upgrade Engine" in
the web UI. Same checks as Longhorn's API: image deployed on all involved
nodes and compatible, no upgrade in progress (except a rollback), live
upgrade only for healthy, non-strict-local volumes, only the default image
while automatic engine upgrade is enabled. See lhtx engineimages.`,
		Args:              cobra.MinimumNArgs(1),
		ValidArgsFunction: g.completeVolumes,
		RunE: func(cmd *cobra.Command, args []string) error {
			if image == "" {
				return fmt.Errorf("--image is required (see lhtx engineimages)")
			}
			ctx := cmd.Context()
			c, err := g.client()
			if err != nil {
				return err
			}
			env, err := c.LoadEngineUpgradeEnv(ctx)
			if err != nil {
				return err
			}
			// accept a version ("v1.12.1") as well as the full image
			for _, ei := range env.Images {
				if ei.Status.Version == image {
					image = ei.Spec.Image
				}
			}
			var acts []*longhorn.Action
			for _, ref := range args {
				v, err := resolveVolume(ctx, c, ref)
				if err != nil {
					return err
				}
				reps, err := c.Replicas(ctx, v.Name)
				if err != nil {
					return err
				}
				a, err := longhorn.UpgradeEngine(v, reps, image, env)
				if err != nil {
					return err
				}
				acts = append(acts, a)
			}
			if len(acts) == 1 {
				return g.runAction(ctx, c, acts[0])
			}
			return g.runBatch(ctx, c, fmt.Sprintf("Upgrade engine of %d volumes", len(acts)), acts)
		},
	}
	cmd.Flags().StringVar(&image, "image", "", "engine image or its version (e.g. v1.12.1)")
	return cmd
}

func expandCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "expand VOLUME SIZE",
		Short: "Grow a volume, e.g. 20Gi or +5Gi (needs --rw)",
		Long: `Grow a volume to SIZE (20Gi) or by SIZE (+5Gi).

A volume bound to a PVC is expanded through the PVC, like longhorn-manager
does: the CSI resizer grows the Longhorn volume and the filesystem (while the
volume is attached), and the PVC shows the new size. The StorageClass must
allow volume expansion. Volumes without a PVC get spec.size patched; their
filesystem has to be grown by hand. Volumes cannot shrink.`,
		Args: cobra.ExactArgs(2),
		ValidArgsFunction: func(cmd *cobra.Command, args []string, tc string) ([]string, cobra.ShellCompDirective) {
			if len(args) == 0 {
				return g.completeVolumes(cmd, args, tc)
			}
			return []string{"+1Gi", "+5Gi", "+10Gi"}, cobra.ShellCompDirectiveNoFileComp
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
			size, err := longhorn.ParseSize(args[1], int64(v.Spec.Size))
			if err != nil {
				return err
			}
			pvc, err := c.PVCInfo(ctx, v)
			if err != nil {
				return err
			}
			a, err := longhorn.ExpandVolume(v, pvc, size)
			if err != nil {
				return err
			}
			return g.runAction(ctx, c, a)
		},
	}
}

func attachCmds(g *globals) []*cobra.Command {
	var maintenance bool
	attach := &cobra.Command{
		Use:   "attach VOLUME NODE",
		Short: "Attach a volume to a node, like Attach in the web UI (needs --rw)",
		Long: `Attach a volume to a node by adding the attachment ticket the web UI uses
("longhorn-ui"), so it can be detached from lhtx or the UI. The volume stays
attached until detached. --maintenance attaches without frontend (no block
device on the node), e.g. to revert to a snapshot.`,
		Args: cobra.ExactArgs(2),
		ValidArgsFunction: func(cmd *cobra.Command, args []string, tc string) ([]string, cobra.ShellCompDirective) {
			if len(args) == 0 {
				return g.completeVolumes(cmd, args, tc)
			}
			return g.completeNodeDisk(cmd, nil, tc)
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
			va, err := c.VolumeAttachment(ctx, v.Name)
			if err != nil {
				return err
			}
			n, err := c.Node(ctx, args[1])
			if err != nil {
				return err
			}
			a, err := longhorn.AttachVolume(v, va, n, maintenance)
			if err != nil {
				return err
			}
			return g.runAction(ctx, c, a)
		},
	}
	attach.Flags().BoolVar(&maintenance, "maintenance", false, "attach without frontend (maintenance mode)")
	detach := &cobra.Command{
		Use:   "detach VOLUME",
		Short: "Detach a manually attached volume (needs --rw)",
		Long: `Remove the manual attachment ticket ("longhorn-ui", from lhtx attach or the
web UI). Tickets of other attachers – CSI for a running pod, backups – are
not touched; the volume stays attached for them.`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: g.completeVolumes,
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
			va, err := c.VolumeAttachment(ctx, v.Name)
			if err != nil {
				return err
			}
			a, err := longhorn.DetachVolume(v, va)
			if err != nil {
				return err
			}
			return g.runAction(ctx, c, a)
		},
	}
	return []*cobra.Command{attach, detach}
}

func writeCmds(g *globals) []*cobra.Command {
	return append([]*cobra.Command{
		createCmd(g), deleteCmd(g), setCmd(g), upgradeEngineCmd(g), expandCmd(g), restoreCmd(g), cloneCmd(g), switchCmd(g),
		nodeDiskCmd(g, "cordon", "Disable replica scheduling on a node or disk (needs --rw)",
			func(n *longhorn.Node, d string, _ bool) (*longhorn.Action, error) {
				return longhorn.SetScheduling(n, d, false)
			}, false),
		nodeDiskCmd(g, "uncordon", "Enable replica scheduling on a node or disk (needs --rw)",
			func(n *longhorn.Node, d string, _ bool) (*longhorn.Action, error) {
				return longhorn.SetScheduling(n, d, true)
			}, false),
		nodeDiskCmd(g, "evict", "Evict all replicas from a node or disk (needs --rw)",
			func(n *longhorn.Node, d string, cancel bool) (*longhorn.Action, error) {
				return longhorn.SetEviction(n, d, !cancel)
			}, true),
	}, attachCmds(g)...)
}
