# lhtx

Low-traffic terminal UI (k9s style) and CLI for [Longhorn](https://longhorn.io),
as a replacement for the Longhorn web UI on slow or metered links.

The Longhorn UI refreshes over WebSockets once per second and can easily use
several MB/s (longhorn/longhorn#2372, #4211, #11134). lhtx talks **only to the
Kubernetes API** (your kubeconfig, like kubectl), reads the `*.longhorn.io`
CRDs and uses watches instead of polling.

![lhtx demo: volume list sorted by problems, a replica rebuild live in the volume detail, snapshots](docs/demo.gif)

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

## Features

- **Views**: volumes (problems first), volume details with replicas per disk
  and live rebuild progress, nodes → disks → replicas with SMART, snapshot
  tree, backups and backup volumes (also of deleted volumes), recurring jobs,
  backup targets, settings, orphans, replica matrix, events –
  [usage](docs/usage.md)
- **Health**: `lhtx doctor` / Health tab, `lhtx overview` across all
  clusters, drain check before node work, stuck mounts on "healthy" nodes
  (hung kubelet / CSI plugin), RWX share managers and hung NFS servers, diagnostics bundle –
  [usage](docs/usage.md#health-check)
- **Changes** with preview, confirmation and server dry run: snapshots,
  backups, replicas, expansion, attach/detach, scheduling and eviction,
  engine upgrade, settings, recurring jobs, backup targets, restore and clone
  into new volumes – [write actions](docs/write-actions.md)
- **Freeze** for Longhorn upgrades and maintenance: stop everything that uses
  the volumes through an admission policy – no scaling, works with any
  operator and GitOps, ordered shutdown, hooks, CloudNativePG hibernation –
  [freeze](docs/freeze.md)
- **Restore and switch**: put a backup under a running app's PVC in one
  confirmed run, with rollback and the way back; **shrink** a volume (new
  smaller volume, copy, swap) – [restore and switch](docs/restore-switch.md)
- **Encrypted volumes**: the key goes along with restores, clones, switches
  and migrations, whatever manages the Secret – [encryption](docs/encryption.md)
- **Optional in-cluster agent** (Helm chart) that carries freezes and
  switches out, so they do not depend on your laptop, lets namespace users
  switch their own PVCs safely, cuts the volume traffic on busy clusters
  about 24× with volume digests, keeps a history of rebuilds, failures and
  backups, filters diagnostic logs in the cluster, and has Prometheus
  metrics with a Grafana dashboard –
  [agent](docs/agent.md)
- **Low traffic** by design: watches where they are cheap, polling where
  they are not, a traffic budget per context – [traffic](docs/traffic.md)
- Settings per kubeconfig context, themes – [configuration](docs/configuration.md)

## Quick start

```
lhtx                                 # TUI for the current kubeconfig context
lhtx volumes                         # CLI, e.g. -o wide / -o json
lhtx doctor                          # what is wrong right now (exit code like Nagios)
lhtx --rw …  /  W in the TUI         # allow changes (always with a preview)
```

All commands and TUI keys: [docs/usage.md](docs/usage.md).

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

**Nix:**

```
nix run github:nxckdx/longhorntx           # or: nix build → result/bin/{lhtx,kubectl-lhtx}
nix run github:nxckdx/longhorntx -- --context prod volumes   # flags after --
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
- Optional, only if you want the config file managed by Nix: a Home Manager
  module (`inputs.lhtx.homeManagerModules.default`, then
  `programs.lhtx = { enable = true; settings = { … }; }`, same keys as
  `config.yaml`). Without it lhtx uses its built-in defaults.

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

## Documentation

- [Usage](docs/usage.md) – commands, TUI keys, health check, drain check, diagnostics
- [Configuration](docs/configuration.md) – config file per context, themes, traffic budget
- [Write actions](docs/write-actions.md) – write mode, dry runs, every change and how it is made
- [Freeze](docs/freeze.md) – freeze for maintenance, hooks, CloudNativePG
- [Restore and switch](docs/restore-switch.md) – a backup under a running app's PVC, and back
- [Encrypted volumes](docs/encryption.md) – keys with restore, clone, switch and migration
- [Agent](docs/agent.md) – the optional in-cluster agent, Helm chart, RBAC
- [Traffic](docs/traffic.md) – measurements and how lhtx keeps the traffic low
- [Development](docs/development.md) – build, test lab, releases, layout, data model, roadmap

## License

MIT, see [LICENSE](LICENSE).
