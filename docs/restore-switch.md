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
warns about unfinished ones). lhtx restores volumes, not databases: one
instance of a replicated database restored from a backup is rarely
consistent with the others – use the database's own backup and restore.
`--dry-run` shows the plan.

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

4. **pre-copy**: a pod next to the app (same node; a ReadWriteOnce PVC may
   be mounted by several pods there) mounts the PVC read-only and copies
   everything while the app keeps running, remembers each entry's
   fingerprint (inode, ctime, mtime, size) and checks the copied content.
   What changes meanwhile is simply left for the final pass. It reads the
   whole volume while the app works – `--no-precopy` skips it
   (ReadWriteOncePod PVCs skip it on their own: nothing may mount them
   twice)

**App down** (frozen, like `lhtx freeze`):

5. freeze the PVC (its pods stop) and wait until the volume is detached
6. the copy pod (lhtx's own image, `lhtx copy`, root) mounts the old PVC
   read-only – the freeze lets exactly this pod through – and the new
   volume. After a pre-copy it copies only what is new or changed (the
   ctime moves with every change and cannot be set back) and removes what
   is gone; without one it checks that the used space **and the number of
   files** fit and copies everything. Files keep owners, modes, setuid,
   hard and symbolic links, devices and FIFOs, xattrs/ACLs/capabilities,
   timestamps and holes (sparse files are not even read where they are
   empty). Then both trees are compared: every entry's type, mode, owner,
   size, links and times, and a SHA-256 of the content of what was copied
   (the pre-copied rest was checked before the freeze). 16 files are
   copied and compared at the same time – with many small files every
   file is a few round trips to the disk (in the lab, 80 000 small files:
   49 s one by one, 12 s in parallel)
7. delete the copy pod and the temporary PV/PVC, then the switch: old PV
   to `Retain`, PVC re-created on the new volume, key guard, unfreeze

The copy runs on the node of the old volume (for `strict-local` volumes
the node of their replica – the only one that can attach them), with the
tolerations of the app's pod. If lhtx loses the API server while copying
it retries for about two minutes – the copy goes on in the cluster.

Until the old PVC is deleted (step 7) every failure – and Ctrl+C – rolls
back: the app starts again on the old volume, the new volume is removed.
The old volume is kept as the way back (`lhtx switch NS/PVC --to-volume
OLD`); it keeps its space until you delete it (lhtx prints how).

**With the [lhtx agent](agent.md)** (`rw: true`) the shrink runs in the
cluster: lhtx and the TUI only follow it, closing the laptop changes
nothing (`--local` runs it on your machine). The copy pod uses the agent's
own image – it comes from wherever the agent's came from. If the agent
itself restarts during the copy, the next one removes the unfinished copy
(the PVC stays frozen) and starts it again; after the swap it finishes the
switch.

**If lhtx is killed** during a local shrink (laptop closed), the PVC stays
frozen and the copy pod, the temporary PVC and the new volume stay.
`lhtx doctor` reports them; `lhtx --rw shrink --abort NS/PVC` removes
them and unfreezes – the app starts again on its old volume. If lhtx was
killed after the old PVC was deleted, `--abort` refuses and points to the
journal (`lhtx switch --status`).

**RWX volumes** are shared over NFS by Longhorn's share manager, and NFS
4.1 does not carry extended attributes (ACLs, file capabilities,
`user.*`). So the pre-copy reads over NFS while the app runs (owners,
modes, links and content come through), and once the PVC is frozen and
its volume detached, the old volume is set to `rwo` for the copy (the
annotation `lhtx.io/shrink-access-mode: rwx` remembers it) and the copy
pod mounts it as a block device through a temporary PV/PVC – the final
pass sets the extended attributes on every entry, and the comparison
checks them. Then the old volume goes back to `rwx` (also on any
failure), and the new volume, made `rwo` for the copy, becomes `rwx`. If
the old volume cannot be set back, the PVC stays frozen rather than
starting its pods on an unshared volume; `lhtx doctor` reports such a
volume as critical and `--abort` sets it back. In the lab: two pods
writing over NFS, encrypted or not, the app down for about 40 s of which
the copy was 0.5 s; killing the agent mid-copy, the next one cleaned up,
set the volume back and finished.

Checks before anything changes: raw block PVCs and namespaces
enforcing the `restricted` Pod Security level are refused; the size must be
smaller and leave 10 % free of what is used (from kubelet's stats when
allowed, otherwise the copy pod checks it). lhtx notes: a PVC managed by
Flux/Helm needs its size changed in Git first (or the next sync expands it
again); a StatefulSet's claim template keeps the old size; the new volume
starts a new backup chain (first backup full, the old backups stay
restorable); snapshots stay with the old volume. Databases are better
shrunk with their own tools (dump/restore).

The app is down from step 5 to the end: stopping it, copying what changed
since the pre-copy (in the lab: 0.6 s for an app writing five files a
second, against 33 s for the full 1.4 GiB; 1.7 s to check 80 000
unchanged files), starting it again. Without the
pre-copy, about as long as reading the used data twice (copy, compare).
`--dry-run` shows the plan; `--image` / `--image-pull-secret` for clusters
without access to ghcr.io (default: env `LHTX_SHRINK_IMAGE`, else the image
of this lhtx version). In the TUI: `K` in the volume details.
