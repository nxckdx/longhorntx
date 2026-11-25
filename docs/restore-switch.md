# Restore and switch

[← README](../README.md)

`lhtx restore VOLUME --switch`, `lhtx switch`, `r` → *switch* in the TUI: the
web UI makes you restore under another name, detach, delete the PVC and
create PV/PVC by hand. lhtx does it in one confirmed run:

![Restore a backup and switch the PVC to it: step preview, PVC frozen, pods stopped, PVC re-created, pods back](switch.gif)

1. restore into a new volume, wait until it is restored and detached
2. freeze the PVC (see above): its pods stop, nothing can start on it or
   re-create it meanwhile; wait until the old volume is detached
3. set the old PV to `Retain`, delete the old PVC
4. create a PV for the restored volume and the PVC again with the same name,
   labels and annotations (Flux and Helm keep owning it), wait until Bound
5. unfreeze: the owners start their pods again on the restored data

The old volume is kept: `lhtx switch NS/PVC --to-volume OLD` switches back
(the old, released PV is rebound). Until the old PVC is deleted every failure
– and Ctrl+C – is rolled back; after that the PVC stays frozen until it is
fixed, so nothing re-creates it with an empty volume. Every step goes to a
journal in `$XDG_STATE_HOME/lhtx/switch/` (`lhtx switch --status`; `doctor`
warns about unfinished ones). A CloudNativePG instance can be switched, but
lhtx warns: restoring one instance's PVC does not restore the database
consistently – prefer CNPG's own recovery. `--dry-run` shows the plan.

A restore or clone into a **new** volume (+ PV/PVC) is described in
[write-actions.md](write-actions.md); encrypted volumes in
[encryption.md](encryption.md).
