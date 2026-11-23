# lhtx

Low-traffic terminal UI (k9s style) and CLI for [Longhorn](https://longhorn.io),
as a replacement for the Longhorn web UI on slow or metered links.

The Longhorn UI refreshes over WebSockets once per second and can easily use
several MB/s (longhorn/longhorn#2372, #4211, #11134). lhtx talks **only to the
Kubernetes API** (your kubeconfig, like kubectl), reads the `*.longhorn.io`
CRDs and uses watches instead of polling.

![lhtx demo: volume list with a running rebuild, volume detail, nodes, health checks and a write action preview](docs/demo.gif)

Try it without a cluster: `lhtx --demo` (or `nix run github:nxckdx/longhorntx -- --demo`)
starts the TUI on a simulated in-memory cluster with made-up volumes and a
rebuild that keeps running. Every command accepts `--demo`.

- read-only by default; changes only with `--rw` (or `W` in the TUI), always
  with a preview and confirmation, plus `--dry-run` / server-side dry run
- multiple clusters through kubeconfig contexts (`--context`, `c` in the TUI)
- shows which **disk** each replica is on, not just the node
- rebuild progress with speed and ETA
- also works as a kubectl plugin: `kubectl lhtx …`

Requires Longhorn ≥ 1.6 (`longhorn.io/v1beta2`). Tested with v1.12.1.

**Only the v1 data engine is supported and tested.** Volumes on the v2 data
engine (SPDK, block disks) may be shown, but nothing is verified for them, and
write actions are not tested.

## Usage

```
lhtx                                 # interactive TUI
lhtx --demo                          # TUI/CLI on a simulated cluster (no kubeconfig needed)

lhtx volumes [-o table|wide|json]    # volumes (table reads only volumes)
lhtx replicas [VOLUME]               # replicas with node + disk path, mode, rebuild %
lhtx nodes [-o wide]                 # nodes → disks: usage, scheduled, replicas, SMART
lhtx disk NODE[:PATH]                # all replicas on a disk (reverse view)
lhtx rebuilds [-w]                   # rebuilds/purges/restores (+ backups with -w), speed + ETA
lhtx snapshots VOLUME                # snapshot chain of a volume (newest first, revert branches indented)
lhtx backups [VOLUME] [--max-age 36h] # backup age per volume, or the backups of one volume
lhtx events VOLUME                   # events of volume, engine, replicas and PVC
lhtx jobs [VOLUME]                   # recurring jobs (that apply to the volume)
lhtx settings [FILTER]               # Longhorn settings
lhtx orphans                         # orphaned replica data / instances
lhtx engineimages                    # engine images, state, how many volumes use them
lhtx targets                         # backup targets
lhtx contexts                        # kubeconfig contexts
lhtx doctor [--backup-max-age 36h]   # health check, exit code 0/1/2/3 (ok/warn/crit/error)
lhtx overview [--contexts a,b]       # doctor summary for all kubeconfig contexts, one line each
lhtx drain-check NODE                # what happens if NODE goes away (reboot/drain)
lhtx diag VOLUME                     # diagnostics bundle (tar.gz) for a bug report
lhtx measure [-d 60s] [--backups]    # measure startup + watch traffic per resource
lhtx config [init|path]              # effective settings of the context, example config file

VOLUME = Longhorn volume / PV name, namespace/pvc, a bare PVC name if unique,
         or a fuzzy query if it matches exactly one volume ("imm valk")
--context NAME  --kubeconfig PATH  -n longhorn-system  -v (print wire bytes)
--refresh auto|watch|30s (volume updates, env LHTX_REFRESH)
```

### Configuration (per context)

`~/.config/lhtx/config.yaml` (`$XDG_CONFIG_HOME`, or the path in
`LHTX_CONFIG`) holds defaults and overrides per kubeconfig context, so for
example a small homelab runs live in write mode and a large customer cluster
polls, has a red badge and a traffic budget. `lhtx config init` writes a
commented example; `lhtx config [--context NAME]` shows the effective values
and where each one comes from.

```yaml
defaults:
  refresh: auto          # auto | watch | 30s
  readOnly: true
contexts:
  homelab:
    readOnly: false      # TUI starts in write mode, CLI writes without --rw
  "customer-*":          # glob; the more specific pattern wins, exact names last
    color: red           # context badge (name or 0-255)
    refresh: 30s
    trafficBudget: 200MiB
  kind-*:
    overview: false      # skipped by lhtx overview
```

Precedence: flag/env > context > defaults > built-in. Settings: `refresh`,
`readOnly`, `color`, `trafficBudget`, `overview`, `namespace`. Unknown keys
and bad values are errors, so typos do not go unnoticed. `readOnly: false`
only skips `--rw`/`W`: previews and confirmations stay. Switching the context
in the TUI applies that context's settings (write mode turns off unless the
new context allows it or `--rw` was given).

**Traffic budget** (TUI): at 80 % of the budget the header shows the usage,
at 100 % all watches stop (`⏸ paused`); on-demand loads still work and
`ctrl+r` resumes with a fresh budget. It counts both directions of the
current connection.

### Health check

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

### Node maintenance

`lhtx drain-check NODE` (or `i` on a node in the TUI) shows, before a reboot,
drain or replacement, what happens if the node goes away: volumes whose only
healthy replica is there (critical, they become unavailable), volumes with ≥ 3
replicas left with a single healthy one or with a rebuild running elsewhere
(warning), degraded 2-replica volumes and volumes attached on the node (info),
plus Longhorn's `node-drain-policy`. Same exit codes as `doctor`.

### Diagnostics bundle

`lhtx diag VOLUME` writes `lhtx-diag-<pvc>-<time>.tar.gz` with a summary
(replicas, engine rebuild/purge status, replica mode change times, snapshots
incl. pending removals, findings), the YAML of volume, engines, replicas,
snapshots and the nodes involved (without managedFields), events, and
instance-manager/longhorn-manager logs from those nodes, filtered to lines that
mention the volume. Logs are limited (`--since 6h`, `--log-limit 5MiB` per
container, `--no-logs`, `--full-logs`). Use it to capture a stuck rebuild or
snapshot removal before trying to fix it.

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
lhtx delete orphan NAME|--all-cleanable    # Longhorn deletes the data dir if cleanable (bulk skips risky ones)
lhtx set replicas VOLUME COUNT             # volume must be attached
lhtx set setting NAME VALUE
lhtx upgrade-engine VOLUME... --image v1.12.1   # like "Upgrade Engine" in the web UI
lhtx expand VOLUME 20Gi|+5Gi               # through the PVC if bound (StorageClass must allow it)
lhtx attach VOLUME NODE [--maintenance]    # like Attach in the web UI (ticket "longhorn-ui")
lhtx detach VOLUME                         # removes only that ticket, never the CSI one
lhtx jobs add|remove VOLUME JOB|group:NAME...   # recurring job labels on the volume
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
UID precondition.

Orphans: `DataCleanable` only says Longhorn *can* delete the data, not that
it is safe. Replica data of a volume that is deleted, faulted, degraded or
has no healthy replica is marked **RISKY** – it may be the last usable copy.
Bulk delete (`--all-cleanable`, `ctrl+x`) skips those; deleting one by name
shows a clear warning.

TUI:

| Key | |
|---|---|
| `1` … `6` | Volumes · Nodes → disks → replicas · Activity · Settings · Health · Orphans (only if there are any) |
| `↑↓`/`jk`, `g`/`G` | move |
| `enter` | open (volume detail, disk, …) |
| `esc` / `q` | back (quit on the top level) |
| `/` | fuzzy filter like fzf: `valk data` finds `valkey-data`; space = and, `'exact`, `!not`, `^start`, `end$` |
| `p` | volume list: problems and activity first (toggle) |
| `i` | nodes view: drain check for the node |
| `s` `b` `e` `J`, `tab` | detail panels: snapshots, backups, events, recurring jobs |
| `ctrl+r` | reload panel (details) or reload volumes (poll mode); resume after the traffic budget paused updates |
| `c` | switch kubeconfig context |
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

Every action opens a dialog with the request: `y` applies (`Y` for
destructive ones), `d` runs a server-side dry run, `esc` cancels.
| `?` | help |

## Install

**Release binaries** (Linux, macOS, Windows; amd64/arm64): download the archive
for your platform from the [GitHub releases](https://github.com/nxckdx/longhorntx/releases),
put `lhtx` into your `PATH`, and for the kubectl plugin also link it as
`kubectl-lhtx`. Update later with:

```
lhtx update          # downloads the latest release, verifies the SHA256, replaces the binary
lhtx update --check  # only check
lhtx version
```

lhtx checks for new releases at most once a day (one small request to the
GitHub API, cached in `~/.cache/lhtx`) and shows `↑ vX.Y.Z` in the TUI header.
Turn it off with `LHTX_NO_UPDATE_CHECK=1`. Nix installs and development builds
never check and are updated through Nix.

**Go:** `go install github.com/nxckdx/longhorntx/cmd/lhtx@latest`

## Releases

Commits on `main` follow [Conventional Commits](https://www.conventionalcommits.org/)
(`feat:`, `fix:`, `docs:`, …). release-please keeps a release PR open with the
changelog and the `VERSION` bump; merging it creates the tag `vX.Y.Z` and the
GitHub release, and goreleaser attaches the binaries and `checksums.txt`. CI
runs gofmt, vet, tests and the Nix build on every push and pull request.

## Install / develop (Nix)

```
nix run github:nxckdx/longhorntx           # or: nix build → result/bin/{lhtx,kubectl-lhtx}
nix run github:nxckdx/longhorntx -- --context prod volumes   # flags after --
nix develop                                # go, gopls, kubectl, pv
go build -o lhtx ./cmd/lhtx && go test ./...
```

Two flake packages:

- `lhtx` (default): built from source.
- `lhtx-bin`: the prebuilt, statically linked binary from the latest GitHub
  release (no Go toolchain, no module download – useful on slow links).
  `nix/release.json` pins its version and hashes; the release workflow updates
  it after each release. Example NixOS module:

  ```nix
  # flake inputs: lhtx.url = "github:nxckdx/longhorntx";
  environment.systemPackages = [
    inputs.lhtx.packages.${pkgs.stdenv.hostPlatform.system}.lhtx-bin
  ];
  ```

### Shell completion

Completes commands, flags, contexts, volume/PVC, node:disk, replica, snapshot
and setting names. Names are cached for 10 minutes in `~/.cache/lhtx/` per
context, so pressing TAB does not list all volumes every time.

- **Nix packages** install the completion files for bash, zsh and fish. On
  NixOS with the package in `environment.systemPackages` and
  `programs.zsh.enable = true` nothing else is needed (open a new shell; with
  a stale cache run `rm ~/.zcompdump*`). With home-manager packages, make sure
  the shell's completion is enabled (`programs.zsh.enableCompletion`, default).
- **Release binaries:** add to `~/.zshrc` (or bash/fish equivalent):
  `source <(lhtx completion zsh)`
- **`kubectl lhtx <TAB>`** (kubectl ≥ 1.26) runs `kubectl_complete-lhtx`.
  The Nix packages include it; otherwise link it next to the binary:
  `ln -s lhtx /usr/local/bin/kubectl_complete-lhtx` (and `kubectl-lhtx`).

With `nix run`, arguments for lhtx must come after `--`; otherwise nix itself
parses flags like `--context`.

After changing `go.mod`, update `vendorHash` in `flake.nix` (set it to
`pkgs.lib.fakeHash`, run `nix build`, copy the `got:` hash).

## Layout

```
cmd/lhtx            entry point (also used as kubectl-lhtx)
internal/kube       kubeconfig/contexts, TCP-level traffic counter
internal/longhorn   slim CRD structs, one-shot client, informer store, joins
internal/cli        cobra commands
internal/tui        Bubble Tea UI
internal/demo       simulated cluster for --demo (fake dynamic client)
docs/demo.tape      VHS script for docs/demo.gif (vhs docs/demo.tape)
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

v2 data engine (not supported, see above): disks would have `diskType: block`
with the block device as `diskPath`; lhtx shows that as is.

## Traffic

Measured with `lhtx measure` (1 minute watch, wire bytes incl. TLS, gzip and
managedFields):

| Resource | Small cluster: objects | per minute | Large cluster: objects | List (gzip) | Watch per minute |
|---|---|---|---|---|---|
| volumes | 24 | ~55 KiB | 878 | 433 KiB | **10.3 MiB** (1859 events) |
| engines | 24 | ~70 KiB | 878 | 744 KiB | **11.7 MiB** (1736 events) |
| nodes | 2 | ~0 | 39 | 82 KiB | 124 KiB |
| replicas | 42 | 0 | 1754 | 678 KiB | ~0 |
| snapshots | 48 | 0 | 881 | 169 KiB | ~0 |
| settings | 116 | 0 | 116 | 103 KiB | ~0 |

(small cluster: 2 nodes; large cluster: 39 nodes)

Findings:

- **Volumes and engines change on every write.** Volumes update only
  `status.actualSize` (plus the managedFields timestamp), engines only the size
  of the `volume-head` snapshot (`lhtx measure --diff` shows this per field).
  Each event carries the whole object (~6 KiB, uncompressed), so the traffic
  grows with the write load. On the large cluster the volume watch alone costs
  ~10 MiB/min, ~620 MiB/hour. That is why lhtx polls volumes on large clusters
  (see below).
- **Replicas, snapshots, settings and backups are practically silent**; their
  watches cost almost nothing.
- **Startup is cheap**: ~1.2 MiB and ~1 s on the large cluster.
- **`kubectl` hides `managedFields`, but they do go over the wire:** +63 % for
  volumes and +80 % for engines.
- **LISTs are gzip-compressed, watch streams are not.** `lhtx volumes`
  (one LIST, ~130 KB of JSON) costs ~15 KB on the wire on the small cluster.
- **client-go ≥ 1.35 sends the initial sync over the watch stream by default**
  (WatchListClient), and that stream is uncompressed. lhtx turns this off per
  informer. The TUI's startup traffic dropped from 309 KiB to 59 KiB.

Strategy:

- **Volumes: `--refresh` (env `LHTX_REFRESH`)**
  - `auto` (default): live watch below 150 volumes, otherwise poll every 30 s.
    The count comes from one `LIST limit=1` (`remainingItemCount`).
  - `watch`: always live. `30s`, `2m`, …: always poll with that interval.
  - Poll mode re-lists with gzip (large cluster: ~0.85 MiB/min instead of
    10.3 MiB/min) but still reacts immediately to what matters: replicas stay
    watched, and a replica change triggers a GET of just that volume. The volume
    open in the detail view gets its own watch (name field selector).
  - The header shows `live` or `⟳ 30s · 12s ago`; `ctrl+r` reloads at once.
- Watched all the time: replicas, nodes.
- Engines are only watched per volume (label selector), and only while the
  detail view of that volume is open or while one of its replicas looks like
  it is rebuilding (`running` without `healthyAt`; the replica watch is free).
  At most 10 engines are watched automatically. If more volumes rebuild at
  once (e.g. after a node failure), the header says how many rebuilds show no
  progress.
- Backups (one CR per backup over the whole retention: 696 backups = 2.4 MB here)
  are watched **from now on**: a LIST with `limit=1` yields the resourceVersion,
  and the watch starting there sends only changes, without an initial list.
  Running backups update their progress constantly, so they show up within
  seconds, even if they started before lhtx.
- Snapshots, backups, events and recurring jobs are loaded on demand when you
  open a panel (label or field selector), not watched. Events go through
  core/v1 using protobuf.
- Redraws happen at most ~5 times per second, however busy the cluster is.
- Result: small cluster, startup ~50–60 KiB, then ~60 KiB/min idle (live).
  Large cluster, startup ~1.2 MiB, then roughly 1 MiB/min idle (poll mode,
  estimated from the measurement). For comparison, the web UI uses up to ~8 MB/s.
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
lhtx measure --diff     # also show which fields change per update
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
   · ✔ snapshot tree · open: backup-volume view in the TUI
   · ✔ config file with settings per context, traffic budget · open: editing
   the settings in the TUI, themes, auto-switch to poll by measured event rate
3. **Actions** ✔ snapshot/backup create, snapshot/replica delete, replica count,
   scheduling/eviction for nodes and disks, settings, engine upgrade, expansion,
   attach/detach, recurring job assignment – with preview, confirmation, client
   and server dry run · open: restore from backup with an automatic switch of
   the PVC to the restored volume (scale down, swap PV/PVC, scale up, keep the
   old volume for rollback; needs a plan for GitOps-managed workloads)
4. **Health** ✔ `lhtx doctor` / Health tab, diagnostics bundle, backup age,
   drain check · open: rebuild/snapshot deadlock workaround as a guided action
   (after collecting evidence with `lhtx diag`)
5. ✔ Multi-cluster overview (`lhtx overview`) · open: overview in the TUI, more
   traffic statistics

## License

MIT, see [LICENSE](LICENSE).
