package cli

import (
	"fmt"
	"os"
	"slices"
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
	var leaseNS, healthAddr string
	var noDigests, noHistory bool
	var rw, manifestsRW bool
	var histVol, histCluster int
	var histAge, capacityEvery time.Duration
	var notify agent.Notify
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
			a.NoDigests, a.NoHistory, a.RW = noDigests, noHistory, rw
			a.HistoryVolumeMax, a.HistoryClusterMax, a.HistoryMaxAge = histVol, histCluster, histAge
			a.CapacityInterval = capacityEvery
			if notify.URL == "" {
				notify.URL = os.Getenv("LHTX_NOTIFY_URL")
			}
			if notify.URL != "" && !slices.Contains(agent.NotifyFormats, notify.Format) {
				return fmt.Errorf("--notify-format %q: one of %s", notify.Format, strings.Join(agent.NotifyFormats, ", "))
			}
			a.Notify = notify
			a.SetVersion(Version)
			if healthAddr != "" {
				go func() {
					if err := a.ServeHealth(cmd.Context(), healthAddr); err != nil {
						fmt.Fprintf(os.Stderr, "health endpoint: %v\n", err)
					}
				}()
			}
			fmt.Fprintf(os.Stderr, "lhtx agent %s · %s · Longhorn namespace %s\n", Version, cl.Context, g.namespace)
			if noLeader {
				return a.Run(cmd.Context())
			}
			// the lease lives in the Longhorn namespace, where lhtx looks for it
			// (the agent itself may run elsewhere)
			if leaseNS == "" {
				leaseNS = g.namespace
			}
			return a.RunWithLeaderElection(cmd.Context(), leaseNS)
		},
	}
	cmd.Flags().BoolVar(&rw, "rw", false, "carry out Freezes and Switches (needs the write RBAC); without it the agent only reads and refuses them")
	cmd.Flags().DurationVar(&capacityEvery, "capacity-interval", 6*time.Hour, "sample the disks for the capacity forecast (lhtx capacity) this often; 0 = off")
	cmd.Flags().StringVar(&notify.URL, "notify-url", "", "send failures and recoveries to this webhook (better: env LHTX_NOTIFY_URL – it may hold a token)")
	cmd.Flags().StringVar(&notify.Format, "notify-format", "json", "webhook format: "+strings.Join(agent.NotifyFormats, ", ")+" (slack also fits Mattermost and Rocket.Chat)")
	cmd.Flags().StringVar(&notify.Cluster, "notify-cluster", "", "cluster name shown in notifications")
	cmd.Flags().DurationVar(&notify.Every, "notify-every", time.Minute, "at most one notification per this interval (events in between are sent together)")
	cmd.Flags().BoolVar(&noHistory, "no-history", false, "do not record VolumeHistories (lhtx history)")
	cmd.Flags().IntVar(&histVol, "history-per-volume", longhorn.MaxVolumeHistory, "events kept per volume (at most 1000)")
	cmd.Flags().IntVar(&histCluster, "history-cluster", longhorn.MaxClusterHistory, "notable events kept in the cluster-wide history (at most 5000)")
	cmd.Flags().DurationVar(&histAge, "history-max-age", 0, "drop events older than this when a history is written, e.g. 2160h (0 = only the count limits)")
	cmd.Flags().BoolVar(&noDigests, "no-digests", false, "do not keep VolumeDigests (lhtx --refresh agent then has nothing to watch)")
	cmd.Flags().StringVar(&healthAddr, "health-addr", ":8081", "address for the probes /healthz, /readyz and the Prometheus /metrics (read-only; empty = off)")
	cmd.Flags().StringVar(&leaseNS, "lease-namespace", "", "namespace of the lease (default: the Longhorn namespace, where lhtx looks first; elsewhere lhtx finds it by label if users may list leases)")
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
			fmt.Print(deploy.Manifests(tag, g.namespace, manifestsRW))
			return nil
		},
	}
	manifests.Flags().StringVar(&tag, "tag", "", "image tag (default: this lhtx's version)")
	manifests.Flags().BoolVar(&manifestsRW, "rw", false, "an agent that carries out freezes and switches (adds the write RBAC and --rw)")

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
				mode := "read-write: carries out freezes and switches"
				if !info.RW {
					mode = "read-only: digests, history, metrics, diag logs – lhtx does freezes and switches itself"
				}
				fmt.Printf("agent: %s (lease in %s, heartbeat %s ago)\nmode:  %s\n", info.Holder, info.Namespace, time.Since(info.Renewed).Round(time.Second), mode)
			} else {
				fmt.Println("agent: none found (no fresh lease " + c.Namespace + "/" + agent.LeaseName + ", none labelled " + agent.LeaseLabel + " that you may list) – lhtx freezes on its own")
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
