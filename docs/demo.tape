# Hero GIF of the README: volumes, problems first, a rebuild live.
Output docs/demo.gif
Set Height 800
Source docs/tapes/setup.tape

Type "lhtx --demo" Sleep 500ms Enter
Sleep 3s

# problems first, then fuzzy filter into the volume that is rebuilding
Type "p" Sleep 2s
Type "/" Sleep 300ms Type "graf" Sleep 1s Enter
Sleep 500ms Enter
Sleep 6s

# snapshots of the volume (loaded on demand)
Type "s" Sleep 2500ms Escape Sleep 300ms Escape Sleep 300ms Escape Sleep 800ms
Type "q"
Sleep 300ms
