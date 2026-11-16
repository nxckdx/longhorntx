package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/nxckdx/longhorntx/internal/format"
	"github.com/nxckdx/longhorntx/internal/longhorn"
)

func nodesCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:     "nodes",
		Aliases: []string{"node", "no", "disks"},
		Short:   "List nodes and their disks with usage and replica count",
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
			st, err := loadState(cmd, c)
			if err != nil {
				return err
			}
			nodes := longhorn.BuildNodes(st)
			if g.output == "json" {
				return writeJSON(os.Stdout, nodes)
			}
			wide := g.output == "wide"
			w := newTab(os.Stdout)
			hdr := "NODE\tSTATUS\tDISK\tTYPE\tUSED\tSIZE\tSCHEDULED\tREPLICAS\tSMART"
			if wide {
				hdr += "\tNAME\tUUID\tMODEL\tPOWER-ON"
			}
			fmt.Fprintln(w, hdr)
			for _, n := range nodes {
				status := nodeStatus(&n)
				if len(n.Disks) == 0 {
					fmt.Fprintf(w, "%s\t%s\t-\t-\t-\t-\t-\t0\t-\n", n.Name, status)
				}
				for _, d := range n.Disks {
					row := []string{
						n.Name, status, diskLabel(&d), dash(d.Type),
						usage(d.Used(), d.Maximum), format.Bytes(d.Maximum),
						usage(d.Scheduled, d.Maximum-d.Reserved),
						fmt.Sprint(len(d.Replicas)), smartShort(d.SMART),
					}
					if wide {
						model, hours := "-", "-"
						if d.SMART != nil {
							model = dash(d.SMART.Model)
							if d.SMART.PowerOnHours > 0 {
								hours = fmt.Sprintf("%dh", d.SMART.PowerOnHours)
							}
						}
						row = append(row, d.Name, d.UUID, model, hours)
					}
					fmt.Fprintln(w, strings.Join(row, "\t"))
				}
			}
			return w.Flush()
		},
	}
}

func diskCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "disk NODE[:PATH|NAME]",
		Short: "List the replicas on a node's disk(s)",
		Long: `List all replicas stored on a disk – the reverse of "lhtx replicas".

Without :PATH all disks of the node are shown.
Example: lhtx disk carlos:/var/mnt/longhorn-volume`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := g.checkOutput(); err != nil {
				return err
			}
			node, sel, _ := strings.Cut(args[0], ":")
			c, err := g.client()
			if err != nil {
				return err
			}
			defer g.reportTraffic(c)
			st, err := loadState(cmd, c)
			if err != nil {
				return err
			}
			var disks []longhorn.DiskView
			found := false
			for _, n := range longhorn.BuildNodes(st) {
				if n.Name != node {
					continue
				}
				found = true
				for _, d := range n.Disks {
					if sel == "" || d.Path == sel || d.Name == sel || d.UUID == sel {
						disks = append(disks, d)
					}
				}
			}
			if !found {
				return fmt.Errorf("node %q not found", node)
			}
			if len(disks) == 0 {
				return fmt.Errorf("node %s has no disk %q", node, sel)
			}
			if g.output == "json" {
				return writeJSON(os.Stdout, disks)
			}
			w := newTab(os.Stdout)
			fmt.Fprintln(w, "DISK\tPVC\tVOLUME\tREPLICA\tSIZE\tHEALTH")
			for _, d := range disks {
				for _, r := range d.Replicas {
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", diskLabel(&d), dash(r.PVC), r.Volume, r.Replica, format.Bytes(r.Size), r.Health)
				}
			}
			return w.Flush()
		},
	}
}

// loadState reads volumes, replicas and nodes (no engines).
func loadState(cmd *cobra.Command, c *longhorn.Client) (longhorn.State, error) {
	ctx := cmd.Context()
	var st longhorn.State
	var err error
	if st.Volumes, err = c.Volumes(ctx); err != nil {
		return st, err
	}
	if st.Replicas, err = c.Replicas(ctx, ""); err != nil {
		return st, err
	}
	st.Nodes, err = c.Nodes(ctx)
	return st, err
}

func nodeStatus(n *longhorn.NodeView) string {
	var s []string
	if n.Ready {
		s = append(s, "Ready")
	} else {
		s = append(s, "NotReady")
	}
	if !n.Schedulable {
		s = append(s, "NoSched")
	}
	if n.Eviction {
		s = append(s, "Evicting")
	}
	return strings.Join(s, ",")
}

func diskLabel(d *longhorn.DiskView) string {
	if d.Path != "" {
		return d.Path
	}
	return d.Name
}

func usage(used, total int64) string {
	if total <= 0 {
		return format.Bytes(used)
	}
	return fmt.Sprintf("%s (%d%%)", format.Bytes(used), used*100/total)
}

func smartShort(s *longhorn.SMARTSummary) string {
	if s == nil {
		return "-"
	}
	parts := []string{dash(s.Status)}
	if s.Temperature > 0 {
		parts = append(parts, fmt.Sprintf("%d°C", s.Temperature))
	}
	if s.LifetimeLeft >= 0 {
		parts = append(parts, fmt.Sprintf("life %d%%", s.LifetimeLeft))
	}
	return strings.Join(parts, " ")
}
