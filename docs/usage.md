# Usage

[← README](../README.md)

## Commands

```
lhtx                                 # interactive TUI
lhtx --demo                          # TUI/CLI on a simulated cluster (no kubeconfig needed)

lhtx volumes [-o table|wide|json]    # volumes (table reads only volumes)
lhtx replicas [VOLUME]               # replicas with node + disk path, mode, rebuild %
lhtx nodes [-o wide]                 # nodes → disks: usage, scheduled, replicas, SMART
lhtx disk NODE[:PATH]                # all replicas on a disk (reverse view)
lhtx rebuilds [-w]                   # rebuilds/purges/restores (+ backups with -w), speed + ETA
lhtx snapshots VOLUME                # snapshot chain of a volume (newest first, revert branches indented)
lhtx backups [VOLUME] [--max-age 36h] # backup age per volume, or the backups of one volume (also deleted ones)
lhtx backupvolumes                   # volumes with backups in the backup store, deleted ones first
lhtx events VOLUME                   # events of volume, engine, replicas and PVC
lhtx jobs [VOLUME]                   # recurring jobs (that apply to the volume)
lhtx settings [FILTER]               # Longhorn settings
lhtx orphans                         # orphaned replica data / instances
lhtx engineimages                    # engine images, state, how many volumes use them
lhtx targets                         # backup targets
lhtx contexts                        # kubeconfig contexts
lhtx history [VOLUME] [--bad --since 168h]  # rebuilds, failures, backups – kept by the lhtx agent
lhtx capacity                    # when will the disks be full (trend; lhtx agent)
lhtx doctor [--backup-max-age 36h]   # health check, exit code 0/1/2/3 (ok/warn/crit/error)
lhtx overview [--contexts a,b]       # doctor summary for all kubeconfig contexts, one line each (+ agent)
lhtx drain-check NODE                # what happens if NODE goes away (reboot/drain)
lhtx mounts [--node N] [--min-wait 2m] # pods stuck mounting Longhorn volumes, per node, kubelet's view
lhtx rwx [-o wide]                   # RWX volumes: share manager, pod, endpoints, NFS options, clients
lhtx placement VOLUME                # why Longhorn does (not) place its replicas, per node and disk
lhtx placement --size 50Gi [--replicas 3] # would a new volume fit?
lhtx matrix [--problems]             # replica distribution volumes × nodes, shared node/disk first
lhtx diag VOLUME                     # diagnostics bundle (tar.gz) for a bug report
lhtx measure [-d 60s] [--backups]    # measure startup + watch traffic per resource
lhtx config [init|path]              # effective settings of the context, example config file

VOLUME = Longhorn volume / PV name, namespace/pvc, a bare PVC name if unique,
         or a fuzzy query if it matches exactly one volume ("imm valk")
--context NAME  --kubeconfig PATH  -n longhorn-system  -v (print wire bytes)
--refresh auto|watch|30s (volume updates, env LHTX_REFRESH)
```

Write commands: [write-actions.md](write-actions.md). Freeze: [freeze.md](freeze.md).

## TUI

![TUI views: snapshot tree with a revert branch, replica matrix, health, backup volumes, recurring jobs, backup targets](views.gif)

| Key | |
|---|---|
| `1` … `7` | Volumes · Nodes → disks → replicas · Activity · Settings · Health · Backups (backup volumes → backups, `J` recurring jobs, `t` backup targets) · Orphans (only if there are any) |
| `C` | snapshots panel (write mode): clone the snapshot into a new volume (+ PV/PVC) – look at old data without reverting |
| `B` | health tab (write mode): back up all attached volumes whose backup is late or missing (one confirmation) |
| `Z` | volume list / details (write mode): freeze the selected volumes, this one, all on its node or ALL (Longhorn upgrade) – plan, `Y`, live log; `Z` again lists active freezes to unfreeze; the header shows `❄ N frozen` |
| `w` | volume details (write mode): switch a PVC to this volume – back to the old one after a restore, or to a clone |
| `K` | volume details (write mode): shrink its PVC to a smaller size – plan, steps, live log, `x` cancels ([shrink](restore-switch.md#shrink-a-volume)) |
| `r` … then `switch` | in the restore form choose "switch NS/PVC to it": restore + switch with a step preview, live log, `x` cancels (rolls back until the old PVC is deleted); lhtx does not quit while it runs |
| `r` | backups tab (write mode): restore the newest backup of the backup volume, or the selected backup in a backup list, into a new volume (+ PV/PVC) |
| `n`, `enter`, `X` | recurring jobs / backup targets pages (write mode): new, edit, delete – forms with live checks (cron: next runs; targets: secret picker); `s` syncs a target now |
| `↑↓`/`jk`, `g`/`G` | move |
| `enter` | open (volume detail, disk, …) |
| `esc` / `q` | back (quit on the top level) |
| `/` | fuzzy filter like fzf: `valk data` finds `valkey-data`; space = and, `'exact`, `!not`, `^start`, `end$` |
| `p` | volume list: problems and activity first (toggle) |
| `i` | nodes view: drain check for the node |
| `m` | nodes view: replica matrix (volumes × nodes; replicas sharing a node or disk first) |
| `s` `b` `e` `J` `H` `P`, `tab` | detail panels: snapshots, backups, events, recurring jobs, history (needs the lhtx agent), placement (why replicas go where, see [replica placement](#replica-placement)) |
| `ctrl+r` | reload panel (details) or reload volumes (poll mode); resume after the traffic budget paused updates |
| `c` | switch kubeconfig context; `o` there checks Longhorn health in all clusters (like `lhtx overview`) |
| `W` | write mode on/off (red **RW** badge; or start with `lhtx --rw`) |
| `S` `B` `R` `U` `E` | snapshot · backup · replica count · engine image · expand (volume list / details) |
| `A` | attach to a node (normal or maintenance) / detach a manually attached volume |
| `space` (jobs panel `J`) | assign / unassign the recurring job or group; the panel lists all of them with how they apply (direct / via group) |
| `space`, `ctrl+a` | volume list: select volumes / all filtered ones (`esc` clears); `S` `B` `R` `U` `E` then act on all selected |
| `X`, `[` `]` | delete replica (selected with `[` `]` in details, or in the disk view) |
| `D` | delete snapshot (snapshots panel); `B` there backs up the selected snapshot |
| `C` `E` | replica scheduling / eviction on/off (nodes view, node or disk row) |
| `X`, `ctrl+x` | orphans tab: delete one / all cleanable orphans that are not RISKY |
| `enter` (Settings) | change value |
| `,` | lhtx settings of the connected context: value and where it comes from; `enter` changes it for this context or all (writes `config.yaml`, comments stay, applied at once) |
| `T` | choose a theme (live preview while moving, `enter` keeps, `esc` back; keep it for good with `theme:` in the config) |
| `?` | help |

Every action opens a dialog with the request: `y` applies (`Y` for
destructive ones), `d` runs a server-side dry run, `esc` cancels.

## Health check

![CLI: lhtx doctor with exit code, lhtx matrix --problems, lhtx backupvolumes](cli.gif)

`lhtx doctor` (and tab `5` in the TUI) reports what is wrong right now:

- faulted/degraded volumes, volumes with only one healthy replica left
- replicas of one volume on the same disk (or node)
- nodes/disks not ready, disks ≥ 80 % / 90 % full, disks Longhorn no longer
  schedules on (with Longhorn's reason)
- SMART: status not PASSED, SSD life below 20 % / 10 %, reallocated/pending sectors
- unavailable backup targets
- backup age: last backup older than twice the interval of the backup recurring
  job that applies to the volume (`--backup-max-age` also covers volumes
  backed up by other means)
- rebuilds running for hours, and a suspected deadlock: a replica rebuilds for
  more than 30 minutes while a snapshot of the same volume waits for removal or
  a purge runs (hint: the known workaround is lowering the replica count until
  the snapshot is purged)
- orphans
- stuck mounts and the CSI side, RWX share managers (see below)

Engines and snapshots are only read for volumes that are rebuilding. Exit codes
follow Nagios (0 ok, 1 warning, 2 critical, 3 error), so `lhtx doctor --min warning`
fits cron or a monitoring check. `-o wide` shows hints, `-o json` is for scripts.

`lhtx overview` runs the same checks against all kubeconfig contexts (or
`--contexts a,b`), 4 at a time, and prints one line per cluster: Longhorn
version, volumes, attached, degraded, faulted, rebuilding, nodes ready and the
worst finding (`-o wide` adds traffic and duration). Clusters without
Longhorn show `no Longhorn` and do not affect the exit code
(`--ignore-not-installed` hides them). Unreachable clusters show the error.
Exit code: worst of all clusters, 3 if one could not be checked.

## Stuck mounts

A node can look healthy in Longhorn and Kubernetes while pods there never get
their volume: kubelet or the Longhorn CSI plugin on that node hangs, the
node stays Ready, and often not even an event says so – the pod just sits in
`ContainerCreating`. `lhtx mounts` (also part of `doctor` and the Health tab)
finds those pods and says how far each got:

| stage | meaning |
|---|---|
| `attach` | the volume is not attached to the node yet (Longhorn / kube-controller-manager side) |
| `attach-error` | attaching failed (the VolumeAttachment's error) |
| `mount` | attached, but kubelet / the CSI plugin on the node did not mount it |

```
NODE     POD             PVC              WAITING   STAGE   DETAIL
node-2   shop/db-0       shop/data-db-0   14 min    mount   no events from kubelet; attached by Longhorn, but not mounted on the node

NODE     READY   CSI DRIVER   CSI PLUGIN   STUCK            KUBELET
node-2   true    ok           ready        1 (up to 14 min) healthz ok, 3/4 Longhorn volumes mounted, failed: NodeStageVolume DeadlineExceeded 6

CRITICAL node/node-2: 1 pod(s) wait for up to 14 min for Longhorn volumes that are attached here but not mounted – kubelet or the CSI plugin on this node hangs
  → first restart the CSI plugin (lhtx --rw restart csi-plugin node-2); if that does not help, restart kubelet: systemctl restart rke2-agent (on the node)
```

Per node it checks that Longhorn's CSI driver is registered with kubelet
(CSINode) and that the `longhorn-csi-plugin` pod is ready. For nodes with
pods stuck at `mount` (and `--node`), lhtx asks kubelet itself through the
API server's node proxy: `/healthz` (a hung sync loop shows up there),
the volume manager's Longhorn volumes wanted vs. mounted, failed mount and
CSI calls since kubelet started, and whether kubelet lists the volume as in
use at all (if not, its volume manager is not working on it). That needs
`get` on `nodes/proxy`; without it lhtx says so. A kubelet that does not
answer within 10 seconds is reported as hanging.

The hint fits how kubelet runs on the node: `rke2-agent` / `rke2-server`,
`k3s-agent` / `k3s`, `talosctl service kubelet restart` on Talos, otherwise
`systemctl restart kubelet`. lhtx never restarts anything itself.

Traffic: one LIST of pending pods (field selector) plus CSINodes and the CSI
plugin pods; only when pods wait: their VolumeAttachments (GET by name),
events and nodes, and kubelet's metrics (~300 KB per asked node).

The agent runs the same check every minute and records stuck mounts in the
volume's history (see [agent.md](agent.md#stuck-mounts)).

## RWX volumes

Longhorn serves each ReadWriteMany volume over NFS from a share-manager pod.
When it fails, client pods hang or get I/O errors – often while everything
looks green. `lhtx rwx` shows the chain per RWX volume, and `doctor` reports
what is wrong with it:

```
PVC / VOLUME   STATE      SHARE     POD NODE   POD                               RESTARTS   ENDPOINTS   CLIENTS   NFS OPTIONS
shop/uploads   attached   running   node-3     ready 13 min, NFS NOT ANSWERING   0          1           2         vers=4.1,noresvport,softerr,timeo=600,retrans=5 (default)

CRITICAL shop/uploads: the NFS server does not answer (lhtx agent, for 3 min) although its pod is Ready – client pods hang
  → lhtx --rw restart share-manager pvc-… (Longhorn starts a new one; clients reconnect)
```

- share manager in `error`, or in use but not `running`
- share-manager pod gone or not ready, restarts in the last 24 hours (with
  the reason: OOMKilled, exit code …) – clients may have seen I/O errors
- the Service has no ready endpoint; the pod runs on another node than the
  volume is attached to
- NFS options: the StorageClass's `nfsOptions` or Longhorn's default;
  `hard` is noted (client I/O blocks for good while the server is away)
- **NFS server not answering**: only the [agent](agent.md#rwx-volumes) can
  tell – it pings every share manager from inside the cluster

A pod stuck mounting an RWX volume (`lhtx mounts`) points to the share
manager, not to the node. Fixes, with preview and `--rw`:

```
lhtx --rw restart share-manager VOLUME   # Longhorn starts a new NFS server; clients reconnect
lhtx --rw restart csi-plugin NODE        # the node's longhorn-csi-plugin pod (stuck mounts)
```

Reads only if there are RWX volumes: ShareManagers, the share-manager pods,
their EndpointSlices, the cluster history, and (`lhtx rwx` only) one PV per
RWX volume.

## Replica placement

A volume stuck `degraded` with `ReplicaSchedulingFailure: insufficient
storage` does not say which node lacks what. `lhtx placement VOLUME` (alias
`why`) goes through Longhorn's replica scheduler checks for every node and
disk – node ready and schedulable (cordoned), scheduling allowed, node and
disk tags against the volume's selectors, disk type and condition, free
space (`storage-minimal-available-percentage` must stay free after the
volume's actual size), over-provisioning (scheduled sizes plus the volume's
size within `storage-over-provisioning-percentage` of max − reserved), and
replica anti-affinity per node, zone and disk:

```
Volume pvc-7f9a…: 15 GiB (actual 0 B), 1 of 2 replicas placed
Longhorn: cannot place a replica – ReplicaSchedulingFailure: insufficient storage

NODE    ZONE   VERDICT   DISK                 AVAILABLE   SCHEDULED / LIMIT     WHY
node-1  -      NO        /var/lib/longhorn/   19.4 GiB    16.3 GiB / 20.6 GiB   over-provisioned: 16.3 GiB scheduled + 15.0 GiB > limit 20.6 GiB (100 % of 20.6 GiB)
node-2  -      NO        /var/lib/longhorn/   20.1 GiB    17 GiB / 20.6 GiB     over-provisioned: 17.0 GiB scheduled + 15.0 GiB > limit 20.6 GiB (100 % of 20.6 GiB)
node-3  -      NO        -                    -           -                     already holds a replica (…; replica-soft-anti-affinity off)

✘ 1 replica(s) missing, room for 0 – free space, lower the replica count, or allow soft anti-affinity
```

`--size 50Gi [--replicas 3] [--node-selector …]` asks the same for a volume
that does not exist yet (before a restore or clone; the space check then
counts no data yet). `doctor` and the Health tab report volumes Longhorn
cannot place, with the reasons per node; `lhtx shrink` warns when the new
volume will not fit; in the TUI: `P` in the volume details. Reads: nodes, the volume and its replicas, seven
settings (doctor: only when a volume is not scheduled).

## Node maintenance

`lhtx drain-check NODE` (or `i` on a node in the TUI) shows, before a reboot,
drain or replacement, what happens if the node goes away: volumes whose only
healthy replica is there (critical, they become unavailable), volumes with ≥ 3
replicas left with a single healthy one or with a rebuild running elsewhere
(warning), degraded 2-replica volumes and volumes attached on the node (info),
plus Longhorn's `node-drain-policy`. Same exit codes as `doctor`.

## Diagnostics bundle

`lhtx diag VOLUME` writes `lhtx-diag-<pvc>-<time>.tar.gz` with a summary
(replicas, engine rebuild/purge status, replica mode change times, snapshots
incl. pending removals, findings), the YAML of volume, engines, replicas,
snapshots and the nodes involved (without managedFields), events, and
instance-manager/longhorn-manager logs from those nodes, filtered to lines that
mention the volume. Logs are limited (`--since 6h`, `--log-limit 5MiB` per
container, `--no-logs`, `--full-logs`). Use it to capture a stuck rebuild or
snapshot removal before trying to fix it.

With the [lhtx agent](agent.md) the logs are read and filtered in the
cluster and only the matching lines travel (a `LogQuery`; in the lab 187 KiB
instead of 2.5 MiB for the same bundle). `--local-logs` reads them here;
`--full-logs` always does.
