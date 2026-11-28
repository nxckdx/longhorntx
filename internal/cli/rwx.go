package cli

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/nxckdx/longhorntx/internal/format"
	"github.com/nxckdx/longhorntx/internal/longhorn"
)

func rwxCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "rwx",
		Short: "RWX volumes: share manager (NFS server), its pod, endpoints, NFS options, clients",
		Long: `Longhorn serves each ReadWriteMany volume over NFS from a share-manager pod.
When that pod hangs or restarts, client pods hang or get I/O errors while
the volume itself looks healthy. This shows the whole chain per RWX volume:
the ShareManager's state, the share-manager pod (node, ready, restarts and
why), ready endpoints of its Service, the NFS options the clients mount with
(StorageClass nfsOptions, or Longhorn's default), and the client pods.
Problems are also in lhtx doctor. The fix for a hung share manager:
lhtx --rw restart share-manager VOLUME.

Reads (only if there are RWX volumes): ShareManagers, the share-manager
pods, their EndpointSlices, and one PV per RWX volume.`,
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
			rs, err := c.RWX(cmd.Context(), nil, true)
			if err != nil {
				return err
			}
			if g.output == "json" {
				return writeJSON(os.Stdout, rs)
			}
			if len(rs) == 0 {
				fmt.Println("No RWX volumes.")
				return nil
			}
			w := newTab(os.Stdout)
			fmt.Fprintln(w, "PVC / VOLUME\tSTATE\tSHARE\tPOD NODE\tPOD\tRESTARTS\tENDPOINTS\tCLIENTS\tNFS OPTIONS")
			for _, r := range rs {
				pod := "-"
				if r.Pod != "" {
					pod = "ready"
					if !r.PodReady {
						pod = "NOT READY"
					}
					pod += " " + format.Duration(r.PodAge)
				}
				restarts := fmt.Sprint(r.Restarts)
				if r.LastRestart != "" {
					restarts += " (" + r.LastRestart + ")"
				}
				if !r.NFSDownSince.IsZero() {
					pod += ", NFS NOT ANSWERING"
				}
				ep := "-"
				if r.EPKnown {
					ep = fmt.Sprint(r.Endpoints)
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\n", label(r), r.State, dash(r.Share), dash(r.PodNode), pod,
					restarts, ep, len(r.Clients), dash(r.NFSOptions))
			}
			if err := w.Flush(); err != nil {
				return err
			}
			if s, err := c.Setting(cmd.Context(), "rwx-volume-fast-failover"); err == nil && s != "" {
				fmt.Printf("\nrwx-volume-fast-failover: %s", s)
				if s == "false" {
					fmt.Print(" (on: Longhorn moves a share manager faster when its node fails)")
				}
				fmt.Println()
			}
			for _, f := range longhorn.RWXFindings(rs, time.Now()) {
				fmt.Printf("\n%s %s: %s\n  → %s\n", f.Severity, f.Object, f.Message, f.Hint)
			}
			if g.output == "wide" {
				for _, r := range rs {
					if len(r.Clients) > 0 {
						fmt.Printf("\n%s clients: %s\n", label(r), strings.Join(r.Clients, ", "))
					}
				}
			}
			return nil
		},
	}
}

func label(r longhorn.RWXVolume) string {
	if r.PVC != "" {
		return r.PVC
	}
	return r.Volume
}
