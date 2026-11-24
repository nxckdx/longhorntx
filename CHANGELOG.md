# Changelog

## [0.5.0](https://github.com/nxckdx/longhorntx/compare/v0.4.0...v0.5.0) (2026-11-24)


### Features

* clone a volume or snapshot into a new volume (lhtx clone, TUI C) ([8bb5b3e](https://github.com/nxckdx/longhorntx/commit/8bb5b3e178e75820210b23682813eed0b992386c))
* create, edit and delete recurring jobs (lhtx jobs create|edit|delete|next) ([e9c6c8b](https://github.com/nxckdx/longhorntx/commit/e9c6c8bb9239a1316451a24e15c7e4a483c70d75))
* doctor/health report suspended Flux objects and unfinished switches; switch --status ([7a4e6dd](https://github.com/nxckdx/longhorntx/commit/7a4e6ddd86c9b64b54f86ba0fa7a722265d1e08b))
* freeze PVCs with an admission policy instead of scaling (lhtx freeze/unfreeze); switch uses it ([c72bb8a](https://github.com/nxckdx/longhorntx/commit/c72bb8a6d179b9c1f5550ac7f229844036bc814f))
* **freeze:** stop apps cleanly – ordered stop, hooks, CloudNativePG hibernation ([9abf116](https://github.com/nxckdx/longhorntx/commit/9abf1167fce0b0a441a757afd77f52ac8b1838ff))
* manage backup targets (lhtx targets create|edit|delete|sync) ([273b2a8](https://github.com/nxckdx/longhorntx/commit/273b2a81e35f6664bf5eb3c40ba8185d80301ea0))
* restore a backup and switch the PVC to it, Flux included (lhtx restore --switch, lhtx switch) ([0bab11e](https://github.com/nxckdx/longhorntx/commit/0bab11eaa23198dd43a71e9e4bb8d0455ddff705))
* restore a backup into a new volume with PV/PVC (lhtx restore, TUI r) ([f981bff](https://github.com/nxckdx/longhorntx/commit/f981bffcbd66ed5ab961c48bc0af16db5f10b1d4))
* **tui:** back up all volumes with late backups from the health tab (B) ([8f1fd0f](https://github.com/nxckdx/longhorntx/commit/8f1fd0f2bbc459baae8bae9a2142c48367b8ee5c))
* **tui:** backup targets page with create/edit/delete/sync ([520ff25](https://github.com/nxckdx/longhorntx/commit/520ff2534a6c1f0df79cc0d4fbe6a2734a860700))
* **tui:** freeze and unfreeze with Z; header shows active freezes ([f081aec](https://github.com/nxckdx/longhorntx/commit/f081aec729fe4274e460b073afe3f99af961e28d))
* **tui:** lhtx settings page (,) that edits config.yaml ([c005a2f](https://github.com/nxckdx/longhorntx/commit/c005a2f2d86e0d9220c38d389b9d1c3651d2a247))
* **tui:** recurring jobs page with create/edit/delete forms ([8d94321](https://github.com/nxckdx/longhorntx/commit/8d94321417ccdbe0929d38be9df70cfd3c1e7ca4))
* **tui:** restore and switch from the backups tab, with live log and cancel ([d81b28b](https://github.com/nxckdx/longhorntx/commit/d81b28b98ba4bffb6edc0d7dcbfcc0fccfba4a25))
* **tui:** w switches a PVC to the volume in the details (switch back) ([3fc74b9](https://github.com/nxckdx/longhorntx/commit/3fc74b9e23395c8423f95cb8d795f6947c7c9fc7))
* warn when a change targets a GitOps- or Helm-managed object ([82b39ff](https://github.com/nxckdx/longhorntx/commit/82b39ff1ee3825b77d77dfb12726c4c5325cb651))

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
