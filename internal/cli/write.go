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
	cmd.AddCommand(snap, rep)
	return cmd
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

func writeCmds(g *globals) []*cobra.Command {
	return []*cobra.Command{
		createCmd(g), deleteCmd(g), setCmd(g),
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
	}
}
