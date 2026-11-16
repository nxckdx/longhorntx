# lhtx

Low-traffic terminal UI (k9s style) and CLI for [Longhorn](https://longhorn.io),
as a replacement for the Longhorn web UI on slow or metered links.

The Longhorn UI refreshes over WebSockets once per second and can easily use
several MB/s (longhorn/longhorn#2372, #4211, #11134). lhtx talks **only to the
Kubernetes API** (your kubeconfig, like kubectl), reads the `*.longhorn.io`
CRDs and uses watches instead of polling.

- read-only by default; changes only with `--rw` (or `W` in the TUI), always
  with a preview and confirmation, plus `--dry-run` / server-side dry run
- multiple clusters through kubeconfig contexts (`--context`, `c` in the TUI)
- shows which **disk** each replica is on, not just the node
- rebuild progress with speed and ETA
- also works as a kubectl plugin: `kubectl lhtx …`

Requires Longhorn ≥ 1.6 (`longhorn.io/v1beta2`). Tested with v1.12.1.

## Usage

```
lhtx                                 # interactive TUI

lhtx volumes [-o table|wide|json]    # volumes (table reads only volumes)
lhtx replicas [VOLUME]               # replicas with node + disk path, mode, rebuild %
lhtx nodes [-o wide]                 # nodes → disks: usage, scheduled, replicas, SMART
lhtx disk NODE[:PATH]                # all replicas on a disk (reverse view)
lhtx rebuilds [-w]                   # rebuilds/purges/restores (+ backups with -w), speed + ETA
lhtx snapshots VOLUME                # snapshots of a volume
lhtx backups VOLUME                  # backups of a volume
lhtx events VOLUME                   # events of volume, engine, replicas and PVC
lhtx jobs [VOLUME]                   # recurring jobs (that apply to the volume)
lhtx settings [FILTER]               # Longhorn settings
lhtx targets                         # backup targets
lhtx contexts                        # kubeconfig contexts
lhtx measure [-d 60s] [--backups]    # measure startup + watch traffic per resource

VOLUME = Longhorn volume / PV name, namespace/pvc, or a bare PVC name if unique
--context NAME  --kubeconfig PATH  -n longhorn-system  -v (print wire bytes)
```

### Changes (write mode)

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
lhtx set replicas VOLUME COUNT             # volume must be attached
lhtx set setting NAME VALUE
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
Volume/Node/Setting. Patches carry the `resourceVersion` the decision was
based on, so they fail instead of overwriting a newer state. Deletes use a
UID precondition.

TUI:

| Key | |
|---|---|
| `1` `2` `3` `4` | Volumes · Nodes → disks → replicas · Activity · Settings |
| `↑↓`/`jk`, `g`/`G` | move |
| `enter` | open (volume detail, disk, …) |
| `esc` / `q` | back (quit on the top level) |
| `/` | filter |
| `s` `b` `e` `J`, `tab` | detail panels: snapshots, backups, events, recurring jobs |
| `ctrl+r` | reload panel |
| `c` | switch kubeconfig context |
| `W` | write mode on/off (red **RW** badge; or start with `lhtx --rw`) |
| `S` `B` `R` | snapshot · backup · replica count (volume list / details) |
| `X`, `[` `]` | delete replica (selected with `[` `]` in details, or in the disk view) |
| `D` | delete snapshot (snapshots panel); `B` there backs up the selected snapshot |
| `C` `E` | replica scheduling / eviction on/off (nodes view, node or disk row) |
| `enter` (Settings) | change value |

Every action opens a dialog with the request: `y` applies (`Y` for
destructive ones), `d` runs a server-side dry run, `esc` cancels.
| `?` | help |

## Install / develop (Nix)

```
nix run github:nxckdx/longhorntx           # or: nix build → result/bin/{lhtx,kubectl-lhtx}
nix develop                                # go, gopls, kubectl, pv
go build -o lhtx ./cmd/lhtx && go test ./...
```

After changing `go.mod`, update `vendorHash` in `flake.nix` (set it to
`pkgs.lib.fakeHash`, run `nix build`, copy the `got:` hash).

## Layout

```
cmd/lhtx            entry point (also used as kubectl-lhtx)
internal/kube       kubeconfig/contexts, TCP-level traffic counter
internal/longhorn   slim CRD structs, one-shot client, informer store, joins
internal/cli        cobra commands
internal/tui        Bubble Tea UI
```

`internal/longhorn` does **not** import `longhorn-manager`: that module pulls
in a huge dependency tree and pins one Longhorn version. Instead, lhtx has its
own small structs with only the fields it needs. Unknown fields are ignored, so
the structs tolerate differences between Longhorn versions. Objects are read
through the dynamic client and converted right away (informer transform), so
the caches hold only those structs.

## Data model (verified against Longhorn v1.12.1)

| What | Where |
|---|---|
| replica → node / disk | `replica.spec.nodeID`, `spec.diskID` (= disk UUID), `spec.diskPath`, `spec.dataDirectoryName`; also labels `longhornnode`, `longhorndiskuuid` |
| disk name | key of `node.status.diskStatus`, matched via `diskStatus[*].diskUUID == replica.spec.diskID` |
| volume ↔ replicas/engines | label `longhornvolume=<volume>` (also `spec.volumeName`) |
| PVC | `volume.status.kubernetesStatus.{namespace,pvcName}` |
| rebuild | `engine.status.rebuildStatus` |
| snapshot purge | `engine.status.purgeStatus` |
| restore | `engine.status.restoreStatus` |
| clone | `engine.status.cloneStatus` |
| replica mode (RW/WO/ERR) | `engine.status.replicaModeMap[<replica name>]` |
| backup | `backup.status.{state,progress}`, label `backup-volume` |
| snapshot | `snapshots.longhorn.io`, label `longhornvolume`; `status.labels.type` = `bak` (for a backup) / `snap` |
| recurring jobs of a volume | volume labels `recurring-job.longhorn.io/<job>` and `recurring-job-group.longhorn.io/<group>` = `enabled` |
| SMART | `node.status.diskStatus[*].healthData` (Longhorn ≥ 1.10); for wear, the *normalized* value is the remaining life (Crucial reports % used as raw value) |
| Longhorn version | setting `current-longhorn-version` |

Gotcha: `rebuildStatus`, `purgeStatus`, `restoreStatus` and `cloneStatus` are
keyed by **replica address** (`tcp://ip:port`), not by replica name.
`engine.status.currentReplicaAddressMap` (name → `ip:port`) is used to look
them up, with `replica.status.storageIP:port` as fallback.

v2 data engine: `diskType: block`, and `diskPath` is the block device. lhtx
shows it as is.

## Traffic

Example: a small cluster (2 nodes, 24 volumes, 42 replicas), idle, measured
with `kubectl … --watch-only -o json` over 60 s:

| Resource | per minute |
|---|---|
| engines | ~100 KB |
| volumes | ~90 KB |
| replicas | 0 |
| nodes | ~15 KB |
| settings | 0 |

Measure your own cluster with `lhtx measure` (see below).

Findings:

- **Volumes and engines change on every write.** Volumes update
  `status.actualSize`; engines update the size of the `volume-head` snapshot.
  Their traffic grows with the write load, not just with the number of volumes.
- **`kubectl` hides `managedFields`, but they do go over the wire:** +63 % for
  volumes and +80 % for engines. The measurement command above therefore shows
  less than lhtx (or any client) actually receives.
- **LISTs are gzip-compressed, watch streams are not.** `lhtx volumes`
  (one LIST, ~130 KB of JSON) costs ~15 KB on the wire.
- **client-go ≥ 1.35 sends the initial sync over the watch stream by default**
  (WatchListClient), and that stream is uncompressed. lhtx turns this off per
  informer. The TUI's startup traffic dropped from 309 KiB to 59 KiB.

Strategy:

- Watched all the time: volumes, replicas, nodes.
- Engines are only watched per volume (label selector), and only while the
  detail view of that volume is open or while one of its replicas looks like
  it is rebuilding (`running` without `healthyAt`; the replica watch is free).
- Backups (one CR per backup over the whole retention: 696 backups = 2.4 MB here)
  are watched **from now on**: a LIST with `limit=1` yields the resourceVersion,
  and the watch starting there sends only changes, without an initial list.
  Running backups update their progress constantly, so they show up within
  seconds, even if they started before lhtx.
- Snapshots, backups, events and recurring jobs are loaded on demand when you
  open a panel (label or field selector), not watched. Events go through
  core/v1 using protobuf.
- Redraws happen at most ~5 times per second, however busy the cluster is.
- Result on the small cluster above: startup ~50–60 KiB, then **~60 KiB/min (~1 KiB/s)**
  idle in the volume list, compared with up to MB/s for the web UI.
- The header shows the bytes lhtx has received on the wire (measured at the
  TCP level, after TLS) and the current rate.

### Startup

The TUI does not wait for the data. It is usable right away, and every
resource appears as soon as its initial list has arrived:

- **volumes** and **nodes** (small) are loaded first, **replicas** (the
  largest list) only once the volumes are in, and the **backup watch** last.
  On a slow link they would otherwise share the bandwidth, and the volume
  list would show up later.
- Each page only waits for what it needs (the volume list: volumes; nodes:
  nodes). Columns that depend on missing data show `…`. Meanwhile the header
  shows `◐ loading replicas`.
- The initial lists use `resourceVersion=0` (served from the apiserver's
  watch cache) and are gzip-compressed. The apiserver compresses responses
  only above 128 KB, so small lists (nodes, settings) go over the wire
  uncompressed.

To simulate a slow link: `LHTX_THROTTLE_KBPS=50 lhtx` (limits the download).

### Measuring a cluster

```
lhtx measure            # 60 s; lhtx measure -d 5m --context prod
```

Lists every resource once (like the TUI at startup: time and bytes on the
wire, TLS/gzip/managedFields included) and then watches all of them for the
duration without an initial list. Each resource uses its own connection, so
the bytes can be attributed exactly. At the end it prints an estimate of
TUI startup and idle traffic. Example (small cluster, 24 volumes):

```
RESOURCE    ITEMS   LIST TIME   LIST BYTES   WATCH EVENTS   WATCH BYTES   PER MINUTE
volumes     24      17ms        12.2 KiB     5              27.4 KiB      54.7 KiB
replicas    42      19ms        14 KiB       0              66 B          131 B
nodes       2       4ms         18.1 KiB     0              67 B          133 B
engines     24      22ms        20.8 KiB     5              34.8 KiB      69.6 KiB
```

## Roadmap

1. **MVP (read-only)** ✔ volume list, volume detail with replicas (node + disk),
   rebuild progress with speed/ETA, nodes/disks (node → disk → replicas, SMART),
   events, `lhtx rebuilds -w`, context switching
2. **Read more data** ✔ snapshots, backups, settings, recurring jobs, backup targets
   · open: settings and backup-volume view in the TUI, snapshot tree
3. **Actions** ✔ snapshot/backup create, snapshot/replica delete, replica count,
   scheduling/eviction for nodes and disks, settings – with preview, confirmation,
   client and server dry run · open: attach/detach, volume expansion, recurring jobs
4. Multi-cluster overview, more traffic statistics
