# Development

[← README](../README.md)

```
nix develop                                # go, gopls, kubectl, pv
go build -o lhtx ./cmd/lhtx && go test ./...
```

After changing `go.mod`, update `vendorHash` in `flake.nix` (set it to
`pkgs.lib.fakeHash`, run `nix build`, copy the `got:` hash).

## Releases

Commits on `main` follow [Conventional Commits](https://www.conventionalcommits.org/)
(`feat:`, `fix:`, `docs:`, …). release-please keeps a release PR open with the
changelog and the `VERSION` bump; merging it creates the tag `vX.Y.Z` and the
GitHub release, and goreleaser attaches the binaries and `checksums.txt`,
pushes the agent image `ghcr.io/nxckdx/lhtx:<version>` (linux/amd64, arm64)
and the workflow pushes the Helm chart `oci://ghcr.io/nxckdx/charts/lhtx-agent`.
CI runs gofmt, vet, tests, `helm lint` and the Nix build on every push and pull
request.

Pushing a new branch opens a pull request to `main` by itself (workflow
`auto-pr`, which also runs the CI for it). Merge it with a merge commit or
"rebase and merge", not squash: release-please builds the changelog from the
individual commits.

## Local test cluster (lhtx-lab)

Write actions, freezes and the agent can be tried on a throwaway cluster:
three NixOS VMs (qemu/KVM, 4 GiB each) with k3s, Longhorn v1.12.1, an NFS
backup target, test apps with hooks, encrypted StorageClasses, External
Secrets and CloudNativePG – plus an optional second one-node cluster for
migrations (`lhtx-lab start2`), all defined in `nix/lab/`.

```
nix run .#lab -- start      # or `lhtx-lab start` in `nix develop`; first start pulls images
lhtx-lab lhtx               # this checkout's lhtx against the lab (never your real kubeconfig)
lhtx-lab agent              # install the agent with the image built from this checkout
lhtx-lab status | ssh 1 | log 1 | stop | destroy
```

State (disks, kubeconfig, ssh key) lives in `~/.local/state/lhtx-lab`
(`LHTX_LAB_DIR` to move it); `destroy` deletes the disks.

## Layout

```
cmd/lhtx            entry point (also used as kubectl-lhtx)
internal/kube       kubeconfig/contexts, TCP-level traffic counter
internal/longhorn   slim CRD structs, one-shot client, informer store, joins
internal/cli        cobra commands
internal/tui        Bubble Tea UI
internal/demo       simulated cluster for --demo (fake dynamic client)
internal/switchover freeze (admission policy) and restore + switch
internal/agent      optional in-cluster agent: Freeze resources, controller, client
deploy/             agent manifests (embedded: lhtx agent manifests)
charts/lhtx-agent   Helm chart of the agent
docs/*.gif          README GIFs, recorded from --demo (docs/tapes/record.sh)
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

## Roadmap

1. **MVP (read-only)** ✔ volume list, volume detail with replicas (node + disk),
   rebuild progress with speed/ETA, nodes/disks (node → disk → replicas, SMART),
   events, `lhtx rebuilds -w`, context switching
2. **Read more data** ✔ snapshots, backups, settings, recurring jobs, backup targets
   · ✔ snapshot tree, backup volumes (tab 6, `lhtx backupvolumes`) incl.
   volumes that were deleted
   · ✔ config file with settings per context, traffic budget · open: editing
   ✔ settings page in the TUI (`,`) · ✔ themes (built-in + own) · ✔ auto switches to poll by measured
   update rate
3. **Actions** ✔ snapshot/backup create, snapshot/replica delete, replica count,
   scheduling/eviction for nodes and disks, settings, engine upgrade, expansion,
   attach/detach, recurring job assignment, recurring jobs and backup targets
   (create/edit/delete), restore into a new volume with PV/PVC, clone of a
   volume or snapshot – with preview,
   confirmation, client and server dry run, restore + switch of the PVC, freeze
   for maintenance (admission policy, works with any operator/GitOps) · ✔
   optional in-cluster agent that carries freezes out (Helm chart) · open:
   CloudNativePG-aware restore
4. **Health** ✔ `lhtx doctor` / Health tab, diagnostics bundle, backup age,
   drain check · open: rebuild/snapshot deadlock workaround as a guided action
   (after collecting evidence with `lhtx diag`)
5. ✔ Multi-cluster overview (`lhtx overview`, `c` → `o` in the TUI) · open:
   more traffic statistics
6. **Agent** ✔ freezes and switches in the cluster · ✔ volume digests (~24×
   less volume traffic) · next: history of rebuilds and backups
