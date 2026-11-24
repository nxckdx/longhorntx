# Freeze everything for a Longhorn upgrade and unfreeze it – no scaling,
# operators and GitOps keep their specs (demo cluster).
Output docs/freeze.gif
Set Height 800
Source docs/tapes/setup.tape

Type "lhtx --demo" Sleep 500ms Enter
Sleep 2500ms
Type "W" Sleep 600ms
Type "Z" Sleep 1500ms
Down Sleep 600ms Enter
Sleep 3500ms
Type "Y" Sleep 5s
Escape Sleep 2500ms
Type "Z" Sleep 1500ms
Down Sleep 400ms Down Sleep 800ms Enter
Sleep 2500ms
Type "Y" Sleep 5s
Escape Sleep 2s
Type "q"
Sleep 300ms
