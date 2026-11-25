# Encrypted volumes

[← README](../README.md)

Longhorn volumes can be encrypted (Longhorn's dm-crypt, the key in a Kubernetes Secret –
one for all volumes or one per PVC via `${pvc.name}` in the StorageClass):
Longhorn records only *that* a volume or backup is encrypted; which Secret
holds the key is on the PV. lhtx handles the key the same way whatever
manages the Secret – by hand, External Secrets, Sealed Secrets, a policy
engine – and never needs to know: it compares a fingerprint of the key and
its cipher parameters (kept in memory, never shown or stored).

Restore and clone create encrypted volumes for encrypted sources. Where the
key for the new PV comes from:

| case | how | what lhtx checks |
|---|---|---|
| same namespace | the new PV refers to the source's Secret (default) | – nothing is copied, nothing for a manager to fight over |
| another namespace or cluster, key already there (e.g. an ExternalSecret reading the same source) | `--secret NS/NAME` | the Secret holds the source's key, else it refuses before creating anything |
| another namespace or cluster, take the key along | `--copy-secret` | copies type + data (read only when applied); warns if the source Secret is managed – the copy is not |
| migration to another cluster | `--key-from-context CTX` plus `--secret` or `--copy-secret` | reads the source key in the other cluster to compare or copy |
| source PV gone (disaster recovery) | `--secret NS/NAME` with the key from your secret backup | cannot compare – a wrong key does not harm the data, the volume just does not open |

```
lhtx --context new --rw restore backup-… --pvc shop/db --key-from-context old --copy-secret
```

In the TUI the restore and clone forms have a *key* choice.

**Switch** keeps the PV's key refs (same PVC name, same Secret) and refuses
to mix encrypted and plain volumes. Re-creating the PVC is the moment a
manager may react – clean up, generate a new key for the "new" PVC. lhtx
reads the key before, and after the new PVC is bound and *before any pod
starts* it watches the Secret for 20 s: gone → it waits for its manager to
bring it back; another key → it keeps the old key in a Secret of its own
(label `lhtx.io/key-backup`, no owner), stops with the PVC frozen and says
how to put the key back (into the manager's source; for a manager that
generates keys itself, detach the Secret from it). `lhtx unfreeze` refuses
while the key Secret differs from that copy (`--ignore-key`).

`lhtx doctor` / Health tab: encrypted volumes whose key Secret is missing,
kept key copies, a per-PVC key used from another namespace (names via the
metadata API only). `lhtx volumes -o wide` and the volume details show
🔒 encrypted.

Do not rotate a key Secret that volumes use (Longhorn cannot re-key a
volume), and back the key Secrets up: a backup of an encrypted volume is
useless without its key. Consider encrypting Secrets in etcd as well.

See also [restore and switch](restore-switch.md).
