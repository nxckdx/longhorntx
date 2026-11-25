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
