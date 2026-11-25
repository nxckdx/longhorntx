package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/nxckdx/longhorntx/internal/format"
	"github.com/nxckdx/longhorntx/internal/kube"
	"github.com/nxckdx/longhorntx/internal/longhorn"
	"github.com/nxckdx/longhorntx/internal/switchover"
)

func restoreCmd(g *globals) *cobra.Command {
	var name, pvc, sc, fs string
	var keys keyFlags
	var replicas int
	var doSwitch bool
	var wait time.Duration
	cmd := &cobra.Command{
		Use:   "restore BACKUP|VOLUME [--name NEW] [--pvc NS/NAME|auto]",
		Short: "Restore a backup into a new volume, optionally with PV and PVC (needs --rw)",
		Long: `Restores a backup into a NEW Longhorn volume – like the web UI's "Restore"
followed by "Create PV/PVC", in one confirmed step. The original volume and
its workload are not touched.

BACKUP is a backup name (backup-…); a VOLUME (also a deleted one, by volume
or PVC name) restores its latest completed backup.

  lhtx --rw restore databases/data-postgres-0 --pvc auto
  lhtx --rw restore backup-0ad1025890d413e1 --name wiki-restored --pvc legacy/wiki-data-restored

Defaults: name "<pvc>-restore-<MMDD>", replicas from the default-replica-count
setting, PV/PVC with Longhorn's static StorageClass, ext4, reclaim Retain.
--pvc auto creates a PVC with the volume's name in the original namespace.

--switch restores and then points the original PVC to the restored volume
(its pods frozen meanwhile, nothing scaled) – see lhtx switch.
The old volume is kept for a rollback.

  lhtx --rw restore immich/library --switch`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: g.completeVolumes,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, err := g.client()
			if err != nil {
				return err
			}
			var b *longhorn.Backup
			if strings.HasPrefix(args[0], "backup-") {
				if b, err = c.Backup(ctx, args[0]); err != nil {
					return err
				}
			} else {
				vol := ""
				if v, err := resolveVolume(ctx, c, args[0]); err == nil {
					vol = v.Name
				} else if vol = deletedBackupVolume(cmd, c, args[0]); vol == "" {
					return err
				}
				bks, err := c.Backups(ctx, vol)
				if err != nil {
					return err
				}
				if b = longhorn.LatestBackup(bks); b == nil {
					return fmt.Errorf("volume %s has no completed backup", vol)
				}
			}
			p, err := c.PlanRestore(ctx, b, time.Now())
			if err != nil {
				return err
			}
			f := cmd.Flags()
			if f.Changed("name") {
				p.Spec.Name = name
			}
			if f.Changed("replicas") {
				p.Spec.Replicas = replicas
			}
			if f.Changed("storage-class") {
				p.Spec.StorageClass = sc
			}
			if f.Changed("fs") {
				p.Spec.FSType = fs
			}
			if err := keys.apply(cmd, g, p.Backup.Volume(), p.Backup.Encrypted(), &p.Spec); err != nil {
				return err
			}
			if doSwitch {
				if pvc != "" {
					return fmt.Errorf("--switch re-creates the original PVC: leave out --pvc")
				}
				return g.restoreAndSwitch(cmd, c, p, wait)
			}
			switch {
			case pvc == "auto":
				if p.Spec.PVCNamespace == "" {
					return fmt.Errorf("the original namespace is unknown: give --pvc NAMESPACE/NAME")
				}
				p.Spec.PVCName = p.Spec.Name
			case pvc != "":
				ns, n, ok := strings.Cut(pvc, "/")
				if !ok || ns == "" || n == "" {
					return fmt.Errorf("--pvc NAMESPACE/NAME or auto")
				}
				p.Spec.PVCNamespace, p.Spec.PVCName = ns, n
			}
			acts, err := c.BuildRestore(ctx, p)
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
	f.StringVar(&name, "name", "", "name of the new volume (and PV)")
	f.StringVar(&pvc, "pvc", "", "also create PV + PVC: NAMESPACE/NAME, or auto (original namespace, volume name)")
	f.IntVar(&replicas, "replicas", 0, "number of replicas (default: the default-replica-count setting)")
	f.StringVar(&sc, "storage-class", "", "StorageClass of PV/PVC (default: Longhorn's static class)")
	f.StringVar(&fs, "fs", "", "filesystem of the PV (default ext4)")
	keys.register(f)
	f.BoolVar(&doSwitch, "switch", false, "then switch the original PVC to the restored volume (its pods are frozen meanwhile)")
	f.DurationVar(&wait, "wait", 10*time.Minute, "with --switch: how long to wait for pods to stop / the PVC to bind")
	return cmd
}

// restoreAndSwitch plans the switch before anything is created, so blockers
// show up first; then restore + switch run after one confirmation.
func (g *globals) restoreAndSwitch(cmd *cobra.Command, c *longhorn.Client, p *longhorn.RestorePlan, wait time.Duration) error {
	ctx := cmd.Context()
	src := ""
	if v, err := c.Volume(ctx, p.Backup.Volume()); err == nil {
		src = v.PVC()
		if !cmd.Flags().Changed("replicas") {
			p.Spec.Replicas = v.Spec.NumberOfReplicas // like the volume it replaces
		}
	}
	if src == "" {
		return fmt.Errorf("the volume of this backup has no PVC any more – nothing to switch; use --pvc to create a new one")
	}
	ns, pvc, _ := strings.Cut(src, "/")
	acts, err := c.BuildRestore(ctx, p)
	if err != nil {
		return err
	}
	sw, err := switchover.BuildPending(ctx, c, ns, pvc, p.Spec.Name, int64(p.Backup.Status.VolumeSize), p.Backup.Encrypted())
	if err != nil {
		return err
	}
	// the restore-only hints (new PVC, original untouched) do not apply here
	acts[0].Warnings = []string{fmt.Sprintf("restores %s of backup %s; the PVC then uses the restored data – changes since that backup are not in it",
		humanSize(int64(p.Backup.Status.Size)), p.Backup.Name)}
	return g.runSwitch(ctx, c, sw, acts[0], wait)
}

// keyFlags choose how an encrypted source's key gets to the new PV.
type keyFlags struct {
	secret, source, fromContext string
	copy                        bool
}

func (k *keyFlags) register(f *pflag.FlagSet) {
	f.StringVar(&k.secret, "secret", "", "encrypted source: use this Secret NAMESPACE/NAME with the key (e.g. from your secret manager); lhtx checks it holds the source's key")
	f.BoolVar(&k.copy, "copy-secret", false, "encrypted source: copy the source's key Secret next to the new PVC (also from --key-from-context)")
	f.StringVar(&k.source, "key-source", "", "encrypted source: the Secret NAMESPACE/NAME with the source's key, if lhtx cannot find it (PV gone)")
	f.StringVar(&k.fromContext, "key-from-context", "", "encrypted source: the kubeconfig context of the cluster the source volume and its key are in (migration)")
}

// apply resolves the source encryption (also in another cluster) and the
// chosen mode into the spec.
func (k *keyFlags) apply(cmd *cobra.Command, g *globals, srcVolume string, encrypted bool, s *longhorn.RestoreSpec) error {
	if k.secret == "" && !k.copy && k.source == "" && k.fromContext == "" {
		return nil
	}
	if !encrypted {
		return fmt.Errorf("--secret/--copy-secret/--key-source/--key-from-context: the source is not encrypted")
	}
	if k.secret != "" && k.copy {
		return fmt.Errorf("--secret or --copy-secret, not both")
	}
	if s.Encryption == nil {
		s.Encryption = &longhorn.Encryption{}
	}
	e := s.Encryption
	if k.fromContext != "" {
		o := g.kube
		o.Context = k.fromContext
		cl, err := kube.Connect(o)
		if err != nil {
			return err
		}
		other, err := longhorn.NewClient(cl, g.namespaceFor(k.fromContext))
		if err != nil {
			return err
		}
		*e = longhorn.Encryption{From: other}
		if v, err := other.Volume(cmd.Context(), srcVolume); err == nil {
			if oe, err := other.VolumeEncryption(cmd.Context(), v); err == nil && oe != nil {
				e.Secret, e.PerPVC = oe.Secret, oe.PerPVC
			}
		}
	}
	ref := func(flag, v string) (longhorn.SecretRef, error) {
		ns, n, ok := strings.Cut(v, "/")
		if !ok || ns == "" || n == "" {
			return longhorn.SecretRef{}, fmt.Errorf("%s NAMESPACE/NAME", flag)
		}
		return longhorn.SecretRef{Namespace: ns, Name: n}, nil
	}
	if k.source != "" {
		r, err := ref("--key-source", k.source)
		if err != nil {
			return err
		}
		e.Secret, e.PerPVC = r, false
	}
	switch {
	case k.secret != "":
		r, err := ref("--secret", k.secret)
		if err != nil {
			return err
		}
		e.Mode, e.Target = longhorn.KeyExisting, r
	case k.copy:
		e.Mode = longhorn.KeyCopy
	}
	return nil
}

func humanSize(n int64) string { return format.Bytes(n) }
