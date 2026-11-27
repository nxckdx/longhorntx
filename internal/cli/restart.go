package cli

import (
	"github.com/spf13/cobra"

	"github.com/nxckdx/longhorntx/internal/longhorn"
)

func restartCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{Use: "restart", Short: "Restart a share manager or a node's CSI plugin (needs --rw)"}
	cmd.AddCommand(&cobra.Command{
		Use:   "share-manager VOLUME",
		Short: "Delete the share-manager pod of an RWX volume; Longhorn starts a new one (needs --rw)",
		Long: `The fix for hung RWX volumes (lhtx rwx, lhtx doctor): the NFS server of the
volume restarts, clients reconnect. While it is gone client I/O fails or
blocks.`,
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
			p, err := c.ShareManagerPod(ctx, v.Name)
			if err != nil {
				return err
			}
			a, err := longhorn.RestartShareManager(v, p)
			if err != nil {
				return err
			}
			return g.runAction(ctx, c, a)
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "csi-plugin NODE",
		Short: "Delete the longhorn-csi-plugin pod on a node; the DaemonSet starts a new one (needs --rw)",
		Long: `The first fix for pods stuck mounting Longhorn volumes on a node (lhtx mounts),
before restarting kubelet. Volumes already mounted keep working.`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: g.completeNodeDisk,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, err := g.client()
			if err != nil {
				return err
			}
			p, err := c.CSIPluginPod(ctx, args[0])
			if err != nil {
				return err
			}
			return g.runAction(ctx, c, longhorn.RestartCSIPlugin(args[0], p))
		},
	})
	return cmd
}
