# Restore and switch

[← README](../README.md)

`lhtx restore VOLUME --switch`, `lhtx switch`, `r` → *switch* in the TUI: the
web UI makes you restore under another name, detach, delete the PVC and
create PV/PVC by hand. lhtx does it in one confirmed run:

![Restore a backup and switch the PVC to it: step preview, PVC frozen, pods stopped, PVC re-created, pods back](switch.gif)

1. restore into a new volume, wait until it is restored and detached
2. freeze the PVC (see above): its pods stop, nothing can start on it or
   re-create it meanwhile; wait until the old volume is detached
3. set the old PV to `Retain`, delete the old PVC
4. create a PV for the restored volume and the PVC again with the same name,
   labels and annotations (Flux and Helm keep owning it), wait until Bound
5. unfreeze: the owners start their pods again on the restored data

The old volume is kept: `lhtx switch NS/PVC --to-volume OLD` switches back
(the old, released PV is rebound). Until the old PVC is deleted every failure
– and Ctrl+C – is rolled back; after that the PVC stays frozen until it is
fixed, so nothing re-creates it with an empty volume. Every step goes to a
journal in `$XDG_STATE_HOME/lhtx/switch/` (`lhtx switch --status`; `doctor`
warns about unfinished ones). A CloudNativePG instance can be switched, but
lhtx warns: restoring one instance's PVC does not restore the database
consistently – prefer CNPG's own recovery. `--dry-run` shows the plan.

`lhtx switch NS/PVC --from-backup BACKUP` is the same as `restore --switch`.
With the [lhtx agent](agent.md) in the cluster the switch runs there – it
continues after a restart, and namespace users may switch their own PVCs.

A restore or clone into a **new** volume (+ PV/PVC) is described in
[write-actions.md](write-actions.md); encrypted volumes in
[encryption.md](encryption.md).

## Shrink a volume

Neither Longhorn nor Kubernetes can make a volume smaller. `lhtx shrink
NS/PVC SIZE` does what you would do by hand – new volume, copy, swap – as
one checked run with the switch underneath:

```
lhtx --rw shrink shop/data 5Gi
```

1. create a new, smaller Longhorn volume with the old one's settings
   (replicas, data locality, selectors, recurring jobs and groups – backups
   go on) and, if encrypted, the same key; a temporary PV/PVC for it
2. freeze the PVC (its pods stop) and wait until the volume is detached
3. a copy pod with lhtx's own image (`lhtx copy`, root, no other image)
   mounts the old PVC read-only (the freeze lets exactly this pod through)
   and the new volume, checks that the used data fits, copies all files –
   owners, modes, setuid, hard and symbolic links, devices and FIFOs,
   xattrs/ACLs/capabilities, timestamps, holes in sparse files – and
   compares both trees
4. delete the copy pod and the temporary PV/PVC, then the switch: old PV to
   `Retain`, PVC re-created on the new volume, key guard, unfreeze

The app is down from step 2 to the end – about as long as copying the used
data takes. Until the old PVC is deleted every failure (also Ctrl+C) rolls
back: the app starts again on the old volume and the new volume is removed.
The old volume is kept as the way back (`lhtx switch NS/PVC --to-volume
OLD`).

Checks before anything changes: raw block PVCs, RWX volumes and namespaces
enforcing the `restricted` Pod Security level are refused; the size must be
smaller and leave 10 % free of what is used (from kubelet's stats when
allowed, otherwise the copy pod checks it). It warns when the PVC is managed
by Flux/Helm (change the size in Git first, or the next sync expands it
again) and for StatefulSets (the claim template keeps the old size).
Databases are better shrunk with their own tools (dump/restore, CNPG).
`--dry-run` shows the plan; `--image` overrides the copy pod's image
(default: the image of this lhtx version).
