# lhtx agent (optional)

[← README](../README.md)

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
  The agent itself needs rights like cluster-admin (delete pods anywhere,
  exec for hooks, admission policies, patch owners)
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
