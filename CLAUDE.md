# CLAUDE.md

lhtx: low-traffic TUI (k9s style) and CLI for Longhorn, replacing the Longhorn
web UI on slow/metered links. Repo `github.com/nxckdx/longhorntx`, binary `lhtx`,
also usable as kubectl plugin (`kubectl-lhtx`). User docs: README.md (short) and docs/*.md.

## Principles (do not break)

- **Kubernetes API only** (kubeconfig, like kubectl). Never the longhorn-manager
  REST API or the UI WebSockets. Data comes from `*.longhorn.io` CRDs (v1beta2).
- **Low traffic is a core goal.** Every new request/watch must be justified.
  Prefer label/field selectors, on-demand loads, watches over polling. Measure
  with `lhtx measure` or `-v`; the TUI header shows bytes on the wire.
- **Read-only by default.** Writes only with `--rw` (CLI) or `W` (TUI), always
  with preview + confirmation; support `--dry-run[=client|server]`.
- Supported Longhorn ≥ 1.6 (`longhorn.MinVersion`); verified against v1.12.1.
  **v1 data engine only** – the user has no v2/SPDK setup and does not plan one.
- License: MIT.
- Do not reference specific clusters, customers or the homelab in code/docs.

## Layout

```
cmd/lhtx            entry point
internal/kube       kubeconfig/contexts; rest.Config with TCP-level byte counter
                    (LHTX_THROTTLE_KBPS simulates slow links)
internal/longhorn   slim CRD structs (no longhorn-manager import), Client (one-shot),
                    Store (informers), joins (view.go, nodes.go, activity.go),
                    write actions (actions.go), health/drain/backup-age checks
                    (doctor.go), diagnostics bundle (diag.go),
                    volume poll mode (volumes_poll.go), recurring jobs / backup
                    targets writes (jobs_write.go, targets_write.go), restore and
                    clone into new volumes (restore.go, clone.go), GitOps owner
                    detection for warnings (gitops.go), secret names via the
                    metadata API only (secrets.go), stuck mounts / CSI / kubelet via the
                    node proxy (mounts.go; agent/mounts.go records them)
internal/fuzzy      fzf-style matching (TUI filter, CLI volume fallback)
internal/demo       --demo: fake dynamic client with made-up data and a
                    simulated rebuild loop (doc GIFs: docs/*.tape + docs/tapes/,
                    nix shell nixpkgs#vhs -c docs/tapes/record.sh)
internal/config     ~/.config/lhtx/config.yaml: defaults + per-context overrides
                    (exact name or glob); flags/env beat it (cli/root.go loadConfig,
                    tui applySettings on every connect; traffic budget tui/settings.go)
internal/switchover freeze (ValidatingAdmissionPolicy denies pods mounting the PVCs and
                    PVC create/delete except our user; delete pods; unfreeze nudges
                    owners) and switch (restore → freeze → swap PV/PVC → unfreeze;
                    rollback until the old PVC is deleted; journal). No per-operator
                    or per-GitOps logic – the desired state is never changed.
internal/agent      optional in-cluster agent (`lhtx agent`, image ghcr.io/nxckdx/lhtx):
                    Freeze CRD lhtx.io/v1alpha1 (cluster-scoped), controller reusing
                    switchover (named, resumable freezes; status log; finalizer =
                    unfreeze; autoUnfreezeAfter; leader election lease lhtx-agent, by
                    default in the Longhorn namespace, labelled = heartbeat). Client:
                    Detect (GET of the lease in the Longhorn ns, else LIST by label), Request, Follow (watch of one object), Unfreeze. lhtx must
                    keep working without it. Requests only via CRs (the health port
                    :8081 /healthz /readyz is read-only). switch.go: namespaced Switch
                    CRs (restore + switch in the cluster; requester vouched for by the
                    VAP lhtx-switch-requester, SubjectAccessReview, own-data rule;
                    resumes via switchover.Continue). digest.go: VolumeDigests; history.go:
                    VolumeHistory (per volume + lhtx-cluster); metrics.go: /metrics; logs.go:
                    LogQuery (diag logs filtered in the cluster); capacity.go: disk
                    samples → VolumeHistory lhtx-capacity (longhorn/capacity.go: Forecast);
                    notify.go: webhook notifications (ntfy/slack/discord/json, batched);
                    probe.go: restore probe (--rw; latest backup → test volume, round robin). Read-only unless
                    --rw (chart rw: true; RBAC split rbac.yaml / rbac-rw.yaml; lease
                    label lhtx.io/rw, lhtx hands work only to agent.DetectRW).
deploy/             agent manifests, embedded (lhtx agent manifests); charts/lhtx-agent
                    is the Helm chart – its templates/crd.yaml must equal deploy/crd.yaml
                    (test). Demo: LHTX_DEMO_AGENT=1 runs the agent in-process.
internal/overview   doctor summary per context (lhtx overview, TUI context picker o)
internal/update     release check (daily, cached) and self-update from GitHub releases
internal/cli        cobra commands (read: volumes.go, nodes.go, data.go, activity.go,
                    measure.go, doctor.go incl. drain-check, diag.go;
                    write: write.go; complete.go)
internal/tui        Bubble Tea v2 (charm.land/*): tui.go model/update/keys,
                    pages.go list pages, render.go header/footer/detail,
                    panels.go detail panels, actions.go write mode/dialogs,
                    form.go multi-field form dialog, backups.go/jobs.go/targets.go/
                    restore.go backups tab pages, theme.go themes + picker,
                    loading.go async startup, health.go tab 5
```

## Traffic design (why things are the way they are)

- Measured on a large cluster (878 volumes): volume watch ~10 MiB/min, engines
  ~12 MiB/min (only `status.actualSize` / volume-head size change; full object
  per event). Replicas, snapshots, settings, backups are silent.
- Replicas, nodes: permanent informers. Volumes: `--refresh` auto (watch below
  150 volumes, else gzip re-list every 30 s), watch, or interval
  (volumes_poll.go). In poll mode a replica change triggers a GET of its
  volume; the volume in the detail view has its own name-selected watch.
- With the lhtx agent: `--refresh agent` (auto picks it) watches the agent's
  VolumeDigests (agent/digest.go, longhorn/digest.go: modeled fields, gzipped,
  rewritten only on notable changes – size by max(64 MiB, 1 %)); lab: ~24× less
  than the volume watch under write load. Patches tolerate the digests' older
  resourceVersion (Apply retries a conflict if the changed fields still match).
- Engines are chatty → watched per volume only (detail view open, or a replica
  looks like it is rebuilding: running without `healthyAt`), max 10 automatic.
- Informers opt out of WatchList (`plainListWatch`): watch streams are not
  gzip-compressed, LISTs are (apiserver compresses only responses > 128 KB).
- `Store.Start` is non-blocking; volumes+nodes load first, replicas after
  volumes, the backup watch last. TUI pages render as soon as their data is in.
- Backups (huge collection) are watched "from now": LIST `limit=1` for the
  resourceVersion, then watch without initial list.
- Snapshots, backups, events, recurring jobs, settings: loaded on demand.
  Events via core/v1 with protobuf, one field-selected LIST per object.
- `kubectl` hides managedFields; they are on the wire anyway.

## CRD gotchas (verified)

- Engine `rebuildStatus`/`purgeStatus`/`restoreStatus`/`cloneStatus` are keyed
  by replica **address** `tcp://ip:port`, not name → map via
  `status.currentReplicaAddressMap` (fallback `replica.status.storageIP:port`).
- Replica disk: `spec.diskID` = `node.status.diskStatus[*].diskUUID`; show the
  disk **path**, not the disk name.
- SMART wear: use the normalized `value` (remaining life), not `rawValue`.
- Byte sizes are sometimes JSON strings, sometimes numbers → `Quantity`.
- Writes mirror longhorn-manager: Snapshot CR (`spec.volume`,
  `spec.createSnapshot`); Backup CR (`spec.snapshotName`, labels `backup-volume`,
  `backup-target`, name `backup-<16 hex>`); eviction requires scheduling off
  (set both in one patch); last available replica must not be deleted.
  Patches include `metadata.resourceVersion`; deletes use a UID precondition.
  Longhorn webhooks have `sideEffects: None`, so server dry run works.

## Working on it

- Dev shell: `nix develop` (go, gopls, kubectl, pv). Build `go build -o lhtx ./cmd/lhtx`,
  test `go test ./...`, also `go vet ./...` and `gofmt`.
- After changing `go.mod`: update `vendorHash` in `flake.nix` (set
  `pkgs.lib.fakeHash`, `nix build`, copy the `got:` hash). Flakes only see
  git-tracked files → `git add` new files before `nix build`.
- Before using a Charm/client-go API, check its signature in `$(go env GOMODCACHE)`
  (Bubble Tea **v2**: `View() tea.View`, `tea.KeyPressMsg`, `charm.land/...` paths).
- TUI testing: run in tmux (`tmux new-session -d -x 160 -y 40 ./lhtx`,
  `send-keys`, `capture-pane -p`). Send `Escape` and the next key separately
  (otherwise read as Alt+key). Render tests in `internal/tui/render_test.go`.
- **Local lab for real tests:** `nix run .#lab -- start` (nix/lab: 3 NixOS VMs, k3s,
  Longhorn, NFS backup target, test apps with hooks). Write actions may be run
  freely against the lab (`lhtx-lab lhtx …`, kubeconfig in ~/.local/state/lhtx-lab) –
  it found bugs the demo could not (ReplicaSet back-off, Longhorn name limit).
- **Clusters are read-only for Claude.** Only read via kubectl/lhtx. Never apply
  a write action, never `--rw` without `--dry-run`, and no `--dry-run=server`
  unless the user explicitly allows it.
- Commit in small logical steps, each building on its own; do not push unless asked.
- **Commit messages: Conventional Commits** (`feat:`, `fix:`, `docs:`, `ci:`,
  `refactor:`, `test:`, `chore:`), optionally with scope (`feat(tui): …`).
  release-please builds releases and the changelog from them; `VERSION` and
  `.release-please-manifest.json` are bumped by the release PR, never by hand.
- Release binaries: goreleaser (`.goreleaser.yaml`); archive names
  `lhtx_<ver>_<os>_<arch>.tar.gz|zip` are relied on by `lhtx update` and the
  `lhtx-bin` flake package. `nix/release.json` (version + SHA256 per system)
  is committed by the release workflow after goreleaser
  (`nix/update-release.sh`); do not edit by hand.
- Keep the docs in sync with features: README.md stays short (pitch, install,
  feature list with links); details go into the matching docs/*.md (usage.md:
  commands + TUI keys, write-actions.md, freeze.md, restore-switch.md,
  encryption.md, agent.md, configuration.md, traffic.md, development.md:
  lab, releases, layout, data model, roadmap).
