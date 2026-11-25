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
lhtx doctor [--backup-max-age 36h]   # health check, exit code 0/1/2/3 (ok/warn/crit/error)
lhtx overview [--contexts a,b]       # doctor summary for all kubeconfig contexts, one line each (+ agent)
lhtx drain-check NODE                # what happens if NODE goes away (reboot/drain)
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
| `s` `b` `e` `J` `H`, `tab` | detail panels: snapshots, backups, events, recurring jobs, history (needs the lhtx agent) |
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
