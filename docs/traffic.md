# Traffic

[← README](../README.md)

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
    The count comes from one `LIST limit=1` (`remainingItemCount`). The live
    watch is measured: if volumes change more than 120 times a minute (the
    traffic follows the write load, not the volume count – each change is a
    new `actualSize`), lhtx switches to polling for the rest of the session
    and says so in the header (`autoPollRate` in the config file, 0 = never).
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

## Startup

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

## Measuring a cluster

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
