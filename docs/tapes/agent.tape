# The optional lhtx agent: a freeze carried out in the cluster (with an
# auto-unfreeze watchdog), then the history it keeps (demo cluster).
Output docs/agent.gif
Set Height 800
Source docs/tapes/setup.tape

Type "LHTX_DEMO_AGENT=1 lhtx --demo" Sleep 500ms Enter
Sleep 3s
Type "W" Sleep 600ms
Type "Z" Sleep 1500ms
Down Sleep 600ms Enter
Sleep 2500ms
Down Sleep 500ms Down Sleep 800ms Enter
Sleep 3s
Type "Y" Sleep 6s
Escape Sleep 1500ms
Down 10 Sleep 1s
Enter Sleep 4s
Type "H" Sleep 4s
Escape Sleep 800ms
Type "q"
Sleep 300ms
