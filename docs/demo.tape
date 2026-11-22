# Records docs/demo.gif with the built-in demo cluster (no real cluster
# involved). Regenerate: go build -o lhtx ./cmd/lhtx && vhs docs/demo.tape
Output docs/demo.gif

Set Shell "bash"
Set FontFamily "FiraCode Nerd Font Mono"
Set FontSize 15
Set Width 1480
Set Height 800
Set Padding 14
Set Theme "Catppuccin Mocha"
Set TypingSpeed 70ms
Set Framerate 15

Hide
Type "export PATH=$PWD:$PATH KUBECONFIG=/dev/null LHTX_NO_UPDATE_CHECK=1 PS1='$ '" Enter
Type "clear" Enter
Show

Type "lhtx --demo" Sleep 500ms Enter
Sleep 3s

# problems first, then fuzzy filter into the volume that is rebuilding
Type "p" Sleep 2s
Type "/" Sleep 300ms Type "graf" Sleep 1s Enter
Sleep 500ms Enter
Sleep 5s

# snapshots and backups of the volume
Type "s" Sleep 2500ms Escape Sleep 400ms
Type "b" Sleep 2500ms Escape Sleep 400ms
Escape Sleep 300ms Escape Sleep 800ms

# nodes, health, orphans
Type "2" Sleep 3s
Type "5" Sleep 3s
Type "6" Sleep 2s

# write mode: every change is previewed and confirmed
Type "1" Sleep 800ms
Type "/" Sleep 300ms Type "valkey" Sleep 800ms Enter Sleep 500ms
Type "W" Sleep 1s
Type "R" Sleep 600ms Ctrl+U Type "3" Sleep 600ms Enter
Sleep 4s
Escape Sleep 1s
Type "q"
Sleep 500ms
