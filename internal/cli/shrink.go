package cli

import (
	"fmt"

	"github.com/nxckdx/longhorntx/internal/agent"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/nxckdx/longhorntx/internal/longhorn"
	"github.com/nxckdx/longhorntx/internal/shrink"
	"github.com/nxckdx/longhorntx/internal/switchover"
	"github.com/nxckdx/longhorntx/internal/tui"
)

func shrinkCmd(g *globals) *cobra.Command {
	var image string
	var pullSecrets []string
	var wait time.Duration
	var abort, local, noPrecopy bool
	cmd := &cobra.Command{
		Use:   "shrink NAMESPACE/PVC SIZE",
		Short: "Make a volume smaller: new volume, copy the files, swap the PVC (needs --rw; the app is down while copying)",
		Long: `Neither Longhorn nor Kubernetes can shrink a volume. lhtx does what you would
do by hand, checked and with a way back:

  1. create a new, smaller Longhorn volume with the same settings (replicas,
     data locality, recurring jobs, encryption with the same key)
  2. freeze the PVC (like lhtx freeze: its pods stop, nothing can start)
  3. copy all files in a pod running lhtx's own image (lhtx copy: owners,
     modes, hard and symbolic links, xattrs/ACLs, holes) and compare both trees
  4. swap the PVC to the new volume and unfreeze

Until the old PVC is deleted (step 4) every failure rolls back – the app
starts again on the old volume, the new one is removed. The old volume is
kept as the way back (lhtx --rw switch NS/PVC --to-volume OLD).

The app is down from the freeze to the unfreeze: about as long as copying
the used data takes. The copy pod runs as root (it must keep file owners);
namespaces enforcing the "restricted" Pod Security level are refused.
Databases are better shrunk with their own tools (dump/restore).
lhtx must keep running until the end (Ctrl+C before the swap rolls back).`,
		Example: `  lhtx --rw shrink shop/data 5Gi
  lhtx shrink shop/data 5Gi --dry-run`,
		Args:              cobra.RangeArgs(1, 2),
		ValidArgsFunction: g.completeVolumes,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			ns, pvc, ok := strings.Cut(args[0], "/")
			if !ok || ns == "" || pvc == "" {
				return fmt.Errorf("give the PVC as NAMESPACE/NAME")
			}
			if abort {
				return g.shrinkAbort(cmd, ns, pvc)
			}
			if len(args) != 2 {
				return fmt.Errorf("give the new size: lhtx shrink NAMESPACE/PVC SIZE")
			}
			q, err := resource.ParseQuantity(args[1])
			if err != nil {
				return fmt.Errorf("size %q: %w", args[1], err)
			}
			var imageErr error // only needed when lhtx copies itself (no agent)
			if image == "" {
				if image, imageErr = tui.ShrinkImage(Version); imageErr != nil {
					imageErr = fmt.Errorf("%w (or give --image)", imageErr)
				}
			}
			c, err := g.client()
			if err != nil {
				return err
			}
			p, err := shrink.Build(ctx, c, ns, pvc, q.Value(), shrink.BuildOptions{Image: image, PullSecrets: pullSecrets, NoPrecopy: noPrecopy}, time.Now())
			if err != nil {
				return err
			}
			sw := p.Switch
			fmt.Printf("Shrink PVC %s from %s to %s · %s\n\n", sw.PVCRef(), humanSize(p.OldSize), humanSize(p.NewSize), c.Cluster.Context)
			if p.Used >= 0 {
				fmt.Printf("  used: %s (%s)\n", humanSize(p.Used), p.UsedFrom)
			}
			if p.Node != "" {
				fmt.Printf("  copy on node: %s (%s)\n", p.Node, p.NodeWhy)
			}
			fmt.Println()
			for i, s := range p.Steps() {
				fmt.Printf("  %2d. %s\n", i+1, s)
			}
			fmt.Println()
			for _, w := range p.Warnings {
				fmt.Println("⚠ " + w)
			}
			if len(p.Blockers) > 0 {
				for _, b := range p.Blockers {
					fmt.Println("✖ " + b)
				}
				return fmt.Errorf("the shrink is not possible (see ✖)")
			}
			mode, err := g.dryRun()
			if err != nil {
				return err
			}
			if mode != longhorn.DryRunNone {
				fmt.Println("\n(dry run: nothing changed; the steps depend on each other, there is no server dry run)")
				return nil
			}
			byAgent := false
			if !local && !cmd.Flags().Changed("image") {
				if ag := agent.DetectRW(ctx, c); ag != nil && agent.SwitchesAvailable(ctx, c, ns) {
					byAgent = true
					fmt.Printf("\ncarried out by the lhtx agent %s with its own image – lhtx only follows (Ctrl+C stops following, not the shrink; --local runs it here)\n", ag.Holder)
				}
			}
			fmt.Println("\nThe apps using the PVC are down from the freeze until the unfreeze at the end (while copying).")
			if err := g.confirm(true, sw.PVCRef()); err != nil {
				return err
			}
			if byAgent {
				name, err := agent.RequestSwitch(ctx, c, ns, agent.SwitchSpec{PVC: pvc, ShrinkTo: args[1], Wait: wait.String(), ImagePullSecrets: pullSecrets, NoPrecopy: noPrecopy})
				if err != nil {
					return err
				}
				fmt.Printf("requested Switch %s/%s (shrink) from the agent\n", ns, name)
				res, err := agent.FollowSwitch(ctx, c, ns, name, plainLog)
				switch {
				case ctx.Err() != nil:
					fmt.Printf("\nstopped following – the agent goes on (kubectl -n %s get switch %s)\n", ns, name)
					return nil
				case err != nil:
					return err
				case res.Status.Phase != agent.SwitchDone:
					return fmt.Errorf("shrink %s: %s", res.Status.Phase, res.Status.Message)
				}
				return nil
			}
			if imageErr != nil {
				return imageErr
			}
			start := time.Now()
			journal := switchover.NewJournalPath(sw)
			err = shrink.Run(ctx, c, p, shrink.Options{Journal: journal, Wait: wait,
				Log: func(f string, a ...any) {
					fmt.Printf("%6s  %s\n", time.Since(start).Round(time.Second), fmt.Sprintf(f, a...))
				}})
			if _, serr := os.Stat(journal); journal != "" && serr == nil {
				fmt.Println("journal:", journal)
			}
			if err == nil {
				fmt.Printf("\nThe old volume %s (%s) still takes its space on the disks. When you no longer need the way back:\n"+
					"  kubectl delete pv %s && kubectl -n %s delete volumes.longhorn.io %s\n",
					sw.OldVolume, humanSize(p.OldSize), sw.OldPV, c.Namespace, sw.OldVolume)
			}
			return err
		},
	}
	cmd.Flags().StringVar(&image, "image", "", "image of the copy pod (default: env LHTX_SHRINK_IMAGE, else ghcr.io/nxckdx/lhtx:<this version>)")
	cmd.Flags().StringSliceVar(&pullSecrets, "image-pull-secret", nil, "pull secret(s) in the PVC's namespace for the copy pod's image")
	cmd.Flags().BoolVar(&noPrecopy, "no-precopy", false, "no copy while the app still runs: everything is copied while it is down (longer downtime, no extra reads beforehand)")
	cmd.Flags().BoolVar(&local, "local", false, "do not hand the shrink to the lhtx agent, even if one runs (lhtx must then run until the end)")
	cmd.Flags().BoolVar(&abort, "abort", false, "clean up an interrupted shrink of the PVC (copy pod, temporary PVC/PV, new volume) and unfreeze it")
	cmd.Flags().DurationVar(&wait, "wait", 10*time.Minute, "how long to wait for pods to stop / start and volumes to detach")
	return cmd
}

func (g *globals) shrinkAbort(cmd *cobra.Command, ns, pvc string) error {
	ctx := cmd.Context()
	c, err := g.client()
	if err != nil {
		return err
	}
	l, err := shrink.FindLeftovers(ctx, c, ns, pvc)
	if err != nil {
		return err
	}
	if l.Problem != "" {
		return fmt.Errorf("%s", l.Problem)
	}
	if l.Empty() {
		fmt.Printf("Nothing left over from a shrink of %s/%s.\n", ns, pvc)
		if n := switchover.MarkRolledBack(switchover.JournalDir(), c.Cluster.Context, ns+"/"+pvc, "aborted by lhtx shrink --abort"); n > 0 {
			fmt.Printf("✔ %d unfinished journal(s) closed as rolled back\n", n)
		}
		return nil
	}
	fmt.Printf("Clean up the interrupted shrink of %s/%s · %s\n\n", ns, pvc, c.Cluster.Context)
	for i, s := range l.Lines() {
		fmt.Printf("  %2d. %s\n", i+1, s)
	}
	if mode, err := g.dryRun(); err != nil || mode != longhorn.DryRunNone {
		fmt.Println("\n(dry run: nothing changed)")
		return err
	}
	if err := g.confirm(true, ns+"/"+pvc); err != nil {
		return err
	}
	if err := shrink.Abort(ctx, c, l, func(f string, a ...any) { fmt.Printf(f+"\n", a...) }); err != nil {
		return err
	}
	if n := switchover.MarkRolledBack(switchover.JournalDir(), c.Cluster.Context, ns+"/"+pvc, "aborted by lhtx shrink --abort"); n > 0 {
		fmt.Printf("✔ %d unfinished journal(s) closed as rolled back\n", n)
	}
	return nil
}
