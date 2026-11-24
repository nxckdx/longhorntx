# Write mode: every change shows the exact request and needs confirmation
# (replica count, expansion, restore of a deleted volume into a new one).
Output docs/write.gif
Set Height 800
Source docs/tapes/setup.tape

Type "lhtx --demo" Sleep 500ms Enter
Sleep 2500ms
Type "/" Sleep 300ms Type "valkey" Sleep 600ms Enter Sleep 400ms
Type "W" Sleep 1s
Type "R" Sleep 600ms Ctrl+U Type "3" Sleep 600ms Enter
Sleep 3500ms
Escape Sleep 800ms
Type "E" Sleep 600ms Type "4Gi" Sleep 600ms Enter
Sleep 3000ms
Escape Sleep 800ms
Type "6" Sleep 1500ms
Type "r" Sleep 2500ms Enter
Sleep 3500ms
Escape Sleep 800ms
Type "q"
Sleep 300ms
