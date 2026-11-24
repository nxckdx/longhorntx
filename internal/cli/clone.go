package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

func cloneCmd(g *globals) *cobra.Command {
	var name, pvc, snapshot, sc string
	var replicas int
	cmd := &cobra.Command{
		Use:   "clone VOLUME [--snapshot S] [--name NEW] [--pvc NS/NAME|auto]",
		Short: "Clone a volume or one of its snapshots into a new volume (needs --rw)",
		Long: `Copies the current data of a volume, or a snapshot of it, into a NEW
volume (Longhorn spec.dataSource vol:// or snap://), optionally with PV and
PVC. The safe alternative to reverting a snapshot: the running volume is
not touched, and you can mount the clone next to it to look at or copy the
old data.

  lhtx --rw clone databases/data-postgres-0 --snapshot snap-1aba1487 --pvc auto

Defaults: name "<pvc>-clone-<MMDD>", same replica count as the source,
PV/PVC with Longhorn's static StorageClass. --pvc auto uses the source's
namespace and the new volume's name.`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: g.completeVolumes,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, err := g.client()
			if err != nil {
				return err
			}
			v, err := resolveVolume(ctx, c, args[0])
			if err != nil {
				return err
			}
			if snapshot != "" {
				snaps, err := c.Snapshots(ctx, v.Name)
				if err != nil {
					return err
				}
				found := false
				for _, s := range snaps {
					found = found || s.Name == snapshot
				}
				if !found {
					return fmt.Errorf("volume %s has no snapshot %q (lhtx snapshots %s)", v.Name, snapshot, args[0])
				}
			}
			s := c.CloneDefaults(ctx, v, time.Now())
			f := cmd.Flags()
			if f.Changed("name") {
				s.Name = name
			}
			if f.Changed("replicas") {
				s.Replicas = replicas
			}
			if f.Changed("storage-class") {
				s.StorageClass = sc
			}
			switch {
			case pvc == "auto":
				if s.PVCNamespace == "" {
					return fmt.Errorf("the source has no PVC: give --pvc NAMESPACE/NAME")
				}
				s.PVCName = s.Name
			case pvc != "":
				ns, n, ok := strings.Cut(pvc, "/")
				if !ok || ns == "" || n == "" {
					return fmt.Errorf("--pvc NAMESPACE/NAME or auto")
				}
				s.PVCNamespace, s.PVCName = ns, n
			}
			acts, err := c.BuildClone(ctx, v, snapshot, s)
			if err != nil {
				return err
			}
			if len(acts) == 1 {
				return g.runAction(ctx, c, acts[0])
			}
			return g.runBatch(ctx, c, acts[0].Title, acts)
		},
	}
	f := cmd.Flags()
	f.StringVar(&snapshot, "snapshot", "", "clone this snapshot instead of the current data")
	f.StringVar(&name, "name", "", "name of the new volume (and PV)")
	f.StringVar(&pvc, "pvc", "", "also create PV + PVC: NAMESPACE/NAME, or auto")
	f.IntVar(&replicas, "replicas", 0, "number of replicas (default: like the source)")
	f.StringVar(&sc, "storage-class", "", "StorageClass of PV/PVC (default: Longhorn's static class)")
	return cmd
}
