# Changelog

## [0.4.0](https://github.com/nxckdx/longhorntx/compare/v0.3.0...v0.4.0) (2026-11-23)


### Features

* auto refresh switches to polling when volumes change often ([fa6be30](https://github.com/nxckdx/longhorntx/commit/fa6be307cda30237decb065345c400ac8e282ba9))
* backup volumes view (TUI tab 6, lhtx backupvolumes) ([079ed98](https://github.com/nxckdx/longhorntx/commit/079ed988e36f7ec2d36e02943f3bc6b673d4d791))
* config file with settings per kubeconfig context and traffic budget ([fefc888](https://github.com/nxckdx/longhorntx/commit/fefc888a9b54797a7047fcdabb92c09cc4ff485f))
* mark risky orphans and skip them in bulk delete ([b3d61bb](https://github.com/nxckdx/longhorntx/commit/b3d61bb48631febb0ba7e45942eb1b5e9151352a))
* **nix:** optional Home Manager module for the config file ([f781a03](https://github.com/nxckdx/longhorntx/commit/f781a037133d03329cd01d97b4e717b474ae7fa8))
* **overview:** clusters without Longhorn are not errors ([fdc866d](https://github.com/nxckdx/longhorntx/commit/fdc866d9c876927d85ed2fb684157437a5a69328))
* replica matrix (lhtx matrix, nodes view → m) ([3a5c774](https://github.com/nxckdx/longhorntx/commit/3a5c7740fd91e1cea983ecb0e9cec18d356fa489))
* show snapshots as a tree (chain to volume-head, revert branches) ([a672740](https://github.com/nxckdx/longhorntx/commit/a67274039ed8af8ae180e50e4e125ce64ab53af3))
* **tui:** cluster overview in the context picker ([0a42a21](https://github.com/nxckdx/longhorntx/commit/0a42a212f797b66ca4ce145b4475ab65b60deeb7))
* **tui:** theme picker with live preview instead of cycling with T ([b773395](https://github.com/nxckdx/longhorntx/commit/b77339544a041c0cbc1f1e3cdfbfaac817311dea))
* **tui:** themes, built-in and own ([be91ea7](https://github.com/nxckdx/longhorntx/commit/be91ea76673f6958e4437044dd7922a3ae77b9f0))

## [0.3.0](https://github.com/nxckdx/longhorntx/compare/v0.2.0...v0.3.0) (2026-11-22)


### Features

* --demo mode with a simulated in-memory cluster ([7da9b60](https://github.com/nxckdx/longhorntx/commit/7da9b6063a6c0a0b7420b8f228ee32deed4025ab))
* assign recurring jobs to volumes (lhtx jobs add/remove, TUI space in J panel) ([95c5c38](https://github.com/nxckdx/longhorntx/commit/95c5c386ea298b344d63499b0b5e7a58a99a2976))
* attach/detach volumes (CLI attach/detach, TUI A) ([c0758ed](https://github.com/nxckdx/longhorntx/commit/c0758ed8900178c78ad1a5bfc4950122201f8155))
* completion for `kubectl lhtx` via kubectl_complete-lhtx ([114669a](https://github.com/nxckdx/longhorntx/commit/114669a82adbf47deaca69d3344af131ce032716))
* expand volumes (CLI lhtx expand, TUI E) ([881f686](https://github.com/nxckdx/longhorntx/commit/881f68635372212d831b8d2396685e64840607bd))
* lhtx overview – doctor summary for all kubeconfig contexts ([dd92b5b](https://github.com/nxckdx/longhorntx/commit/dd92b5b75922e14514e91650605deb649a9834b3))
* **nix:** lhtx-bin package with the prebuilt release binary ([a5e7beb](https://github.com/nxckdx/longhorntx/commit/a5e7bebaf65a0ed30f8cffa9729f54386ead9266))


### Bug Fixes

* **nix:** kubectl_complete-lhtx in lhtx-bin only from 0.3.0 on ([2d276bc](https://github.com/nxckdx/longhorntx/commit/2d276bc0c3b512d96b0459958a1146bcc5075c7d))

## [0.2.0](https://github.com/nxckdx/longhorntx/compare/v0.1.0...v0.2.0) (2026-11-19)


### Features

* lhtx version, lhtx update [--check], update hint in the TUI header ([d2a5945](https://github.com/nxckdx/longhorntx/commit/d2a5945289669f175b1aeece50fd37c86562663c))
* **update:** release check and self-update from GitHub releases ([4afb7db](https://github.com/nxckdx/longhorntx/commit/4afb7db3c4bdbb8de73fc9197610501b77cf7ff3))
