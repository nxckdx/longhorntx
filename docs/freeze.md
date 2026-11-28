# Freeze for maintenance

[← README](../README.md)

`lhtx freeze`, `lhtx unfreeze`, `Z` in the TUI. Stop everything
that uses Longhorn volumes – for a Longhorn upgrade, node work or a volume
swap – without touching a single Deployment, StatefulSet or operator:

![Freeze all volumes for a Longhorn upgrade, then unfreeze: pods stopped by an admission policy, volumes detach and attach again](freeze.gif)

```
lhtx --rw freeze --all --reason "longhorn upgrade"   # every PVC with a Longhorn volume
lhtx --rw freeze --node worker-3                     # volumes attached on a node
lhtx --rw freeze shop/db-data shop/uploads           # some PVCs
lhtx freeze --status                                 # what is frozen
lhtx --rw unfreeze --all                             # pods start again
```

lhtx creates a ValidatingAdmissionPolicy (Kubernetes ≥ 1.30; checked by the
API server itself, no webhook server): no new pod may mount the PVCs, and
nobody but lhtx's user may create or delete them. A server dry run with a
test pod proves the policy is enforced before anything is deleted. Then the
pods are deleted and lhtx waits until the volumes are detached.

Clustered apps shut down cleanly: each app's pods stop one after the other,
highest ordinal first (like a StatefulSet scales down), each with its normal
grace period; apps run in parallel. Pods of an operator are treated like any
other: deleted, and the operator cannot start them again until the unfreeze.
App knowledge comes from **hooks** – a command run with `sh -c` via
`pods/exec` in a ready pod of the app; a failing pre-freeze hook stops the
freeze before anything is stopped. Keep hooks idempotent: the agent runs the
pre-freeze hooks again if it restarts in the middle of a freeze:

```yaml
metadata:
  annotations:   # on the pod, or its Deployment/StatefulSet/operator resource
    lhtx.io/pre-freeze: >-
      opensearch: curl -s -XPUT localhost:9200/_cluster/settings -H 'Content-Type: application/json'
      -d '{"persistent":{"cluster.routing.allocation.enable":"primaries"}}' && curl -s -XPOST localhost:9200/_flush
    lhtx.io/post-unfreeze: >-
      opensearch: curl -s -XPUT localhost:9200/_cluster/settings -H 'Content-Type: application/json'
      -d '{"persistent":{"cluster.routing.allocation.enable":null}}'
    lhtx.io/hook-timeout: 2m
```

lhtx looks on the pod first (`podAnnotations` in most charts), then up its
owners (ReplicaSet → Deployment, StatefulSet, the operator's resource), then
in the ConfigMap `lhtx-hooks` in the Longhorn namespace – for charts that
cannot annotate anything (with Flux, a HelmRelease `postRenderers` patch can
add the annotations, too):

```yaml
# ConfigMap lhtx-hooks, key hooks.yaml
- namespace: cloud
  selector: {app: nextcloud}        # pod labels
  container: nextcloud
  preFreeze: php occ maintenance:mode --on
  postUnfreeze: php occ maintenance:mode --off
  timeout: 1m
```

The plan lists every app, how it is stopped and which hooks run (and where
they come from). Post-unfreeze hooks run once a pod of the app is ready again.

Because the desired state does not change, it works the same for any
operator and any GitOps setup: no operator "repairs" a scale-down by
deleting its cluster and PVCs (as some do with `replicas: 0`), no Flux or
Argo CD sync reverts anything – they simply cannot start pods. `unfreeze`
removes the policy and nudges the owners of the deleted pods (annotation
`lhtx.io/unfrozen-at`, metadata only) so their controllers start pods at
once instead of after their back-off, then waits until the volumes are
attached again. The freeze lives in the cluster: `lhtx freeze --status`,
`lhtx doctor` and the Health tab show it, `unfreeze` works from any machine.
Needs cluster-admin rights. Longhorn itself can still attach frozen volumes
(recurring jobs on detached volumes, manual attach).

With the [lhtx agent](agent.md) in the cluster the freeze runs there and does
not depend on your laptop.
