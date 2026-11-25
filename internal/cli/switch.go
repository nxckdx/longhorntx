package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/nxckdx/longhorntx/internal/agent"
	"github.com/nxckdx/longhorntx/internal/longhorn"
	"github.com/nxckdx/longhorntx/internal/switchover"
)

func switchCmd(g *globals) *cobra.Command {
	var to, fromBackup string
	var wait time.Duration
	var status, local bool
	cmd := &cobra.Command{
		Use:   "switch NAMESPACE/PVC --to-volume VOLUME | --from-backup BACKUP",
		Short: "Point a PVC to another Longhorn volume (restored/cloned) while its apps are frozen (needs --rw)",
		Long: `Switches a PVC to another Longhorn volume – typically one restored from a
backup (lhtx restore) – and takes care of everything around it:

  1. wait until the target volume is restored and detached
  2. freeze the PVC (see lhtx freeze --help): an admission policy keeps pods
     from mounting it, its pods are stopped in order with their hooks –
     nothing is scaled, operators and GitOps keep their desired state
  3. set the old PV to Retain, delete the old PVC
  4. create a PV for the target volume and the PVC again (same name, labels,
     annotations – Flux, Argo CD and Helm keep owning it), wait until Bound
  5. unfreeze: the owners start their pods again on the new volume

The old volume stays: switch back with the same command and --to-volume OLD.
Until the old PVC is deleted, any failure (or Ctrl+C) is rolled back
automatically; after that the PVC stays frozen until the switch is done.
CloudNativePG instances are allowed with a warning (one instance from a
backup is rarely consistent with the others).

Every step is written to a journal in $XDG_STATE_HOME/lhtx/switch/.
--dry-run shows the plan only.

With the lhtx agent in the cluster the switch runs there (lhtx follows; it
continues after the agent restarts). Then people with only namespace rights
can switch their own PVCs too: bind the ClusterRole lhtx-switch with a
RoleBinding; the agent checks that they may delete/create the PVC and delete
its pods and that only the PVC's own data is used (--from-backup: a backup
of it; --to-volume: a volume it had, or a restore/clone of one).`,
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: g.completeVolumes,
		RunE: func(cmd *cobra.Command, args []string) error {
			if status {
				return printSwitchStatus()
			}
			if len(args) != 1 {
				return fmt.Errorf("give the PVC as NAMESPACE/NAME (or --status)")
			}
			ns, pvc, ok := strings.Cut(args[0], "/")
			if !ok || ns == "" || pvc == "" {
				return fmt.Errorf("give the PVC as NAMESPACE/NAME")
			}
			if (to == "") == (fromBackup == "") {
				return fmt.Errorf("give --to-volume VOLUME or --from-backup BACKUP")
			}
			c, err := g.client()
			if err != nil {
				return err
			}
			if fromBackup != "" {
				return g.switchFromBackup(cmd, c, ns, pvc, fromBackup, wait, local)
			}
			p, err := switchover.Build(cmd.Context(), c, ns, pvc, to)
			if apierrors.IsForbidden(err) && !local {
				return g.switchViaAgentOnly(cmd, c, ns, agent.SwitchSpec{PVC: pvc, ToVolume: to, Wait: wait.String()}, err)
			}
			if err != nil {
				return err
			}
			return g.runSwitch(cmd.Context(), c, p, nil, wait, local, switchTarget{})
		},
	}
	cmd.Flags().StringVar(&to, "to-volume", "", "the Longhorn volume the PVC should use")
	cmd.Flags().StringVar(&fromBackup, "from-backup", "", "restore this backup into a new volume first (like restore --switch)")
	cmd.Flags().DurationVar(&wait, "wait", 10*time.Minute, "how long to wait for pods to stop / the PVC to bind")
	cmd.Flags().BoolVar(&local, "local", false, "run the switch here even if an lhtx agent could do it")
	cmd.Flags().BoolVar(&status, "status", false, "list the switches of this machine (journals), newest first")
	return cmd
}

// runSwitch previews (restore action if any, then the switch steps),
// confirms by typing the PVC, applies the restore and runs the switch.
// switchTarget: what the agent needs to redo the switch (the volume, or the
// backup to restore and the new volume's name and replicas).
type switchTarget struct {
	backup   string
	replicas int
}

func (g *globals) runSwitch(ctx context.Context, c *longhorn.Client, p *switchover.Plan, restore *longhorn.Action, wait time.Duration, local bool, tgt switchTarget) error {
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
	byAgent := false
	if !local {
		if ag := agent.DetectRW(ctx, c); ag != nil && agent.SwitchesAvailable(ctx, c, p.Namespace) {
			byAgent = true
			fmt.Printf("\ncarried out by the lhtx agent %s – lhtx only follows (Ctrl+C stops following, not the switch; --local runs it here)\n", ag.Holder)
		}
	}
	fmt.Println("\nThe apps using the PVC are down from the freeze until the unfreeze at the end.")
	if err := g.confirm(true, p.PVCRef()); err != nil {
		return err
	}
	if byAgent {
		spec := agent.SwitchSpec{PVC: p.PVC, Wait: wait.String()}
		if restore != nil {
			spec.FromBackup, spec.VolumeName, spec.Replicas = tgt.backup, p.NewVolume, tgt.replicas
		} else {
			spec.ToVolume = p.NewVolume
		}
		name, err := agent.RequestSwitch(ctx, c, p.Namespace, spec)
		if err != nil {
			return err
		}
		fmt.Printf("requested Switch %s/%s from the agent\n", p.Namespace, name)
		sw, err := agent.FollowSwitch(ctx, c, p.Namespace, name, plainLog)
		switch {
		case ctx.Err() != nil:
			fmt.Printf("\nstopped following – the agent goes on (kubectl -n %s get switch %s)\n", p.Namespace, name)
			return nil
		case err != nil:
			return err
		case sw.Status.Phase != agent.SwitchDone:
			return fmt.Errorf("switch %s: %s", sw.Status.Phase, sw.Status.Message)
		}
		return nil
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

// switchFromBackup: restore --switch by backup name; without the rights to
// plan it here (namespace users) the agent does it.
func (g *globals) switchFromBackup(cmd *cobra.Command, c *longhorn.Client, ns, pvc, backup string, wait time.Duration, local bool) error {
	ctx := cmd.Context()
	b, err := c.Backup(ctx, backup)
	if apierrors.IsForbidden(err) && !local {
		return g.switchViaAgentOnly(cmd, c, ns, agent.SwitchSpec{PVC: pvc, FromBackup: backup, Wait: wait.String()}, err)
	}
	if err != nil {
		return err
	}
	p, err := c.PlanRestore(ctx, b, time.Now())
	if err != nil {
		return err
	}
	p.Spec.Name = longhorn.RestoreName(ns+"/"+pvc, b.Volume(), time.Now())
	return g.restoreAndSwitch(cmd, c, p, wait, local, ns+"/"+pvc)
}

// switchViaAgentOnly hands a switch to the agent without a local plan: the
// user may switch the PVC (namespace rights) but not read Longhorn.
func (g *globals) switchViaAgentOnly(cmd *cobra.Command, c *longhorn.Client, ns string, spec agent.SwitchSpec, why error) error {
	ctx := cmd.Context()
	ag := agent.DetectRW(ctx, c)
	if ag == nil || !agent.SwitchesAvailable(ctx, c, ns) {
		return fmt.Errorf("%w (without these rights only the lhtx agent can switch it – none found that does switches)", why)
	}
	what := "volume " + spec.ToVolume
	if spec.FromBackup != "" {
		what = "a restore of backup " + spec.FromBackup
	}
	fmt.Printf("Switch PVC %s/%s to %s · %s\n\n", ns, spec.PVC, what, c.Cluster.Context)
	fmt.Printf("  no preview: you may not read Longhorn (%v)\n", why)
	fmt.Printf("  the lhtx agent %s plans and runs it: freeze the PVC, re-create it on the new volume, unfreeze\n", ag.Holder)
	fmt.Println("  it checks that you may delete/create the PVC and delete its pods, and that only this PVC's own data is used")
	fmt.Println("\n⚠ the apps using the PVC are down from the freeze until the unfreeze at the end")
	if mode, _ := g.dryRun(); mode != longhorn.DryRunNone {
		fmt.Println("\n(dry run: nothing changed)")
		return nil
	}
	if err := g.confirm(true, ns+"/"+spec.PVC); err != nil {
		return err
	}
	name, err := agent.RequestSwitch(ctx, c, ns, spec)
	if err != nil {
		return err
	}
	fmt.Printf("requested Switch %s/%s from the agent\n", ns, name)
	sw, err := agent.FollowSwitch(ctx, c, ns, name, plainLog)
	switch {
	case ctx.Err() != nil:
		fmt.Printf("\nstopped following – the agent goes on (kubectl -n %s get switch %s)\n", ns, name)
		return nil
	case err != nil:
		return err
	case sw.Status.Phase != agent.SwitchDone:
		return fmt.Errorf("switch %s: %s", sw.Status.Phase, sw.Status.Message)
	}
	return nil
}

func switchJournal(p *switchover.Plan) string { return switchover.NewJournalPath(p) }

func printSwitchStatus() error {
	dir := switchover.JournalDir()
	js := switchover.Journals(dir)
	if len(js) == 0 {
		fmt.Println("no switches recorded in", dir)
		return nil
	}
	w := newTab(os.Stdout)
	fmt.Fprintln(w, "WHEN\tCONTEXT\tPVC\tFROM\tTO\tRESULT")
	for _, e := range js {
		res := "✔ finished"
		switch {
		case e.RolledBack:
			res = "↺ rolled back: " + e.Error
		case e.Error != "":
			res = "✖ " + e.Error
		case !e.Finished:
			res = "… not finished (last: " + e.LastStep + ")"
		}
		if len(res) > 100 {
			res = res[:97] + "…"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", e.At.Local().Format("2006-01-02 15:04"), dash(e.Context), e.PVC, e.From, e.To, res)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	fmt.Println("\njournals:", dir)
	return nil
}
