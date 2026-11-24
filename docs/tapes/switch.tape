# Restore a backup and switch the PVC to it: plan, Flux suspended, workload
# scaled down and up, PVC re-created – all in the demo cluster.
Output docs/switch.gif
Set Height 760
Source docs/tapes/setup.tape

Type "lhtx --demo" Sleep 500ms Enter
Sleep 2500ms
Type "6" Sleep 1200ms
Type "/" Sleep 300ms Type "grafana" Sleep 600ms Enter Sleep 500ms
Type "W" Sleep 800ms
Type "r" Sleep 2000ms
Tab Tab Tab Tab Sleep 400ms Right Sleep 1500ms
Enter Sleep 4500ms
Type "Y" Sleep 17s
Escape Sleep 800ms
Type "q"
Sleep 300ms
