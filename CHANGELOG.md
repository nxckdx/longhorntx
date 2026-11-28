# Changelog

## [0.8.0](https://github.com/nxckdx/longhorntx/compare/v0.7.0...v0.8.0) (2026-11-28)


### Features

* **agent:** --notify-url / LHTX_NOTIFY_URL, --notify-format, --notify-cluster, --notify-every ([f6979bd](https://github.com/nxckdx/longhorntx/commit/f6979bd57139ae1ea88781363dd0ec522f77baa8))
* **agent:** metrics for volumes by robustness and the oldest freeze ([c35da69](https://github.com/nxckdx/longhorntx/commit/c35da6912521cfe107c31f57685a7c2115531273))
* **agent:** mount check – stuck mounts into the history, notifications, metric and alerts ([62a88c8](https://github.com/nxckdx/longhorntx/commit/62a88c874351baaff78adcedfb0435dbb9fca6c9))
* **agent:** restore probe – restore the latest backup into a test volume now and then, round robin (--restore-probe-interval, needs --rw) ([2d7487b](https://github.com/nxckdx/longhorntx/commit/2d7487bd487d8bb105792ae49cfd06adec3165b6))
* **agent:** RWX history (share manager failures and restarts) and NFS probe of every running share manager ([5f2e4f3](https://github.com/nxckdx/longhorntx/commit/5f2e4f3456bbe9910faf5b3a3f5ac590cc2d7036))
* **agent:** sample disk capacity every 6 hours (--capacity-interval) ([d655446](https://github.com/nxckdx/longhorntx/commit/d65544647864f818b4f22bef3786b41b98eaff45))
* **agent:** shrink through the agent (Switch spec.shrinkTo) – runs in the cluster, resumes after a restart ([dbd6d77](https://github.com/nxckdx/longhorntx/commit/dbd6d775165a764fbb04f37d87e0124d5d4f29c1))
* **agent:** webhook notifications for failures and recoveries (ntfy, slack, discord, json; batched) ([8840dff](https://github.com/nxckdx/longhorntx/commit/8840dffa4341c428704db4325be4f1f31d813e0b))
* **chart:** capacity.interval; agent may read Longhorn nodes ([15fa11c](https://github.com/nxckdx/longhorntx/commit/15fa11c2d9efac6440c455a174b2e3d4f473b7a5))
* **chart:** notify.* – webhook URL from a Secret ([28fc76a](https://github.com/nxckdx/longhorntx/commit/28fc76a8631ec8a05d1a557ab0f09685bc55f796))
* **chart:** PrometheusRule with alerts (agent down, volumes faulted/degraded, backups/replicas failing, long or stuck freezes) ([5f6fa2f](https://github.com/nxckdx/longhorntx/commit/5f6fa2fd939d2ff9b08d8bd9880d8f42b730133f))
* **chart:** restoreProbe.*, alert LonghornRestoreProbeFailed; rw role may delete (test) volumes ([45f7e15](https://github.com/nxckdx/longhorntx/commit/45f7e1568176ff38a5dc70215ee8c6f08ca43bc2))
* **cli:** lhtx mounts; doctor and Health tab report stuck mounts and CSI problems ([1f62d37](https://github.com/nxckdx/longhorntx/commit/1f62d378c1b0cc617953751e49287668ef818481))
* **copy:** compare xattrs; a pre-copy over NFS leaves them to the final pass ([8156f1b](https://github.com/nxckdx/longhorntx/commit/8156f1bbcbd97e090a187b6ba9bfab73f7761c38))
* **copy:** lhtx copy – copy a tree keeping owners, modes, hard/symlinks, devices, xattrs/ACLs/capabilities and holes, then verify ([47a9edc](https://github.com/nxckdx/longhorntx/commit/47a9edc1f09be44c7146c9890fa7882b670329f3))
* **freeze:** no CloudNativePG special case – operator pods are stopped like any other ([81e4751](https://github.com/nxckdx/longhorntx/commit/81e47515acd213ec3a52484a9cd566e9b3406813))
* lhtx capacity; doctor warns when a disk will be full within 14 days ([fefbe34](https://github.com/nxckdx/longhorntx/commit/fefbe34009d0c08a4260f4f4841b674642061582))
* lhtx placement – why Longhorn does (not) place a volume's replicas ([41ae8a8](https://github.com/nxckdx/longhorntx/commit/41ae8a816c2ba0bee59f2aafbc46e06bee2fba75))
* **longhorn:** disk capacity samples and a trend forecast ([3de37f8](https://github.com/nxckdx/longhorntx/commit/3de37f865bac27b823fcb466c3f4e5c87ac16716))
* **longhorn:** restore probe volume and history kinds; doctor warns about failed restore probes ([36ec003](https://github.com/nxckdx/longhorntx/commit/36ec00324456e486e16fc3a6f23797f083a73e0d))
* **mounts:** find pods stuck mounting Longhorn volumes, per node, and ask kubelet ([2387e73](https://github.com/nxckdx/longhorntx/commit/2387e738d2b8bdffdb98cadf9cb0352ecc85610e))
* **placement:** hints from the reasons; doctor also finds replicas without a node while Longhorn says scheduled ([cf48699](https://github.com/nxckdx/longhorntx/commit/cf486993ecd66805b61af92719f999f5b36f33ac))
* **rwx:** lhtx rwx and doctor check the share chain of RWX volumes; lhtx restart share-manager / csi-plugin ([3fa9660](https://github.com/nxckdx/longhorntx/commit/3fa966071dc1af79d7ab612cc56f3974bca77a55))
* **rwx:** lhtx rwx and doctor show the agent's NFS verdict; docs for RWX checks, NFS probe and restarts ([77a2fcf](https://github.com/nxckdx/longhorntx/commit/77a2fcf0763be9482a976293bcc964ecf240d494))
* **shrink:** copy and verify many files in parallel (lhtx copy --jobs, default 8) ([4b3d1ee](https://github.com/nxckdx/longhorntx/commit/4b3d1eedb8dd5c1dd0aab33ebe37b2b8b13de1ff))
* **shrink:** lhtx shrink – new smaller volume, freeze, copy and verify in a pod with lhtx's image, swap the PVC ([b753e63](https://github.com/nxckdx/longhorntx/commit/b753e635bb215139c1e820e4f53a019ae35cf2d1))
* **shrink:** pre-copy while the app runs – the downtime is only what changed ([33a28cb](https://github.com/nxckdx/longhorntx/commit/33a28cb3422340b54e87070b7a14326d412350c6))
* **shrink:** RWX volumes ([1d3d89f](https://github.com/nxckdx/longhorntx/commit/1d3d89ffa62c0462c2c9ef4515dc25c537022fae))
* **switchover:** AfterFreeze hook (rolls back on error) and a freeze exception for labelled pods of the freezing user ([5be33ed](https://github.com/nxckdx/longhorntx/commit/5be33ed0b80a93503a9fd51fd4f229a6ad5df92f))
* **switchover:** KeepFrozen – an AfterFreeze error that must not roll back ([b907b7c](https://github.com/nxckdx/longhorntx/commit/b907b7c321d5227e7c6a74db20d244f4dae7e025))
* **tui:** placement panel (P) in the volume details ([76466be](https://github.com/nxckdx/longhorntx/commit/76466be5d36aa542d126107583504d05c27f117e))
* **tui:** shrink a volume with K in the volume details (plan, steps, live log, x cancels) ([fcb208a](https://github.com/nxckdx/longhorntx/commit/fcb208a050d15f8d81863cda1589b98ab5b125a4))


### Bug Fixes

* **agent:** a shrink that stopped before anything changed is Failed, not Stuck ([b7a6221](https://github.com/nxckdx/longhorntx/commit/b7a6221ed8d0dde7f876787431369099cd2a4cb5))
* **agent:** a stopping agent hands its switch/shrink over instead of rolling back; deletes only what this run created ([9c11a94](https://github.com/nxckdx/longhorntx/commit/9c11a94fc1148c05bca6221d2661aed242c7c080))
* **agent:** restore probe – encrypted test volume for encrypted backups; fail a restore that never starts ([8fab8a9](https://github.com/nxckdx/longhorntx/commit/8fab8a913625244fe62f8aebeb6002a9a4eb7325))
* **agent:** restore probe waits the full interval after a probe (the history is written a few seconds later) ([690793c](https://github.com/nxckdx/longhorntx/commit/690793c2f67bf8daddb54d79c64932264198dc69))
* **capacity:** show how long samples were taken in minutes, not rounded to hours ([4fbc8e4](https://github.com/nxckdx/longhorntx/commit/4fbc8e4208d7386e994ecc1e37c7d7f1b7cb7c8b))
* **copy:** skip holes (SEEK_DATA), verify content by default, check inodes, tolerate refused security.*/trusted.* xattrs, verify without holding the tree in memory, probe mode ([081fb83](https://github.com/nxckdx/longhorntx/commit/081fb83a410fd699e8f33cf137635d97b8c987d2))
* **copy:** verify reads the copy from the disk, not the page cache (fadvise DONTNEED after sync) ([26c3bc5](https://github.com/nxckdx/longhorntx/commit/26c3bc558e2f1acfe406bb2837c3a22578254136))
* **freeze:** nudge owners only once the API server no longer enforces the removed policy ([0e9ec52](https://github.com/nxckdx/longhorntx/commit/0e9ec523502ec055b1614e322ff788b3d9664d92))
* **jobs:** warn about job labels Longhorn leaves on volumes; next runs as info ([bf5a2e2](https://github.com/nxckdx/longhorntx/commit/bf5a2e21b526ea3d0c94d19b3da7c05992a34b29))
* **mounts:** shorter mount errors, name the not-ready CSI plugin as the likely cause; failed counts are since kubelet start ([bb0337b](https://github.com/nxckdx/longhorntx/commit/bb0337ba578c01254d9d5a4020f7f96af43a6637))
* **shrink:** doctor does not flag volumes kept by a later shrink; clearer message for a deleted copy pod ([53f1c8a](https://github.com/nxckdx/longhorntx/commit/53f1c8acd8c53e7066d7f0fe95ec4f4d2a381dd9))
* **shrink:** fail fast on hopeless pods, require all replicas placed, close journals on abort; docs ([f1a2a39](https://github.com/nxckdx/longhorntx/commit/f1a2a3956fbb23b528dc9a54f1cd37c68e34907f))
* **shrink:** free name for a second shrink on a day, correct rollback message, shrink-specific key warning; docs and tests ([ae983c9](https://github.com/nxckdx/longhorntx/commit/ae983c9bc303545d074513079be0cf1677ef8593))
* **shrink:** pre-flight before the app stops, pinned node, retries, abort for interrupted runs ([6b9e890](https://github.com/nxckdx/longhorntx/commit/6b9e890804ffe422ecfe03590950a444cf99a6b4))
* **targets:** edits and syncs failed with "changed meanwhile" after Longhorn rewrote the target ([47cc875](https://github.com/nxckdx/longhorntx/commit/47cc8756450c4c6468f88817e17e376ffa6e559a))
* **tui:** after a server dry run the confirm dialog stays open – apply with y ([4f428e3](https://github.com/nxckdx/longhorntx/commit/4f428e34bcd37963dd7ef44e5071dab93cc16ff3))


### Performance Improvements

* **shrink:** copy with 16 workers by default ([2bb44d1](https://github.com/nxckdx/longhorntx/commit/2bb44d11503ccfce27c906b358b41fb951c67c1f))

## [0.7.0](https://github.com/nxckdx/longhorntx/compare/v0.6.0...v0.7.0) (2026-11-26)


### ⚠ BREAKING CHANGES

* **agent:** the chart installs the write rights (ClusterRole <release>-rw) and starts the agent with --rw only with rw: true.
* **agent:** the agent carries out freezes and switches only when started with --rw (Helm value rw: true).

### Features

* **agent:** configurable history size and age (--history-per-volume, --history-cluster, --history-max-age) ([7b07020](https://github.com/nxckdx/longhorntx/commit/7b07020375d38819cf876ef6d6a6580c343817fa))
* **agent:** read-only unless started with --rw ([62626ac](https://github.com/nxckdx/longhorntx/commit/62626ac36cebdd86864ae58e72b67fcb00de2722))
* **agent:** write RBAC only with rw (chart rw: true, lhtx agent manifests --rw) ([5eec656](https://github.com/nxckdx/longhorntx/commit/5eec656ac3bf0da83d3ef018727116314c8fd5f9))
* **chart:** history.perVolume, history.cluster, history.maxAge, history.enabled ([c047301](https://github.com/nxckdx/longhorntx/commit/c04730110c3b9dfab67d29686ae764eee763b355))
* hand freezes and switches only to an agent started with --rw; agent status and overview show the mode ([e9c663e](https://github.com/nxckdx/longhorntx/commit/e9c663e9ecca75832ed0203407a2de200b47c500))
* **nix:** lhtx-lab agent ro installs a read-only agent ([d235e92](https://github.com/nxckdx/longhorntx/commit/d235e922a2348d073344558208981c99ab1ce261))


### Bug Fixes

* **agent:** rebuild duration of a replica rebuilt in place counts from its failure ([3c3d77f](https://github.com/nxckdx/longhorntx/commit/3c3d77ff019ad50e9fc1769e56835b875d00782a))
* **chart:** honour fullnameOverride ([94f7368](https://github.com/nxckdx/longhorntx/commit/94f73688a0f43694fcc672c55eba880cb0e4607f))
* **history:** shorten long PVC names in the cluster table (full in -o json) ([f641920](https://github.com/nxckdx/longhorntx/commit/f641920aeaa7197b4eefe594d8fa06c347d54478))

## [0.6.0](https://github.com/nxckdx/longhorntx/compare/v0.5.0...v0.6.0) (2026-11-25)


### Features

* --refresh agent – watch the agent's VolumeDigests instead of the volumes ([ea12229](https://github.com/nxckdx/longhorntx/commit/ea122297a570ff4c6bc23e5ae9410bcbe572e3c9))
* **agent:** health probes; chart: probes, PodDisruptionBudget and spreading for several replicas ([c677094](https://github.com/nxckdx/longhorntx/commit/c677094501e9073940cc060391e1f2678737fce3))
* **agent:** Helm chart (oci://ghcr.io/nxckdx/charts/lhtx-agent) ([0339d32](https://github.com/nxckdx/longhorntx/commit/0339d32f2fa1b33ddb07ebe05e2a40ecb1ce7e62))
* **agent:** history – rebuilds, failures, backups, expansions per volume and cluster-wide ([f5e2542](https://github.com/nxckdx/longhorntx/commit/f5e25422470b12f73b1eb9405e402e1a3accd350))
* **agent:** history metrics by kind and rebuild durations; dashboard panels for them ([6944aff](https://github.com/nxckdx/longhorntx/commit/6944affbc22cf41c3f22234d145245a50dfd6c5e))
* **agent:** in-cluster agent that carries out Freeze resources ([a3a987b](https://github.com/nxckdx/longhorntx/commit/a3a987b7d4f68b967a4c0b233366e9df6f5e3b19))
* **agent:** lease in any namespace, found by label (chart: leaseNamespace) ([b39097f](https://github.com/nxckdx/longhorntx/commit/b39097fc4c84f8842f1bfa514493b9c4aad2df41))
* **agent:** lhtx agent command, manifests (CRD, RBAC, Deployment), agent status ([c970074](https://github.com/nxckdx/longhorntx/commit/c97007431418f51d54e595c705c373efbef47986))
* **agent:** LogQuery – volume logs read and filtered in the cluster for lhtx diag ([5bb531e](https://github.com/nxckdx/longhorntx/commit/5bb531e0b860cb91f187f4b9c23620f8170dbd7a))
* **agent:** LogQuery CRD, requester policy and RBAC for it, ClusterRole lhtx-diag ([ed74b08](https://github.com/nxckdx/longhorntx/commit/ed74b08dce8c9d562cc71a4457b46437bf855971))
* **agent:** manifests and chart for Switch resources (CRD, requester policy, RBAC) ([081deb3](https://github.com/nxckdx/longhorntx/commit/081deb38d5e4223851ecbb53a2b13bad463ef7f3))
* **agent:** Prometheus metrics on the health port (/metrics) ([8fbd9f0](https://github.com/nxckdx/longhorntx/commit/8fbd9f0c7f997c6bc3a9e4ba8f27ef14e2d1b0e3))
* **agent:** Switch resources – restore and switch a PVC in the cluster ([aa5420d](https://github.com/nxckdx/longhorntx/commit/aa5420d54ea83463b0886b25c1fe2ee4201099a8))
* **agent:** VolumeDigests – the fields lhtx shows, rewritten only on notable changes ([5f56d44](https://github.com/nxckdx/longhorntx/commit/5f56d44a66b03abc5d186dd761e7ed71a2e27ed6))
* **agent:** VolumeHistory CRD, RBAC (watch replicas and backups), reader role ([e31817b](https://github.com/nxckdx/longhorntx/commit/e31817bcd0f6527e8b044dd07c48c6ab1cd6905e))
* **chart:** metrics Service, optional ServiceMonitor and Grafana dashboard ([23940ac](https://github.com/nxckdx/longhorntx/commit/23940ac7d3853fa1a4bdaae8930aa40b4d85957a))
* **diag:** logs through the lhtx agent when it offers LogQueries (--local-logs) ([512764b](https://github.com/nxckdx/longhorntx/commit/512764bb704b4bfaab2d06e30981ef1dbbd86589))
* **doctor:** encrypted volumes whose key Secret is missing ([88cad94](https://github.com/nxckdx/longhorntx/commit/88cad94bf2d0b3f07349b9f781944f7f77d39b56))
* **doctor:** kept key copies and keys from another namespace ([999bebb](https://github.com/nxckdx/longhorntx/commit/999bebb1005c093e3c6209831f5f416daf6a97a5))
* **doctor:** note key Secrets maintained by a controller – it must never rotate the key ([595a439](https://github.com/nxckdx/longhorntx/commit/595a439168e6940836e9f2e6be98a25142449f80))
* **doctor:** repeated trouble from the agent's history (backups failing ≥2×, replicas ≥3× in 7 days) ([82776dc](https://github.com/nxckdx/longhorntx/commit/82776dccce230edb9c9d81c7e5e4e02e29491835))
* **freeze:** freeze through the agent without read rights ([f6b11d8](https://github.com/nxckdx/longhorntx/commit/f6b11d8d278d54b022741a6ce83fbf1e767686d6))
* **freeze:** hand freezes to the lhtx agent when one runs (--auto-unfreeze, --local) ([8b89dec](https://github.com/nxckdx/longhorntx/commit/8b89deca065fc76d92d99aa65f9d3567aca9f173))
* **freeze:** named freezes that can be resumed (for the agent) ([ce895cf](https://github.com/nxckdx/longhorntx/commit/ce895cfdee02b1b0567c9f27276f35de72b94282))
* **kube:** in-cluster config for the agent ([d0f276a](https://github.com/nxckdx/longhorntx/commit/d0f276ad64cef1d002e46ccfd46de3be6a361c85))
* lhtx history [VOLUME] – the agent's history (--since, --kind, --bad) ([fc70a78](https://github.com/nxckdx/longhorntx/commit/fc70a78a67dc144f9a423939ea91e0a086049df5))
* **measure:** include the agent's volumedigests when it keeps them ([59c3d11](https://github.com/nxckdx/longhorntx/commit/59c3d11d8a012f77b6029c6f4ec8ad6694f0ce5d))
* **nix:** lab gets CloudNativePG with a two-instance cluster ([bc4cb87](https://github.com/nxckdx/longhorntx/commit/bc4cb87bf03b7fdd9df816f080d81951aca48323))
* **nix:** lab gets External Secrets and a second one-node cluster (start2) for migrations ([28ef09b](https://github.com/nxckdx/longhorntx/commit/28ef09b359e3d49392c02900b4ddddde5d5c922c))
* **nix:** lab has an NFS backup target on lab-1 ([202f7e6](https://github.com/nxckdx/longhorntx/commit/202f7e69950b11da201bb34757caf53df4ab7283))
* **nix:** lhtx-lab – local 3-node k3s + Longhorn cluster in NixOS VMs ([259cbbc](https://github.com/nxckdx/longhorntx/commit/259cbbca379f82446b192b8ff0dbb97fdc79df84))
* **nix:** lhtx-lab agent imports the image into the running nodes (no VM restart) ([3cd015c](https://github.com/nxckdx/longhorntx/commit/3cd015cccb5e76d6dc92616431a603a36ab950cd))
* **overview:** AGENT column – is an lhtx agent alive in the cluster ([aa5d9d7](https://github.com/nxckdx/longhorntx/commit/aa5d9d73e189beb67910ab184db264b3ff33a9d1))
* **restore:** encrypted volumes – restore, clone and switch keep the key ([0e8fd8b](https://github.com/nxckdx/longhorntx/commit/0e8fd8b12eaa6758b7d6ea03b2e86afd27f72a9e))
* **restore:** key handling for encrypted volumes that works with any secret manager ([384a9dd](https://github.com/nxckdx/longhorntx/commit/384a9dd6428f8b2e86070510aac9841092948446))
* show encrypted volumes (volume details, volumes -o wide) ([70b3c76](https://github.com/nxckdx/longhorntx/commit/70b3c76be5dd9970c951a0f7d714610d4b9d6161))
* **switch:** continue an interrupted switch (named freeze, Continue) ([5c29678](https://github.com/nxckdx/longhorntx/commit/5c2967817c81703bf59ff03427f496056212df12))
* **switch:** guard the key of encrypted volumes while the PVC is re-created ([f2d3acd](https://github.com/nxckdx/longhorntx/commit/f2d3acd6655376f2bc0f12c0d0aa2752f61d821d))
* **switch:** hand switches to the lhtx agent when it can do them (--local) ([bde3c5d](https://github.com/nxckdx/longhorntx/commit/bde3c5ddcb7e43722deeae2f594259b22061eb6c))
* **switch:** switch --from-backup; namespace users switch through the agent ([90f6904](https://github.com/nxckdx/longhorntx/commit/90f69043c010c4db7732790bb5c48a872e763407))
* **tui:** History panel in the volume details (H) ([715e801](https://github.com/nxckdx/longhorntx/commit/715e801bf6ad211a3db3b7366ccd587db7de61ee))
* **tui:** restore + switch and w hand the switch to the lhtx agent when it can do it ([911cc08](https://github.com/nxckdx/longhorntx/commit/911cc08eca37faecca12e0aff7e4ed910e6b3e3c))
* **tui:** restore and clone forms choose the key of encrypted sources ([3233e84](https://github.com/nxckdx/longhorntx/commit/3233e84437df3cf38ad0e03a188eb69d6297b9ba))
* **tui:** with the agent, Z asks whether the freeze should end on its own (1h … 24h) ([a142ba0](https://github.com/nxckdx/longhorntx/commit/a142ba0c29f62a01714257c7006389df48bbeadc))
* **tui:** Z hands freezes to the lhtx agent when one runs ([b01a641](https://github.com/nxckdx/longhorntx/commit/b01a641c581c3ef2ba57353056305f33c5731f8f))


### Bug Fixes

* **actions:** retry a patch with a stale resourceVersion if its fields did not change ([aa3d3dd](https://github.com/nxckdx/longhorntx/commit/aa3d3dd4a781e867caa5417fd87dceb46a93e445))
* **agent:** a restore volume is reused only if this Switch created it ([923681a](https://github.com/nxckdx/longhorntx/commit/923681af6e6cc1a5f038e3fface3c60f08d7b1fb))
* **agent:** lhtx_agent_digest_volumes counts the existing digests from the start ([790f52b](https://github.com/nxckdx/longhorntx/commit/790f52b019feb82eda36c40af09655b28a170e84))
* **agent:** manifests pass the Longhorn namespace to the agent ([2aad64e](https://github.com/nxckdx/longhorntx/commit/2aad64e0fd80d77074cebe6ea9e56545266e7f4a))
* **agent:** never take an existing volume as the restore unless it is the restore of that backup ([ec539ed](https://github.com/nxckdx/longhorntx/commit/ec539edfb98ec63c51a470386f29a24da6282d95))
* **agent:** RBAC – watch on Longhorn volumes for the digests ([f851d2a](https://github.com/nxckdx/longhorntx/commit/f851d2a08286e18c8359902409cf27328883847f))
* **agent:** record a rebuild's start once the replica is scheduled (it has no node when created) ([365f6b1](https://github.com/nxckdx/longhorntx/commit/365f6b16267af5a3c17052a5dc29a184b07b2056))
* **agent:** write a deleted VolumeDigest again at once ([5f10f28](https://github.com/nxckdx/longhorntx/commit/5f10f282c4f7725da156d593119e8d4a01e2bad3))
* backup target poll intervals shown compactly (5m, not 5m0s) ([c9a624c](https://github.com/nxckdx/longhorntx/commit/c9a624c98d15637d46f24da2720fcfb2fa1ca7a2))
* **cli:** targets shows one line of a backup target error (full text with -o json) ([9ab8f3a](https://github.com/nxckdx/longhorntx/commit/9ab8f3a4cfa1dc2a5da6e4c4d0b9aff91182f057))
* **doctor:** a switch finished by hand is no warning ([75edfb4](https://github.com/nxckdx/longhorntx/commit/75edfb4e349f2e1cccf63022d2f98c2d5875938b))
* **doctor:** a switch that failed before changing anything is no warning ([2dd2b87](https://github.com/nxckdx/longhorntx/commit/2dd2b87f1dd7ab8738170f044336bd52b985cbed))
* **freeze:** nudge again only owners that have no pod ([87c0859](https://github.com/nxckdx/longhorntx/commit/87c085941b75c5db0323a96aa4f063a30091738e))
* **freeze:** nudge owners again while volumes are not back after unfreeze ([6efcef5](https://github.com/nxckdx/longhorntx/commit/6efcef5b7ba09b6318647771bee261e3bd4ca5c2))
* **freeze:** run pre-freeze hooks only in a ready pod ([74d2cf4](https://github.com/nxckdx/longhorntx/commit/74d2cf47d8f0ae5cd8c0eca6461881b3ef52e5cd))
* **kube:** do not print API server warnings (klog on stderr, breaks the TUI) ([668af13](https://github.com/nxckdx/longhorntx/commit/668af1321e653248a2f506e19b0e0f090af3c19e))
* **nix:** lab NFS export allows NATed pod traffic (insecure) ([c3819a6](https://github.com/nxckdx/longhorntx/commit/c3819a6ae4fdbf681b6f1e91e40131cc0fe5f025))
* **nix:** vendorHash for the agent's client-go packages ([adc44f8](https://github.com/nxckdx/longhorntx/commit/adc44f8c4176bc501c773230eb2df935db239d31))
* **replicas:** lowering the replica count also deletes the surplus replicas ([f10f664](https://github.com/nxckdx/longhorntx/commit/f10f66432999633cc6d2de74104a04861b29de43))
* **restore:** name --key-from-context when the source's key is unknown ([ae5fc37](https://github.com/nxckdx/longhorntx/commit/ae5fc3755320bb99c02f248c4a4da00b668f3c02))
* **restore:** name the new volume after the former PVC when the volume has none now ([142c983](https://github.com/nxckdx/longhorntx/commit/142c9834285edd4e0cf06d2f049a9122006beff3))
* **restore:** volume names of at most 40 characters, named after the PVC ([e3d4e0a](https://github.com/nxckdx/longhorntx/commit/e3d4e0af344af8a0b8de041818ccbe16ab76b893))
* **switch:** clear recovery steps when the key changed; unfreeze checks the kept key ([f1229db](https://github.com/nxckdx/longhorntx/commit/f1229dbff688257f6b4d94c4abd2321c89d1c65e))


### Performance Improvements

* **agent:** rewrite a digest for size changes above the larger of 64 MiB and 1 % ([72c3f90](https://github.com/nxckdx/longhorntx/commit/72c3f900b9df0f53b28c2e79846558faa8b39078))
* **agent:** VolumeDigest data gzipped (watch events are not compressed) ([b2f35ad](https://github.com/nxckdx/longhorntx/commit/b2f35adbfd640f48101ad69317f4fde5c50399dd))

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
