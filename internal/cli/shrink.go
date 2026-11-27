package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/nxckdx/longhorntx/internal/longhorn"
	"github.com/nxckdx/longhorntx/internal/shrink"
	"github.com/nxckdx/longhorntx/internal/switchover"
)

func shrinkCmd(g *globals) *cobra.Command {
	var image string
	var wait time.Duration
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
Databases are better shrunk with their own tools (dump/restore, CNPG).
lhtx must keep running until the end (Ctrl+C before the swap rolls back).`,
		Example: `  lhtx --rw shrink shop/data 5Gi
  lhtx shrink shop/data 5Gi --dry-run`,
		Args:              cobra.ExactArgs(2),
		ValidArgsFunction: g.completeVolumes,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			ns, pvc, ok := strings.Cut(args[0], "/")
			if !ok || ns == "" || pvc == "" {
				return fmt.Errorf("give the PVC as NAMESPACE/NAME")
			}
			q, err := resource.ParseQuantity(args[1])
			if err != nil {
				return fmt.Errorf("size %q: %w", args[1], err)
			}
			if image == "" {
				if Version == "dev" {
					return fmt.Errorf("a development build does not know its image: give --image")
				}
				image = "ghcr.io/nxckdx/lhtx:" + strings.TrimPrefix(Version, "v")
			}
			c, err := g.client()
			if err != nil {
				return err
			}
			p, err := shrink.Build(ctx, c, ns, pvc, q.Value(), image, time.Now())
			if err != nil {
				return err
			}
			sw := p.Switch
			fmt.Printf("Shrink PVC %s from %s to %s · %s\n\n", sw.PVCRef(), humanSize(p.OldSize), humanSize(p.NewSize), c.Cluster.Context)
			if p.Used >= 0 {
				fmt.Printf("  used: %s (%s)\n\n", humanSize(p.Used), p.UsedFrom)
			}
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
			fmt.Println("\nThe apps using the PVC are down from the freeze until the unfreeze at the end (while copying).")
			if err := g.confirm(true, sw.PVCRef()); err != nil {
				return err
			}
			start := time.Now()
			journal := switchover.NewJournalPath(sw)
			err = shrink.Run(ctx, c, p, shrink.Options{Journal: journal, Wait: wait,
				Log: func(f string, a ...any) {
					fmt.Printf("%6s  %s\n", time.Since(start).Round(time.Second), fmt.Sprintf(f, a...))
				}})
			if journal != "" {
				fmt.Println("journal:", journal)
			}
			return err
		},
	}
	cmd.Flags().StringVar(&image, "image", "", "image of the copy pod (default: ghcr.io/nxckdx/lhtx:<this version>)")
	cmd.Flags().DurationVar(&wait, "wait", 10*time.Minute, "how long to wait for pods to stop / start and volumes to detach")
	return cmd
}
