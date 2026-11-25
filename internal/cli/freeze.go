package cli

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/nxckdx/longhorntx/internal/agent"
	"github.com/nxckdx/longhorntx/internal/longhorn"
	"github.com/nxckdx/longhorntx/internal/switchover"
)

func freezeCmds(g *globals) []*cobra.Command {
	var all, status, local bool
	var node, reason string
	var wait, autoUnfreeze time.Duration
	freeze := &cobra.Command{
		Use:   "freeze [NS/PVC…] [--node NODE | --all]",
		Short: "Stop everything that uses Longhorn volumes, without scaling anything (needs --rw)",
		Long: `Freezes PVCs: a ValidatingAdmissionPolicy (Kubernetes ≥ 1.30, enforced by
the API server, no webhook server) denies new pods that mount them and
creating/deleting the PVCs by anyone else; then their pods are deleted and
lhtx waits until the volumes are detached.

Nothing in the desired state changes – Deployments, StatefulSets, operators,
Flux and Argo CD keep their specs and just cannot start pods. So no operator
deletes its cluster because of a scale-down, and no GitOps tool reverts
anything. Made for Longhorn upgrades and other maintenance:

  lhtx --rw freeze --all --reason "longhorn 1.12 → 1.13"   # detach everything
  … upgrade Longhorn …
  lhtx --rw unfreeze --all                                 # pods start again

  lhtx --rw freeze --node worker-3                          # volumes attached there
  lhtx --rw freeze shop/db-data shop/uploads

Clean shutdown of clustered apps: each app's pods are stopped one by one,
highest ordinal first (like a StatefulSet scales down), each with its normal
grace period; apps run in parallel. CloudNativePG clusters are hibernated
(cnpg.io/hibernation: on) so the operator stops them itself. Hooks add app
knowledge – a command run with sh -c via pods/exec in one pod of the app:

  lhtx.io/pre-freeze:    "[container:] command"   before its pods stop
  lhtx.io/post-unfreeze: "[container:] command"   once a pod is ready again
  lhtx.io/hook-timeout:  "2m"

lhtx looks for them on the pod, then up the owners (ReplicaSet → Deployment,
StatefulSet, the operator's resource), then in the ConfigMap lhtx-hooks in
the Longhorn namespace (key hooks.yaml) for charts that cannot annotate:

  - namespace: search
    selector: {app.kubernetes.io/name: opensearch}
    container: opensearch
    preFreeze: curl -s -XPUT localhost:9200/_cluster/settings -H 'Content-Type: application/json' -d '{"persistent":{"cluster.routing.allocation.enable":"primaries"}}' && curl -s -XPOST localhost:9200/_flush
    postUnfreeze: curl -s -XPUT localhost:9200/_cluster/settings -H 'Content-Type: application/json' -d '{"persistent":{"cluster.routing.allocation.enable":null}}'

A failing pre-freeze hook stops the freeze before anything is stopped.

unfreeze removes the policy and nudges the owners of the deleted pods
(annotation lhtx.io/unfrozen-at) so they start their pods at once instead of
after their back-off, then waits until the volumes are attached again.
The freeze lives in the cluster (lhtx freeze --status, lhtx doctor) – it can
be ended from any machine. Needs cluster-admin rights.

With the lhtx agent in the cluster (lhtx agent --help) lhtx hands the freeze
to it and only follows its progress – Ctrl+C or a lost connection does not
stop it halfway – and --auto-unfreeze ends it on its own. Needs only the
right to create Freeze resources then. --local skips the agent.`,
		ValidArgsFunction: g.completeVolumes,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, err := g.client()
			if err != nil {
				return err
			}
			if status {
				return printFreezes(cmd, c)
			}
			pvcs, err := selectPVCs(cmd, c, args, all, node)
			if err != nil {
				return err
			}
			p, err := switchover.PlanFreeze(ctx, c, pvcs, reason)
			if apierrors.IsForbidden(err) && !local {
				// may create Freezes but not read pods/volumes: the agent
				// plans it and logs every step
				if ag := agent.Detect(ctx, c); ag != nil {
					return requestBlind(cmd, g, c, ag, pvcs, reason, wait, autoUnfreeze, err)
				}
			}
			if err != nil {
				return err
			}
			fmt.Printf("Freeze %d PVCs · %s\n\n", len(p.PVCs), c.Cluster.Context)
			fmt.Printf("  PVCs:   %s\n", strings.Join(short(p.PVCs, 8), ", "))
			fmt.Printf("  pods:   %d to stop\n", len(p.Pods))

			fmt.Printf("  volumes attached now: %d – lhtx waits until they are detached\n", len(p.Attached))
			fmt.Println("\n  apps (stopped in parallel, each in order; pods get their normal grace period):")
			for _, l := range p.GroupLines() {
				fmt.Println("    " + l)
			}
			if with, without := p.HookCount(); without > 0 {
				fmt.Printf("  %d of %d apps have no hooks – add lhtx.io/pre-freeze / lhtx.io/post-unfreeze if they need a clean shutdown (lhtx freeze --help)\n",
					without, with+without)
			}
			ag := (*agent.Info)(nil)
			if !local {
				ag = agent.Detect(ctx, c)
			}
			if ag != nil {
				fmt.Printf("  policy: deny pods mounting them, deny PVC create/delete except by the agent\n")
				fmt.Printf("  carried out by the lhtx agent %s – lhtx only follows (Ctrl+C stops following, not the freeze)\n", ag.Holder)
				if autoUnfreeze > 0 {
					fmt.Printf("  the agent ends the freeze on its own %s after it is frozen\n", autoUnfreeze)
				}
				fmt.Println()
			} else {
				if autoUnfreeze > 0 {
					return fmt.Errorf("--auto-unfreeze needs the lhtx agent in the cluster (lhtx agent --help)")
				}
				fmt.Printf("  policy: deny pods mounting them, deny PVC create/delete except by %s\n\n", p.User)
			}
			for _, w := range p.Warnings {
				fmt.Println("⚠ " + w)
			}
			if ag == nil {
				fmt.Println("⚠ until lhtx unfreeze, these workloads stay down – also if lhtx stops or this machine goes away")
			} else {
				fmt.Println("⚠ until lhtx unfreeze, these workloads stay down")
			}
			mode, err := g.dryRun()
			if err != nil {
				return err
			}
			if mode != longhorn.DryRunNone {
				fmt.Println("\n(dry run: nothing changed – a freeze has no server dry run)")
				return nil
			}
			if err := g.confirm(true, "freeze"); err != nil {
				return err
			}
			start := time.Now()
			if ag != nil {
				spec := agent.Spec{PVCs: p.PVCs, Reason: reason, Wait: wait.String()}
				if autoUnfreeze > 0 {
					spec.AutoUnfreezeAfter = strings.TrimSuffix(strings.TrimSuffix(autoUnfreeze.String(), "0s"), "0m")
				}
				return requestFreeze(ctx, c, spec, p.User)
			}
			f, err := switchover.ApplyFreeze(ctx, c, p, switchover.FreezeOptions{Wait: wait, Log: logger(start)})
			if f != nil {
				fmt.Printf("\nfrozen: %s – end it with: lhtx --rw unfreeze %s\n", f.Name, f.Name)
			}
			return err
		},
	}
	f := freeze.Flags()
	f.BoolVar(&all, "all", false, "every PVC that has a Longhorn volume (e.g. for a Longhorn upgrade)")
	f.StringVar(&node, "node", "", "the PVCs whose Longhorn volume is attached on this node")
	f.StringVar(&reason, "reason", "", "note stored with the freeze (shown by --status and doctor)")
	f.DurationVar(&wait, "wait", 10*time.Minute, "how long to wait for the volumes to detach")
	f.BoolVar(&status, "status", false, "list the active freezes")
	f.BoolVar(&local, "local", false, "do not hand the freeze to the lhtx agent, even if one runs")
	f.DurationVar(&autoUnfreeze, "auto-unfreeze", 0, "with the agent: end the freeze on its own this long after it is frozen (e.g. 4h)")
	_ = freeze.RegisterFlagCompletionFunc("node", func(c *cobra.Command, _ []string, s string) ([]string, cobra.ShellCompDirective) {
		return g.completeNodeDisk(c, nil, s)
	})

	var uall, ignoreKey bool
	var uwait time.Duration
	unfreeze := &cobra.Command{
		Use:   "unfreeze [NAME | --all]",
		Short: "End a freeze: pods may start again, their owners are nudged (needs --rw)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, err := g.client()
			if err != nil {
				return err
			}
			fs, err := activeFreezes(ctx, c)
			if err != nil {
				return err
			}
			var sel []switchover.Freeze
			for _, fr := range fs {
				if uall || (len(args) == 1 && fr.Name == args[0]) {
					sel = append(sel, fr)
				}
			}
			switch {
			case len(fs) == 0:
				fmt.Println("nothing is frozen")
				return nil
			case len(sel) == 0 && len(args) == 0 && len(fs) == 1:
				sel = fs
			case len(sel) == 0:
				_ = printFreezes(cmd, c)
				return fmt.Errorf("give a freeze NAME or --all")
			}
			for _, fr := range sel {
				fmt.Printf("Unfreeze %s: %d PVCs, %d owners to nudge · %s\n", fr.Name, len(fr.PVCs), len(fr.Owners), c.Cluster.Context)
			}
			if !g.rw {
				return fmt.Errorf("lhtx is read-only by default: add --rw")
			}
			if mode, _ := g.dryRun(); mode != longhorn.DryRunNone {
				fmt.Println("(dry run: nothing changed)")
				return nil
			}
			if err := g.confirm(false, ""); err != nil {
				return err
			}
			start := time.Now()
			var errs []string
			for _, fr := range sel {
				err := agent.Unfreeze(ctx, c, fr, switchover.FreezeOptions{Wait: uwait, IgnoreKey: ignoreKey}, logger(start), plainLog)
				if ctx.Err() != nil {
					fmt.Println("\nstopped following – the agent goes on (lhtx agent status)")
					return nil
				}
				if err != nil {
					errs = append(errs, err.Error())
				}
			}
			if len(errs) > 0 {
				return fmt.Errorf("%s", strings.Join(errs, "; "))
			}
			return nil
		},
	}
	unfreeze.Flags().BoolVar(&uall, "all", false, "end all freezes")
	unfreeze.Flags().BoolVar(&ignoreKey, "ignore-key", false, "unfreeze even if a key Secret differs from the copy a switch kept")
	unfreeze.Flags().DurationVar(&uwait, "wait", 10*time.Minute, "how long to wait for the volumes to be attached again")
	return []*cobra.Command{freeze, unfreeze}
}

// requestBlind hands a freeze to the agent without a local plan: the user
// may create Freezes but not read the pods and volumes behind them.
func requestBlind(cmd *cobra.Command, g *globals, c *longhorn.Client, ag *agent.Info, pvcs []string, reason string,
	wait, autoUnfreeze time.Duration, why error) error {
	ctx := cmd.Context()
	fmt.Printf("Freeze %d PVCs · %s\n\n", len(pvcs), c.Cluster.Context)
	fmt.Printf("  PVCs:   %s\n", strings.Join(short(pvcs, 8), ", "))
	fmt.Printf("  no preview: you may not read what uses them (%v)\n", why)
	fmt.Printf("  the lhtx agent %s plans it, stops every pod that mounts them and logs each step\n", ag.Holder)
	if autoUnfreeze > 0 {
		fmt.Printf("  the agent ends the freeze on its own %s after it is frozen\n", autoUnfreeze)
	}
	fmt.Println("\n⚠ until lhtx unfreeze, these workloads stay down")
	if mode, _ := g.dryRun(); mode != longhorn.DryRunNone {
		fmt.Println("\n(dry run: nothing changed)")
		return nil
	}
	if err := g.confirm(true, "freeze"); err != nil {
		return err
	}
	user, _ := switchover.Username(ctx, c.Dynamic())
	spec := agent.Spec{PVCs: pvcs, Reason: reason, Wait: wait.String()}
	if autoUnfreeze > 0 {
		spec.AutoUnfreezeAfter = strings.TrimSuffix(strings.TrimSuffix(autoUnfreeze.String(), "0s"), "0m")
	}
	return requestFreeze(ctx, c, spec, user)
}

// requestFreeze creates the Freeze and follows the agent until it is frozen
// or failed (a failed Freeze is removed: nothing is frozen).
func requestFreeze(ctx context.Context, c *longhorn.Client, spec agent.Spec, user string) error {
	name, err := agent.Request(ctx, c, spec, user)
	if err != nil {
		return err
	}
	fmt.Printf("requested Freeze %s from the agent\n", name)
	fr, _, err := agent.Follow(ctx, c, name, true, plainLog, agent.Settled)
	switch {
	case ctx.Err() != nil:
		fmt.Printf("\nstopped following – the agent goes on (lhtx agent status)\n")
		return nil
	case err != nil:
		return err
	case fr == nil || fr.Status.Phase == agent.PhaseFailed:
		msg := "the Freeze was removed"
		if fr != nil {
			msg = fr.Status.Message
			_ = agent.End(ctx, c, name)
		}
		return fmt.Errorf("the agent could not freeze: %s", msg)
	}
	fmt.Printf("\nfrozen: %s – end it with: lhtx --rw unfreeze %s\n", agent.PolicyName(name), agent.PolicyName(name))
	return nil
}

// plainLog prints the agent's lines: they carry their own time (UTC).
func plainLog(f string, a ...any) { fmt.Printf("  %s\n", fmt.Sprintf(f, a...)) }

func logger(start time.Time) func(string, ...any) {
	return func(f string, a ...any) {
		fmt.Printf("%6s  %s\n", time.Since(start).Round(time.Second), fmt.Sprintf(f, a...))
	}
}

// selectPVCs resolves the arguments: NS/PVC names, --node or --all.
func selectPVCs(cmd *cobra.Command, c *longhorn.Client, args []string, all bool, node string) ([]string, error) {
	n := 0
	for _, b := range []bool{len(args) > 0, all, node != ""} {
		if b {
			n++
		}
	}
	if n != 1 {
		return nil, fmt.Errorf("give NS/PVC names, --node NODE or --all")
	}
	if len(args) > 0 {
		return args, nil
	}
	vols, err := c.Volumes(cmd.Context())
	if err != nil {
		return nil, err
	}
	var out []string
	for _, v := range vols {
		if v.BoundPVC() == "" {
			continue
		}
		if all || (v.Status.State == "attached" && v.Status.CurrentNodeID == node) {
			out = append(out, v.BoundPVC())
		}
	}
	sort.Strings(out)
	if len(out) == 0 {
		return nil, fmt.Errorf("no PVC matches")
	}
	return out, nil
}

// activeFreezes lists the freezes; who may not read admission policies
// (only create Freezes for the agent) gets the agent's Freezes instead.
func activeFreezes(ctx context.Context, c *longhorn.Client) ([]switchover.Freeze, error) {
	fs, err := switchover.ActiveFreezes(ctx, c.Dynamic())
	if !apierrors.IsForbidden(err) {
		return fs, err
	}
	crs, lerr := agent.List(ctx, c)
	if lerr != nil || crs == nil {
		return nil, err
	}
	fs = nil
	for _, cr := range crs {
		if cr.Status.Phase != agent.PhaseFrozen && cr.Status.Phase != agent.PhaseFreezing {
			continue
		}
		f := switchover.Freeze{Name: agent.PolicyName(cr.Name), Request: cr.Name, PVCs: cr.Spec.PVCs,
			Reason: cr.Spec.Reason, User: cr.RequestedBy, Created: cr.Created}
		if cr.Status.Freeze != nil {
			f.Owners = cr.Status.Freeze.Owners
		}
		fs = append(fs, f)
	}
	return fs, nil
}

func printFreezes(cmd *cobra.Command, c *longhorn.Client) error {
	fs, err := activeFreezes(cmd.Context(), c)
	if err != nil {
		return err
	}
	if len(fs) == 0 {
		fmt.Println("nothing is frozen")
		return nil
	}
	w := newTab(os.Stdout)
	fmt.Fprintln(w, "NAME\tSINCE\tPVCS\tOWNERS\tBY\tREASON")
	for _, f := range fs {
		fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%s\t%s\n", f.Name, f.Created.Local().Format("2006-01-02 15:04"), len(f.PVCs), len(f.Owners), dash(f.User), dash(f.Reason))
	}
	return w.Flush()
}

func short(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return append(append([]string{}, s[:n]...), fmt.Sprintf("… %d more", len(s)-n))
}

func listSuffix(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return ": " + strings.Join(short(s, 5), ", ")
}
