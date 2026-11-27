# lhtx agent (optional)

[← README](../README.md)

![The lhtx agent: a freeze of all volumes carried out in the cluster with a 4h auto-unfreeze, then the history of a volume – a rebuild and how long it took](agent.gif)

## Read-only or read-write

Like lhtx itself, the agent only reads unless told otherwise:

| | read-only (default) | `--rw` (chart: `rw: true`) |
|---|---|---|
| volume digests, history, metrics, diag logs | ✔ | ✔ |
| Freezes and Switches | refused with a reason in their status – lhtx does them itself with your rights | carried out |
| rights | read Longhorn, pods, PVCs, PVs, pod logs; its own `lhtx.io` resources; its lease | also: delete pods anywhere, exec (hooks), admission policies, patch owners (`*`), create/delete PVCs, create PVs, encryption key Secrets, create Longhorn volumes – like cluster-admin |

The write rights are a separate ClusterRole (`lhtx-agent-rw`, chart
`<release>-rw`), only installed with `rw: true` (`lhtx agent manifests
--rw`, kustomize: add `rbac-rw.yaml` and `--rw`). The lease carries the
label `lhtx.io/rw`, so lhtx only hands freezes and switches to an agent that
does them; `lhtx agent status` and `lhtx overview` show the mode. Starting
read-only and switching to read-write later is a safe way to introduce the
agent in a cluster.

## Freezes through the agent

A freeze run from a laptop stops when the laptop
sleeps or the LTE link drops – possibly halfway through. The agent is a small
operator in the cluster (same binary, `lhtx agent`, image
`ghcr.io/nxckdx/lhtx`) that carries freezes out instead. When lhtx finds it
alive (its lease, renewed within a minute), `lhtx
freeze` and `Z` create a `Freeze` resource (`lhtx.io/v1alpha1`) with your own
rights and only follow the agent's progress from its status; Ctrl+C or `x`
stops following, not the freeze. Without an agent everything runs locally as
before (`--local` forces it).

```
lhtx --rw freeze --all --reason "longhorn upgrade" --auto-unfreeze 4h
lhtx agent status                  # is an agent alive, which Freezes exist
lhtx --rw unfreeze --all           # deletes the Freeze; the agent brings the apps back
kubectl get freezes                # the same with kubectl (delete = unfreeze)
```

- it resumes a freeze after its own restart, ends forgotten freezes after
  `--auto-unfreeze` (`spec.autoUnfreezeAfter`), keeps the `Freeze` (finalizer)
  until the apps are back after an unfreeze
- requests go only through the Kubernetes API – the agent has no HTTP
  endpoint. Who may create `Freeze` resources may freeze: bind the ClusterRole
  `lhtx-freeze` (chart: `<release>-freeze`) – no cluster-admin needed then.
  The agent itself needs `--rw` and rights like cluster-admin (delete pods
  anywhere, exec for hooks, admission policies, patch owners)
- one active agent (leader election); a restart while unfreezing goes on
  where it stopped

## Switches through the agent

`lhtx switch` and `lhtx restore --switch` (and the TUI) hand the switch to
the agent as well, as a `Switch` resource (`lhtx.io/v1alpha1`) in the PVC's
namespace – a restore of a large volume can take hours, and the laptop does
not have to stay online. The agent continues after its own restart (before
the old PVC is deleted it rolls back and starts over; after that it binds the
new PVC and unfreezes). `--local` runs it on your machine as before.

```
lhtx --rw switch shop/db-data --from-backup backup-1a2b3c   # restore + switch
lhtx --rw switch shop/db-data --to-volume db-data-clone-1125
kubectl -n shop get switches
```

The agent has far more rights than the person asking, so a Switch is
checked three ways:

- the admission policy `lhtx-switch-requester` (installed with the agent)
  only accepts a Switch whose `spec.requestedBy`/`requestedGroups` are the
  real requester; the spec cannot be changed afterwards. Without the policy
  the agent refuses to work
- the agent asks the API server (SubjectAccessReview) whether that requester
  may delete and create the PVC and delete its pods
- unless the requester may create Longhorn volumes (an admin), only the
  PVC's own data may be used: a volume that belonged to it, a restore or
  clone of one, or a backup of one

So namespace users can switch their own PVCs: bind the ClusterRole
`lhtx-switch` (chart: `<release>-switch`) with a RoleBinding in their
namespace; together with the usual `edit` role that is all they need. lhtx
then skips the local preview (they may not read Longhorn) and the agent plans
it. Restores go into a new volume the agent marks as its own; a volume of the
same name from before is never taken (it may hold changes) – the name gets
`-2`, `-3`, ….

## Less traffic: volume digests

The agent keeps a small `VolumeDigest` per volume – the fields lhtx shows,
rewritten only on notable changes – and lhtx watches those instead of the
volumes (`--refresh agent`; `auto` picks it when an agent keeps them, the
header then says `live via agent`). In the lab that was about 24× less
traffic under write load; details in [traffic.md](traffic.md#volume-digests-with-the-agent).
`lhtx measure` shows the digests next to the volumes. Turn them off with
`lhtx agent --no-digests` (chart: `extraArgs`).

## Health and replicas

The agent answers the kubelet's probes on a read-only port (`:8081`, no
actions): `/readyz` – a standby is ready at once, the leader once its watches
are synced (startup and readiness probe); `/healthz` – fails if the leader's
loop stalls or its lease is overdue (liveness). With `replicaCount` > 1 one
agent is active and the others wait as standby (leader election, takeover
within ~30 s); the chart then adds a PodDisruptionBudget, spreads the pods
over nodes and rolls updates. One agent per node (DaemonSet) would not help:
freezes and switches are cluster-wide and all data comes from the API server.

`lhtx overview` shows in column AGENT whether an agent runs in each cluster.

## History

Longhorn keeps no history – its events expire after an hour. The agent
records, from the moment it runs: robustness changes involving degraded or
faulted, rebuilds started and finished (with the duration), failed replicas,
completed and failed backups, expansions and engine upgrades. The last 50
events per volume and the last 300 notable ones cluster-wide (failures,
degradations, finished rebuilds) are kept as `VolumeHistory` resources
(gzipped, owned by the volume).

```
lhtx history                       # notable events in the cluster
lhtx history --bad --since 168h    # what went wrong this week
lhtx history shop/db-data          # everything about one volume
```

In the TUI: `H` in the volume details. `lhtx agent --no-history` turns it off.

### How much history is kept

By count, not by time: the last **50** events per volume and the last
**300** notable ones cluster-wide (older ones fall out when new ones are
written). A volume's history is deleted with the volume (owner reference).
Configurable: `lhtx agent --history-per-volume N --history-cluster N
--history-max-age 2160h` (chart: `history.perVolume`, `history.cluster`,
`history.maxAge`; `history.enabled: false` = `--no-history`). The age limit
applies when a history is written, so a quiet volume keeps its old events
until something new happens. The histories stay when the agent is removed
(`kubectl -n longhorn-system delete volumehistories --all`).

## Notifications

Without Prometheus, too: the agent sends failures and recoveries to a
webhook – failed backups and replicas, volumes degraded or faulted (and
healthy again), failed or stuck freezes and switches. Events are collected
for a few seconds and sent together, at most one message a minute
(`--notify-every`): a node restart that degrades every volume is one
message, not sixty.

| format | for |
|---|---|
| `ntfy` | ntfy (title, priority and tags as headers) |
| `slack` | Slack, Mattermost, Rocket.Chat incoming webhooks (`{"text": …}`) |
| `discord` | Discord webhooks (`{"content": …}`) |
| `json` | anything else: `{"title", "cluster", "events": [{at, kind, volume, pvc, text, bad}]}` |

```yaml
notify:
  existingSecret: lhtx-notify   # key "url"; or url: … and the chart makes the Secret
  format: ntfy
  cluster: prod
```

Without the chart: env `LHTX_NOTIFY_URL` (the URL often holds a token) and
`--notify-format`, `--notify-cluster`. Volume events come from the history,
so `--no-history` turns them off too.

## Restore probe

"We have backups" is not "we can restore them". With `--restore-probe-interval`
(chart `restoreProbe.enabled`, default every 24h; needs `rw: true`) the agent
takes one volume per run – the one probed longest ago –, restores its latest
backup into a one-replica test volume `lhtx-probe-…`, waits until Longhorn
has read every block back from the backup target, and deletes the test
volume. Encrypted backups are restored into an encrypted test volume; no
key is needed, as nothing is ever attached to a pod.

```
lhtx history --kind restore-probe,restore-probe-failed
WHEN               PVC / VOLUME   KIND            WHAT                                                      TOOK
2026-11-26 21:14   shop/db        restore-probe   backup backup-1a2b… (74 MiB) restored into a test volume   20s
```

A failure (an error, a restore that does not start within 15 minutes, or
one not done within `restoreProbe.timeout`) is a bad history event: `lhtx
history --bad`, `lhtx doctor`, notifications, and the alert
`LonghornRestoreProbeFailed`. `lhtx_agent_restore_probe_last_success_timestamp_seconds`
says when one last worked. Each probe reads a whole backup from the target –
mind traffic and space (`restoreProbe.maxSize`, default 100Gi, skips larger
volumes). Test volumes left behind by an agent restart are removed when it
starts again.

## Stuck mounts

Every minute (`--mount-check-interval`, chart `mountCheck.interval`, `0` =
off) the agent runs the check of [`lhtx mounts`](usage.md#stuck-mounts): one
LIST of pending pods, more only when pods wait for Longhorn volumes. A pod
waiting longer than `mountCheck.after` (5m) goes into the volume's history
as `mount-stuck` (a bad event: notifications, `lhtx history --bad`); when it
no longer waits, `mounted` with the time it waited. The metric
`lhtx_agent_mounts_waiting{node,stage}` drives the alerts
`LonghornMountStuck` and `LonghornAttachStuck`. Read-only either way – the
agent never restarts kubelet or pods.

`mountCheck.kubelet: true` (`--mount-check-kubelet`) also asks kubelet on
nodes with stuck mounts. It grants the agent `get` on `nodes/proxy`, which
reaches every kubelet API – a powerful right, hence off by default.

## Capacity forecast

Longhorn shows how full a disk is, not how fast it fills. The agent samples
every disk every 6 hours (`--capacity-interval`, chart `capacity.interval`,
`0` = off) and keeps ~60 days in the VolumeHistory `lhtx-capacity`. `lhtx
capacity` fits a line through the last 14 days:

```
DISK                          USED      USABLE    USED%  PER DAY    FULL IN
node-1:/var/lib/longhorn      412 GiB   900 GiB   45%    +6.1 GiB   ~80 days
node-2:/var/lib/longhorn      801 GiB   900 GiB   89%    +9.8 GiB   ~10 days
```

"Usable" is the disk's maximum minus what is reserved in Longhorn. `lhtx
doctor` (and the Health tab) warn when a disk will be full within 14 days
and note it within 30. It needs 2 days of samples first.

## Diagnostics: logs filtered in the cluster

`lhtx diag VOLUME` needs the instance-manager and longhorn-manager logs of
the volume's nodes – megabytes, of which only the lines mentioning the
volume are kept. With the agent, lhtx asks for them with a `LogQuery`: the
agent reads the logs inside the cluster, filters them and answers with only
those lines (gzipped; the largest logs keep their newest lines if the answer
gets too big). The requester is vouched for by the admission policy and must
be allowed to read pod logs in the Longhorn namespace; ClusterRole
`lhtx-diag` lets people create LogQueries.

## Metrics and dashboard

The health port also serves Prometheus metrics (`/metrics`, counts only – no
names of volumes, PVCs or people):

| metric | |
|---|---|
| `lhtx_agent_leader` | 1 on the active agent, 0 on a standby |
| `lhtx_agent_freezes{phase}`, `lhtx_agent_frozen_pvcs` | Freeze resources, PVCs frozen by the agent |
| `lhtx_agent_freeze_oldest_seconds` | how long the oldest freeze has kept its apps down |
| `lhtx_agent_volumes{robustness}` | volumes by robustness (needs the digests) |
| `lhtx_agent_switches{phase}` | Switch resources |
| `lhtx_agent_operations_total{kind,result}` | finished freezes, unfreezes, switches … |
| `lhtx_agent_volume_events_total`, `lhtx_agent_digest_writes_total` | volume updates the agent saw vs digests written – the traffic lhtx saves |
| `lhtx_agent_digest_volumes` | volumes with a digest |
| `lhtx_agent_history_events_total{kind}` | history events by kind (rebuilt, replica-failed, backup-failed, …) |
| `lhtx_agent_rebuild_seconds` (sum/count) | durations of finished rebuilds |
| `lhtx_agent_mounts_waiting{node,stage}` | pods waiting for a Longhorn volume longer than `mountCheck.after` |
| `lhtx_agent_build_info{version}` | |

Chart values: `metrics.service` (on; a Service with `prometheus.io/scrape`
annotations, also in the plain manifests), `metrics.serviceMonitor.enabled`
(Prometheus operator), `metrics.dashboard.enabled` (a ConfigMap with a
Grafana dashboard for the sidecar, label `grafana_dashboard: "1"`).

### Alerts

`metrics.prometheusRule.enabled: true` adds a `PrometheusRule` (Prometheus
operator; kube-prometheus-stack needs its `release` label in
`metrics.prometheusRule.labels`):

| alert | when |
|---|---|
| `LhtxAgentDown` | no agent holds the lease for 10 minutes |
| `LonghornVolumeFaulted` | a volume is faulted for 5 minutes (critical) |
| `LonghornVolumeDegraded` | a volume is degraded for `degradedFor` (30m) |
| `LonghornBackupsFailing` | `backupFailures` (2) backups failed in 24 hours |
| `LonghornReplicasFailing` | `replicaFailures` (3) replicas failed in an hour |
| `LonghornMountStuck` | pods on a node wait for attached volumes that are not mounted, for 5 minutes (critical) |
| `LonghornAttachStuck` | pods on a node wait for volumes to attach, for 10 minutes |
| `LhtxFreezeLong` | a freeze has kept apps down for `frozenForSeconds` (6h) |
| `LhtxFreezeOrSwitchStuck` | a Freeze freezing or failed, or a Switch stuck, for 30 minutes |

The descriptions name the lhtx command that shows the details.

How many lhtx TUIs use the agent cannot be seen by the agent – lhtx only
talks to the API server. The dashboard takes it from the API server's own
metrics instead: every lhtx TUI in agent mode holds one namespaced watch on
`volumedigests` (`apiserver_longrunning_requests{resource="volumedigests",
verb="WATCH",scope="namespace"}`, minus the agent's own watch). That needs
the API server's metrics in Prometheus (kube-prometheus-stack scrapes them).

Install with Helm (Flux/Argo CD: an OCI `HelmRepository` / chart source):

```
helm install lhtx-agent oci://ghcr.io/nxckdx/charts/lhtx-agent -n lhtx-system --create-namespace \
  --set longhornNamespace=longhorn-system
```

or plain manifests (CRD, RBAC, Deployment in the Longhorn namespace):
`lhtx agent manifests | kubectl apply -f -` or `kubectl apply -k deploy/`.
The agent may run in any namespace. lhtx looks for its lease first in the
Longhorn namespace (one GET; everyone may read just that lease), then by the
label `app.kubernetes.io/name=lhtx-agent` in all namespaces (one LIST, if you
may list leases). So the lease can live elsewhere too: chart value
`leaseNamespace` (+ `rbac.leaseListForAllUsers=true`), or
`lhtx agent --lease-namespace`. The chart has no Helm hooks – it works the same
with Argo CD and Flux; the CRD is a normal template (updated on upgrade, kept
on uninstall). Try it without a cluster:
`LHTX_DEMO_AGENT=1 lhtx --demo`.
