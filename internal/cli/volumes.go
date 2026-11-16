package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/nxckdx/longhorntx/internal/format"
	"github.com/nxckdx/longhorntx/internal/kube"
	"github.com/nxckdx/longhorntx/internal/longhorn"
)

func volumesCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:     "volumes",
		Aliases: []string{"volume", "vol", "v"},
		Short:   "List Longhorn volumes",
		Long: `List Longhorn volumes.

-o table only reads volumes (cheapest). -o wide and -o json additionally
read replicas and nodes to show where the replicas live.`,
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
			ctx := cmd.Context()

			var snap longhorn.State
			if snap.Volumes, err = c.Volumes(ctx); err != nil {
				return err
			}
			withReplicas := g.output != "table"
			if withReplicas {
				if snap.Replicas, err = c.Replicas(ctx, ""); err != nil {
					return err
				}
				if snap.Nodes, err = c.Nodes(ctx); err != nil {
					return err
				}
			}
			views := longhorn.Build(snap)

			if g.output == "json" {
				return writeJSON(os.Stdout, views)
			}
			w := newTab(os.Stdout)
			if withReplicas {
				fmt.Fprintln(w, "PVC\tVOLUME\tSTATE\tROBUSTNESS\tSIZE\tACTUAL\tNODE\tREPLICAS\tLOCATIONS")
			} else {
				fmt.Fprintln(w, "PVC\tVOLUME\tSTATE\tROBUSTNESS\tSIZE\tACTUAL\tNODE\tREPLICAS")
			}
			for _, v := range views {
				row := []string{
					dash(v.PVC), v.Name, v.State, v.Health,
					format.Bytes(v.Size), format.Bytes(v.Actual), dash(v.Node),
				}
				if withReplicas {
					var locs []string
					for _, r := range v.Replicas {
						locs = append(locs, r.Node+":"+dash(r.Disk.Path))
					}
					row = append(row, fmt.Sprintf("%d/%d", v.HealthyReplicas(), v.Desired), strings.Join(locs, ","))
				} else {
					row = append(row, fmt.Sprint(v.Desired))
				}
				fmt.Fprintln(w, strings.Join(row, "\t"))
			}
			return w.Flush()
		},
	}
}

func replicasCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:     "replicas [VOLUME]",
		Aliases: []string{"replica", "rep", "r"},
		Short:   "List replicas with node and disk",
		Long: `List the replicas of a volume (or of all volumes) with node and disk.

VOLUME may be the Longhorn volume / PV name, namespace/pvc, or a bare PVC
name if it is unambiguous. With a VOLUME, the engine is read as well to show
replica mode and rebuild progress.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := g.checkOutput(); err != nil {
				return err
			}
			c, err := g.client()
			if err != nil {
				return err
			}
			defer g.reportTraffic(c)
			ctx := cmd.Context()

			var snap longhorn.State
			single := len(args) == 1
			if single {
				v, err := resolveVolume(ctx, c, args[0])
				if err != nil {
					return err
				}
				snap.Volumes = []*longhorn.Volume{v}
				if snap.Replicas, err = c.Replicas(ctx, v.Name); err != nil {
					return err
				}
				if snap.Engines, err = c.Engines(ctx, v.Name); err != nil {
					return err
				}
			} else {
				if snap.Volumes, err = c.Volumes(ctx); err != nil {
					return err
				}
				if snap.Replicas, err = c.Replicas(ctx, ""); err != nil {
					return err
				}
			}
			if snap.Nodes, err = c.Nodes(ctx); err != nil {
				return err
			}
			views := longhorn.Build(snap)

			if g.output == "json" {
				if single {
					return writeJSON(os.Stdout, views[0])
				}
				return writeJSON(os.Stdout, views)
			}

			wide := g.output == "wide"
			w := newTab(os.Stdout)
			hdr := []string{"REPLICA", "NODE", "PATH", "STATE", "HEALTH"}
			if !single {
				hdr = append([]string{"PVC"}, hdr...)
			}
			if single {
				hdr = append(hdr, "MODE", "REBUILD")
			}
			if wide {
				hdr = append(hdr, "DISK", "DISK-UUID", "DATA-DIR", "HEALTHY-AT")
			}
			fmt.Fprintln(w, strings.Join(hdr, "\t"))
			now := time.Now()
			for _, v := range views {
				for _, r := range v.Replicas {
					row := []string{r.Name, r.Node, dash(r.Disk.Path), dash(r.State), r.Health}
					if !single {
						row = append([]string{dash(v.PVC)}, row...)
					}
					if single {
						reb := "-"
						if r.Rebuild != nil {
							reb = fmt.Sprintf("%d%%", r.Rebuild.Progress)
						}
						row = append(row, dash(r.Mode), reb)
					}
					if wide {
						age := format.Age(r.Replica.Spec.HealthyAt, now)
						row = append(row, dash(r.Disk.Name), r.Disk.UUID, r.Replica.Spec.DataDirectoryName, dash(age))
					}
					fmt.Fprintln(w, strings.Join(row, "\t"))
				}
			}
			return w.Flush()
		},
	}
}

func contextsCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:     "contexts",
		Aliases: []string{"ctx"},
		Short:   "List kubeconfig contexts (no network access)",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			names, cur, err := kube.Options{Kubeconfig: g.kube.Kubeconfig}.Contexts()
			if err != nil {
				return err
			}
			if g.kube.Context != "" {
				cur = g.kube.Context
			}
			for _, n := range names {
				mark := "  "
				if n == cur {
					mark = "* "
				}
				fmt.Println(mark + n)
			}
			return nil
		},
	}
}

// resolveVolume accepts a volume/PV name, ns/pvc or a bare PVC name. A direct
// GET is tried first because it is the cheapest request.
func resolveVolume(ctx context.Context, c *longhorn.Client, ref string) (*longhorn.Volume, error) {
	if !strings.Contains(ref, "/") {
		v, err := c.Volume(ctx, ref)
		if err == nil || !apierrors.IsNotFound(err) {
			return v, err
		}
	}
	vols, err := c.Volumes(ctx)
	if err != nil {
		return nil, err
	}
	return longhorn.ResolveVolume(vols, ref)
}
