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

**While the app still runs** – a failure here costs no downtime, lhtx
removes what it created:

1. the copy pod goes through the API server as a server dry run: Pod
   Security, policy engines (Kyverno, Gatekeeper), quotas and LimitRanges
   see it before anything is created
2. a new, smaller Longhorn volume with the old one's settings (replicas,
   data locality, selectors, recurring jobs and groups – backups go on)
   and, if encrypted, the same key; a temporary PV/PVC for it
3. a pre-flight pod on the copy node mounts the new volume: the image can
   be pulled, the pod scheduled, Longhorn places **all** replicas,
   attaches the volume and creates the filesystem (and encryption).
   A bad image or an unschedulable pod ends this at once, not after a
   timeout

**App down** (frozen, like `lhtx freeze`):

4. freeze the PVC (its pods stop) and wait until the volume is detached
5. the copy pod (lhtx's own image, `lhtx copy`, root) mounts the old PVC
   read-only – the freeze lets exactly this pod through – and the new
   volume; checks that the used space **and the number of files** fit;
   copies all files keeping owners, modes, setuid, hard and symbolic
   links, devices and FIFOs, xattrs/ACLs/capabilities, timestamps and
   holes (sparse files are not even read where they are empty); then
   compares both trees: every entry's type, mode, owner, size, links,
   times and a SHA-256 of every file's content
6. delete the copy pod and the temporary PV/PVC, then the switch: old PV
   to `Retain`, PVC re-created on the new volume, key guard, unfreeze

The copy runs on the node of the old volume (for `strict-local` volumes
the node of their replica – the only one that can attach them), with the
tolerations of the app's pod. If lhtx loses the API server while copying
it retries for about two minutes – the copy goes on in the cluster.

Until the old PVC is deleted (step 6) every failure – and Ctrl+C – rolls
back: the app starts again on the old volume, the new volume is removed.
The old volume is kept as the way back (`lhtx switch NS/PVC --to-volume
OLD`); it keeps its space until you delete it (lhtx prints how).

**If lhtx is killed** during a shrink (laptop closed), the PVC stays
frozen and the copy pod, the temporary PVC and the new volume stay.
`lhtx doctor` reports them; `lhtx --rw shrink --abort NS/PVC` removes
them and unfreezes – the app starts again on its old volume. If lhtx was
killed after the old PVC was deleted, `--abort` refuses and points to the
journal (`lhtx switch --status`).

Checks before anything changes: raw block PVCs, RWX volumes and namespaces
enforcing the `restricted` Pod Security level are refused; the size must be
smaller and leave 10 % free of what is used (from kubelet's stats when
allowed, otherwise the copy pod checks it). lhtx notes: a PVC managed by
Flux/Helm needs its size changed in Git first (or the next sync expands it
again); a StatefulSet's claim template keeps the old size; the new volume
starts a new backup chain (first backup full, the old backups stay
restorable); snapshots stay with the old volume. Databases are better
shrunk with their own tools (dump/restore, CNPG).

The app is down from step 4 to the end – about as long as reading the used
data twice (copy, compare) takes, plus stopping and starting the app.
`--dry-run` shows the plan; `--image` / `--image-pull-secret` for clusters
without access to ghcr.io (default: the image of this lhtx version).
