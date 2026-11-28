package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/nxckdx/longhorntx/internal/format"
	"github.com/nxckdx/longhorntx/internal/longhorn"
)

func placementCmd(g *globals) *cobra.Command {
	var size string
	var replicas int
	var nodeSel, diskSel []string
	cmd := &cobra.Command{
		Use:     "placement VOLUME | placement --size SIZE",
		Aliases: []string{"why"},
		Short:   "Why Longhorn does (not) place replicas: every node and disk with the scheduler's verdict",
		Long: `Goes through Longhorn's replica scheduler checks for every node and disk:
node ready and schedulable (cordoned), scheduling allowed, tags against the
volume's node/disk selector, disk type and condition, free space (the
minimal available percentage must stay free after the volume's actual size)
and over-provisioning (scheduled sizes plus the volume's size within the
limit), and replica anti-affinity per node, zone and disk.

With VOLUME: where a new replica of it could go and why not elsewhere – the
answer to "ReplicaSchedulingFailure" and a volume stuck degraded. With
--size: whether a new volume of that size would fit (before a restore,
clone or shrink). Read-only: nodes, the volume and its replicas, a few
settings.`,
		Args: cobra.MaximumNArgs(1),
		ValidArgsFunction: func(cmd *cobra.Command, args []string, tc string) ([]string, cobra.ShellCompDirective) {
			if len(args) == 0 {
				return g.completeVolumes(cmd, args, tc)
			}
			return nil, cobra.ShellCompDirectiveNoFileComp
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := g.checkOutput(); err != nil {
				return err
			}
			if (len(args) == 1) == (size != "") {
				return fmt.Errorf("give a VOLUME or --size")
			}
			ctx := cmd.Context()
			c, err := g.client()
			if err != nil {
				return err
			}
			defer g.reportTraffic(c)
			var v *longhorn.Volume
			var reps []*longhorn.Replica
			if len(args) == 1 {
				if v, err = resolveVolume(ctx, c, args[0]); err != nil {
					return err
				}
				if reps, err = c.Replicas(ctx, v.Name); err != nil {
					return err
				}
			} else {
				q, err := resource.ParseQuantity(size)
				if err != nil {
					return fmt.Errorf("--size: %w", err)
				}
				v = &longhorn.Volume{}
				v.Name = "(new volume)"
				v.Spec.Size = longhorn.Quantity(q.Value())
				v.Spec.NumberOfReplicas = replicas
				v.Spec.NodeSelector, v.Spec.DiskSelector = nodeSel, diskSel
			}
			nodes, err := c.Nodes(ctx)
			if err != nil {
				return err
			}
			p := longhorn.AnalyzePlacement(v, reps, nodes, c.PlacementSettings(ctx))
			if g.output == "json" {
				return writeJSON(os.Stdout, p)
			}
			printPlacement(p, len(args) == 0)
			return nil
		},
	}
	cmd.Flags().StringVar(&size, "size", "", "check a new volume of this size instead (e.g. 50Gi)")
	cmd.Flags().IntVar(&replicas, "replicas", 3, "with --size: number of replicas")
	cmd.Flags().StringSliceVar(&nodeSel, "node-selector", nil, "with --size: node tags the volume requires")
	cmd.Flags().StringSliceVar(&diskSel, "disk-selector", nil, "with --size: disk tags the volume requires")
	return cmd
}

func printPlacement(p *longhorn.Placement, hypothetical bool) {
	s := p.Settings
	if hypothetical {
		fmt.Printf("A new volume of %s with %d replicas\n", format.Bytes(p.Size), p.Want)
	} else {
		fmt.Printf("Volume %s: %s (actual %s), %d of %d replicas placed\n", p.Volume, format.Bytes(p.Size), format.Bytes(p.Actual), p.Have, p.Want)
		if !p.Scheduled {
			fmt.Printf("Longhorn: cannot place a replica – %s\n", p.ScheduledReason)
		}
	}
	fmt.Printf("settings: over-provisioning %d %%, minimal available %d %%, soft anti-affinity node %v / zone %v / disk %v\n\n",
		s.OverProvisioning, s.MinimalAvailable, s.NodeSoft, s.ZoneSoft, s.DiskSoft)
	w := newTab(os.Stdout)
	fmt.Fprintln(w, "NODE\tZONE\tVERDICT\tDISK\tAVAILABLE\tSCHEDULED / LIMIT\tWHY")
	for _, n := range p.Nodes {
		verdict := "ok"
		switch {
		case !n.OK:
			verdict = "NO"
		case n.Soft:
			verdict = "ok (soft)"
		}
		if len(n.Disks) == 0 || (!n.OK && n.Reason != "" && !diskReason(n)) {
			fmt.Fprintf(w, "%s\t%s\t%s\t-\t-\t-\t%s\n", n.Node, dash(n.Zone), verdict, dash(n.Reason))
			continue
		}
		for i, d := range n.Disks {
			name, zone, nv := n.Node, dash(n.Zone), verdict
			if i > 0 {
				name, zone, nv = "", "", ""
			}
			why := d.Reason
			switch {
			case why == "" && d.Soft:
				why = "holds a replica already (soft disk anti-affinity)"
			case why == "" && n.Reason != "":
				why = n.Reason
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s / %s\t%s\n", name, zone, nv, d.Path, format.Bytes(d.Available),
				format.Bytes(d.Scheduled), format.Bytes(d.Limit), dash(why))
		}
	}
	_ = w.Flush()
	fmt.Println()
	missing := p.Missing()
	switch {
	case hypothetical && p.Placeable() >= p.Want:
		fmt.Printf("✔ fits: %d nodes can take a replica (%d without relying on soft anti-affinity)\n", p.Placeable(), p.Fresh)
	case hypothetical:
		fmt.Printf("✘ does not fit: room for %d of %d replicas\n", p.Placeable(), p.Want)
	case missing == 0:
		fmt.Printf("✔ all replicas placed; a replacement could go to %d node(s)\n", p.Placeable())
	case p.Placeable() >= missing:
		fmt.Printf("✔ %d replica(s) missing and there is room – Longhorn places them (it retries every few seconds)\n", missing)
	default:
		fmt.Printf("✘ %d replica(s) missing, room for %d\n", missing, p.Placeable())
	}
	if p.Placeable() < max(missing, map[bool]int{true: p.Want}[hypothetical]) {
		for _, h := range p.Hints() {
			fmt.Println("  → " + h)
		}
	}
}

// diskReason: the node's reason comes from its disks (shown per disk).
func diskReason(n longhorn.NodeVerdict) bool {
	for _, d := range n.Disks {
		if d.Reason != "" && strings.Contains(n.Reason, d.Reason) {
			return true
		}
	}
	return false
}
