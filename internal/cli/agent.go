package cli

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/nxckdx/longhorntx/deploy"
	"github.com/nxckdx/longhorntx/internal/agent"
	"github.com/nxckdx/longhorntx/internal/kube"
	"github.com/nxckdx/longhorntx/internal/longhorn"
)

func agentCmd(g *globals) *cobra.Command {
	var noLeader bool
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Optional in-cluster agent: carries out freezes so they do not depend on your laptop",
		Long: `The lhtx agent runs as a Deployment in the Longhorn namespace and carries
out freezes requested as Freeze resources (lhtx.io/v1alpha1). With an agent
in the cluster, lhtx freeze / unfreeze (and Z in the TUI) hand the work to it
and only follow its progress: closing the laptop or losing the LTE link no
longer stops a freeze halfway, the agent resumes after a restart, and
--auto-unfreeze ends a forgotten freeze on its own.

Without an agent lhtx works as before. Install it:

  lhtx agent manifests | kubectl apply -f -
  lhtx agent status

Requests go through the Kubernetes API only (the agent has no HTTP endpoint):
whoever may create Freeze resources may freeze – bind the ClusterRole
lhtx-freeze to them. The agent itself needs rights like cluster-admin
(deleting pods, exec for hooks, admission policies).

"lhtx agent" without a subcommand runs the agent (in its pod; with
--context it runs against that cluster, for development).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var cl *kube.Cluster
			var err error
			if g.kube.Context == "" && g.kube.Kubeconfig == "" && os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
				cl, err = kube.InCluster()
			} else {
				cl, err = kube.Connect(g.kube)
			}
			if err != nil {
				return err
			}
			c, err := longhorn.NewClient(cl, g.namespace)
			if err != nil {
				return err
			}
			a := agent.New(c, os.Getenv("POD_NAME"))
			fmt.Fprintf(os.Stderr, "lhtx agent %s · %s · Longhorn namespace %s\n", Version, cl.Context, g.namespace)
			if noLeader {
				return a.Run(cmd.Context())
			}
			// the lease lives in the Longhorn namespace, where lhtx looks for it
			// (the agent itself may run elsewhere)
			return a.RunWithLeaderElection(cmd.Context(), g.namespace)
		},
	}
	cmd.Flags().BoolVar(&noLeader, "no-leader-election", false, "run without the lease (only one agent may run; lhtx will not detect it)")

	var tag string
	manifests := &cobra.Command{
		Use:   "manifests",
		Short: "Print the agent's manifests (CRD, RBAC, Deployment) for kubectl apply -f -",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			if tag == "" {
				tag = "latest"
				if Version != "dev" {
					tag = strings.TrimPrefix(Version, "v")
				}
			}
			fmt.Print(deploy.Manifests(tag, g.namespace))
			return nil
		},
	}
	manifests.Flags().StringVar(&tag, "tag", "", "image tag (default: this lhtx's version)")

	status := &cobra.Command{
		Use:   "status",
		Short: "Is an agent running? Lists the Freeze resources",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := g.client()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			if info := agent.Detect(ctx, c); info != nil {
				fmt.Printf("agent: %s (heartbeat %s ago)\n", info.Holder, time.Since(info.Renewed).Round(time.Second))
			} else {
				fmt.Println("agent: none running (no fresh lease " + c.Namespace + "/" + agent.LeaseName + ") – lhtx freezes on its own")
			}
			fs, err := agent.List(ctx, c)
			if err != nil {
				return err
			}
			if len(fs) == 0 {
				return nil
			}
			fmt.Println()
			w := newTab(os.Stdout)
			fmt.Fprintln(w, "FREEZE\tPHASE\tPVCS\tSINCE\tAUTO-UNFREEZE\tBY\tREASON\tMESSAGE")
			for _, f := range fs {
				phase := f.Status.Phase
				if f.Deleting {
					phase += " (ending)"
				}
				since := "-"
				if f.Status.FrozenAt != nil {
					since = f.Status.FrozenAt.Local().Format("2006-01-02 15:04")
				}
				fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\t%s\t%s\t%s\n", f.Name, dash(phase), len(f.Spec.PVCs), since,
					dash(f.Spec.AutoUnfreezeAfter), dash(f.RequestedBy), dash(f.Spec.Reason), dash(f.Status.Message))
			}
			return w.Flush()
		},
	}
	cmd.AddCommand(manifests, status)
	return cmd
}
