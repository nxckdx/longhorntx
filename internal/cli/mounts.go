package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/nxckdx/longhorntx/internal/format"
	"github.com/nxckdx/longhorntx/internal/longhorn"
)

func mountsCmd(g *globals) *cobra.Command {
	var opt longhorn.MountOptions
	noKubelet := false
	cmd := &cobra.Command{
		Use:   "mounts",
		Short: "Pods stuck mounting Longhorn volumes, per node – and what kubelet says about it",
		Long: `Longhorn can show a node and a volume as healthy while pods on that node never
get the volume mounted: kubelet or the Longhorn CSI plugin there hangs, the
node stays Ready, and nobody reports an error. This lists pods that wait
(ContainerCreating) for a Longhorn volume, how far each got:

  attach        the volume is not attached to the node yet (Longhorn side)
  attach-error  attaching failed
  mount         attached, but kubelet / the CSI plugin did not mount it (node side)

and per node: is Longhorn's CSI driver registered, is the longhorn-csi-plugin
pod ready, and – for nodes with pods stuck at "mount" and for --node –
kubelet's own view through the API server's node proxy (/healthz, its
volume manager: Longhorn volumes wanted vs. mounted, failed mount and CSI
calls). That needs get on nodes/proxy; without it lhtx says so and goes on.
The hint names the restart that fits the node (rke2, k3s, Talos, kubelet).

Reads: one LIST of pending pods (field selector), CSINodes, the CSI plugin
pods; for stuck pods their VolumeAttachments, events and nodes.`,
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
			nodes, err := c.Nodes(cmd.Context())
			if err != nil {
				return err
			}
			var names []string
			for _, n := range nodes {
				names = append(names, n.Name)
			}
			opt.Kubelet = !noKubelet
			rep, err := c.Mounts(cmd.Context(), nil, names, opt)
			if err != nil {
				return err
			}
			if g.output == "json" {
				return writeJSON(os.Stdout, rep)
			}
			return printMounts(rep)
		},
	}
	cmd.Flags().DurationVar(&opt.MinWait, "min-wait", 0, "count pods waiting at least this long (default 2m)")
	cmd.Flags().StringSliceVar(&opt.ProbeNodes, "node", nil, "also ask kubelet on these nodes (repeatable)")
	cmd.Flags().BoolVar(&noKubelet, "no-kubelet", false, "do not ask kubelet (node proxy)")
	return cmd
}

func printMounts(rep *longhorn.MountReport) error {
	w := newTab(os.Stdout)
	if len(rep.Stuck) == 0 {
		fmt.Fprintln(w, "No pod waits for a Longhorn volume.")
	} else {
		fmt.Fprintln(w, "NODE\tPOD\tPVC\tWAITING\tSTAGE\tDETAIL")
		for _, s := range rep.Stuck {
			d := s.Detail
			if s.NoEvents {
				d = "no events from kubelet; " + d
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", s.Node, s.Pod, s.PVC, format.Duration(s.Waiting), s.Stage, oneLine(d, 90))
		}
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "NODE\tREADY\tCSI DRIVER\tCSI PLUGIN\tSTUCK\tKUBELET")
	for _, nm := range rep.Nodes {
		ready := "-"
		if nm.Ready != nil {
			ready = fmt.Sprint(*nm.Ready)
		}
		plugin := "-"
		if nm.CSIPlugin != "" {
			plugin = "ready"
			if !nm.CSIPluginOK {
				plugin = "NOT READY"
			}
			if nm.CSIRestarts > 0 {
				plugin += fmt.Sprintf(" (%d restarts)", nm.CSIRestarts)
			}
		}
		stuck, kl := "-", "-"
		if nm.Stuck > 0 {
			stuck = fmt.Sprintf("%d (up to %s)", nm.Stuck, format.Duration(nm.Oldest))
		}
		if nm.Kubelet != nil {
			kl = nm.Kubelet.Summary()
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", nm.Node, ready, nm.CSI, plugin, stuck, kl)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	for _, f := range longhorn.MountFindings(*rep) {
		fmt.Printf("\n%s %s: %s\n  → %s\n", f.Severity, f.Object, f.Message, f.Hint)
	}
	return nil
}
