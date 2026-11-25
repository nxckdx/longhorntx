# Configuration

[← README](../README.md)

`~/.config/lhtx/config.yaml` (`$XDG_CONFIG_HOME`, or the path in
`LHTX_CONFIG`) holds defaults and overrides per kubeconfig context, so for
example a small homelab runs live in write mode and a large customer cluster
polls, has a red badge and a traffic budget. `lhtx config init` writes a
commented example; `lhtx config [--context NAME]` shows the effective values
and where each one comes from; in the TUI `,` shows the same and can change
values (the file keeps your comments; a file managed by Home Manager is
read-only and says so).

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
`autoPollRate`, `readOnly`, `color`, `theme`, `trafficBudget`, `overview`, `namespace`. Unknown keys
and bad values are errors, so typos do not go unnoticed. `readOnly: false`
only skips `--rw`/`W`: previews and confirmations stay. Switching the context
in the TUI applies that context's settings (write mode turns off unless the
new context allows it or `--rw` was given).

**Themes**: `default`, `catppuccin`, `dracula`, `nord`, `gruvbox`, `light`
(for light terminals) – `theme:` in the config (globally or per context),
`--theme`, or `T` in the TUI (a picker with live preview). `lhtx config themes`
shows them with colour swatches. Own themes or tweaks of the built-in ones
start from a base and override single colours:

```yaml
defaults:
  theme: mine
themes:
  mine:
    base: nord
    accent: "#ff79c6"    # #rrggbb, #rgb or 0-255
    warn: 214
  default:               # tweak a built-in: same name, no base needed
    ok: "#00ff87"
```

Colours: `dim faint ok warn error info accent headerBg selectedBg barFg tabBg
tabFg badgeFg prodBg border`, `palette` (context badge colours).

**Traffic budget** (TUI): at 80 % of the budget the header shows the usage,
at 100 % all watches stop (`⏸ paused`); on-demand loads still work and
`ctrl+r` resumes with a fresh budget. It counts both directions of the
current connection.
