# Changes (write mode)

[← README](../README.md)

![Write mode: replica count, expansion and a restore into a new volume, each shows the exact request before anything is sent](write.gif)

Nothing is changed without `--rw`. Every command first prints the exact request
(verb, object, changed fields, JSON body). `--dry-run` stops there.
`--dry-run=server` sends the request with `dryRun=All`: the apiserver and
Longhorn's admission webhooks validate it, but nothing is persisted. Both work
without `--rw`. Destructive actions ask you to type the object name, `--yes`
skips the prompt (needed without a terminal).

```
lhtx create snapshot VOLUME [--name N]
lhtx create backup VOLUME [--snapshot S]   # without --snapshot: snapshot, wait, back up
lhtx delete snapshot NAME
lhtx delete replica NAME                   # refused for the only healthy replica
lhtx delete orphan NAME|--all-cleanable    # Longhorn deletes the data dir if cleanable (bulk skips risky ones)
lhtx set replicas VOLUME COUNT             # volume must be attached
lhtx restart share-manager VOLUME          # RWX: Longhorn starts a new NFS server (lhtx rwx)
lhtx restart csi-plugin NODE               # the node's longhorn-csi-plugin pod (lhtx mounts)
lhtx set setting NAME VALUE
lhtx upgrade-engine VOLUME... --image v1.12.1   # like "Upgrade Engine" in the web UI
lhtx expand VOLUME 20Gi|+5Gi               # through the PVC if bound (StorageClass must allow it)
lhtx restore BACKUP|VOLUME [--name N] [--pvc NS/NAME|auto]   # backup → NEW volume (+ PV/PVC)
lhtx clone VOLUME [--snapshot S] [--pvc NS/NAME|auto]       # volume/snapshot → NEW volume (safe "revert")
lhtx restore VOLUME --switch               # restore, then switch the PVC to it (pods stopped by a freeze)
lhtx switch NS/PVC --to-volume VOLUME      # point a PVC to another Longhorn volume (and back)
lhtx switch NS/PVC --from-backup BACKUP    # restore, then switch (same as restore --switch)
lhtx freeze --all|--node N|NS/PVC…         # stop all pods using the volumes, detach (maintenance)
lhtx unfreeze [NAME|--all]                 # end it, pods start again
lhtx agent status|manifests                # optional in-cluster agent that carries freezes out
lhtx attach VOLUME NODE [--maintenance]    # like Attach in the web UI (ticket "longhorn-ui")
lhtx detach VOLUME                         # removes only that ticket, never the CSI one
lhtx jobs add|remove VOLUME JOB|group:NAME...   # recurring job labels on the volume
lhtx jobs create NAME --task backup --cron "0 2 * * *" --retain 14 --groups default
lhtx jobs edit NAME [--cron …] [--retain …] [--groups …]   # only the given flags change
lhtx jobs delete NAME                      # preview lists the volumes that lose the job
lhtx jobs next "0 2 * * 1-5"               # when a cron runs (UTC like Longhorn) in your time zone
lhtx targets create NAME --url s3://bucket@region/ --secret S [--poll 5m]
lhtx targets edit|delete|sync NAME         # sync = re-read the store now
lhtx cordon|uncordon NODE[:DISK]           # replica scheduling off/on
lhtx evict NODE[:DISK] [--cancel]          # also disables scheduling (Longhorn requires it)

# examples
lhtx evict node-3:/var/lib/longhorn --dry-run=server
lhtx --rw set replicas gitea/gitea-data 3
```

All changes go through Longhorn CRs, the same objects longhorn-manager's own
API creates (verified against v1.12.1): Snapshot CR (`spec.volume`,
`spec.createSnapshot`), Backup CR (`spec.snapshotName`, labels `backup-volume`
and `backup-target`), deleting Snapshot/Replica CRs, merge patches on
Volume/Node/Setting. Volumes bound to a PVC are expanded by patching the PVC,
like longhorn-manager does (the CSI resizer then grows volume and
filesystem). Attach/detach add or remove the attachment ticket `longhorn-ui`
in the VolumeAttachment CR, exactly like the web UI, so both can undo each
other's attach. Patches carry the `resourceVersion` the decision was
based on, so they fail instead of overwriting a newer state. Deletes use a
UID precondition. A restore creates a Longhorn Volume with `spec.fromBackup`
(the backup's URL, size from the backup), then – if asked – a PV
(`driver.longhorn.io`, `volumeHandle` = volume, reclaim `Retain`, Longhorn's
static StorageClass) and a PVC bound to it by `volumeName`, like the web UI's
"Restore" + "Create PV/PVC"; existing volumes/PVs/PVCs of those names are
refused up front. A clone works the same with `spec.dataSource`
(`vol://VOLUME` or `snap://VOLUME/SNAPSHOT`) instead of `fromBackup`.
Changes to objects owned by Flux, Argo CD or Helm (labels
and annotations of those tools) carry a warning: the next reconcile reverts
them, change them in Git instead. The credential secret picker for backup
targets reads secret **names** through the metadata API – secret data never
travels.

## Orphans

Orphans: `DataCleanable` only says Longhorn *can* delete the data, not that
it is safe. Replica data of a volume that is deleted, faulted, degraded or
has no healthy replica is marked **RISKY** – it may be the last usable copy.
Bulk delete (`--all-cleanable`, `ctrl+x`) skips those; deleting one by name
shows a clear warning.

More: [freeze](freeze.md) · [restore and switch](restore-switch.md) ·
[encrypted volumes](encryption.md) · [agent](agent.md)
