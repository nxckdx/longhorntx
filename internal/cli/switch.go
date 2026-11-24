package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/nxckdx/longhorntx/internal/longhorn"
	"github.com/nxckdx/longhorntx/internal/switchover"
)

func switchCmd(g *globals) *cobra.Command {
	var to string
	var wait time.Duration
	cmd := &cobra.Command{
		Use:   "switch NAMESPACE/PVC --to-volume VOLUME",
		Short: "Point a PVC to another Longhorn volume (restored/cloned), workload and Flux included (needs --rw)",
		Long: `Switches a PVC to another Longhorn volume – typically one restored from a
backup (lhtx restore) – and takes care of everything around it:

  1. wait until the target volume is restored and detached
  2. suspend the Flux Kustomizations/HelmReleases that manage the workload or
     the PVC (spec.suspend, like flux suspend; already suspended ones stay)
  3. scale the Deployments/StatefulSets that mount the PVC to 0, wait
  4. set the old PV to Retain, delete the old PVC
  5. create a PV for the target volume and the PVC again (same name, labels,
     annotations – Flux and Helm keep owning it), wait until Bound
  6. scale back up, resume Flux with a reconcile request

The old volume stays: switch back with the same command and --to-volume OLD.
Until the old PVC is deleted, any failure (or Ctrl+C) is rolled back
automatically. Refused: workloads of operators (CloudNativePG, Prometheus
operator, …), HPAs, bare pods, Argo CD / Flux Operator ResourceSets.

Every step is written to a journal in $XDG_STATE_HOME/lhtx/switch/.
--dry-run shows the plan only.`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: g.completeVolumes,
		RunE: func(cmd *cobra.Command, args []string) error {
			ns, pvc, ok := strings.Cut(args[0], "/")
			if !ok || ns == "" || pvc == "" {
				return fmt.Errorf("give the PVC as NAMESPACE/NAME")
			}
			if to == "" {
				return fmt.Errorf("--to-volume is required")
			}
			c, err := g.client()
			if err != nil {
				return err
			}
			p, err := switchover.Build(cmd.Context(), c, ns, pvc, to)
			if err != nil {
				return err
			}
			return g.runSwitch(cmd.Context(), c, p, nil, wait)
		},
	}
	cmd.Flags().StringVar(&to, "to-volume", "", "the Longhorn volume the PVC should use")
	cmd.Flags().DurationVar(&wait, "wait", 10*time.Minute, "how long to wait for pods to stop / the PVC to bind")
	return cmd
}

// runSwitch previews (restore action if any, then the switch steps),
// confirms by typing the PVC, applies the restore and runs the switch.
func (g *globals) runSwitch(ctx context.Context, c *longhorn.Client, p *switchover.Plan, restore *longhorn.Action, wait time.Duration) error {
	title := fmt.Sprintf("Switch PVC %s from volume %s to %s", p.PVCRef(), p.OldVolume, p.NewVolume)
	if restore != nil {
		title = restore.Title + ", then switch PVC " + p.PVCRef() + " to it"
	}
	fmt.Printf("%s · %s\n\n", title, c.Cluster.Context)
	n := 1
	if restore != nil {
		fmt.Printf("  %2d. %s\n", n, restore.Summary())
		n++
	}
	for _, s := range p.Steps() {
		fmt.Printf("  %2d. %s\n", n, s)
		n++
	}
	fmt.Println()
	if restore != nil {
		for _, w := range restore.Warnings {
			fmt.Println("⚠ " + w)
		}
	}
	for _, w := range p.Warnings {
		fmt.Println("⚠ " + w)
	}
	if len(p.Blockers) > 0 {
		for _, b := range p.Blockers {
			fmt.Println("✖ " + b)
		}
		return fmt.Errorf("the switch is not possible (see ✖)")
	}
	mode, err := g.dryRun()
	if err != nil {
		return err
	}
	switch mode {
	case longhorn.DryRunClient:
		fmt.Println("\n(dry run: nothing changed)")
		return nil
	case longhorn.DryRunServer:
		if restore != nil {
			if err := c.Apply(ctx, restore, longhorn.DryRunServer); err != nil {
				return fmt.Errorf("server dry run rejected the restore: %w", err)
			}
			fmt.Println("\n✔ the restore volume is accepted by the server (dry run); the switch steps depend on each other and cannot be dry-run")
			return nil
		}
		return fmt.Errorf("--dry-run=server is not possible for the switch (its steps depend on each other): use --dry-run")
	}
	fmt.Println("\nThe workload is down from step \"scale … to 0\" until the PVC is bound again.")
	if err := g.confirm(true, p.PVCRef()); err != nil {
		return err
	}
	if restore != nil {
		if err := c.Apply(ctx, restore, longhorn.DryRunNone); err != nil {
			return err
		}
		fmt.Printf("✔ %s\n", restore.Title)
	}
	journal := switchJournal(p)
	start := time.Now()
	err = switchover.Run(ctx, c, p, switchover.Options{
		Journal: journal, Wait: wait,
		Log: func(f string, a ...any) {
			fmt.Printf("%6s  %s\n", time.Since(start).Round(time.Second), fmt.Sprintf(f, a...))
		},
	})
	if journal != "" {
		fmt.Println("journal:", journal)
	}
	return err
}

func switchJournal(p *switchover.Plan) string {
	dir := os.Getenv("XDG_STATE_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(dir, "lhtx", "switch", fmt.Sprintf("%s-%s-%s.json", time.Now().Format("20060102-150405"), p.Namespace, p.PVC))
}
