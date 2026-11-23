# CLI: health check with exit code, replica spread, backups of deleted volumes.
Output docs/cli.gif
Set Height 620
Source docs/tapes/setup.tape

Type "lhtx --demo doctor --min warning; echo exit=$?" Sleep 400ms Enter
Sleep 3500ms
Type "clear" Enter
Type "lhtx --demo matrix --problems" Sleep 400ms Enter
Sleep 2500ms
Type "lhtx --demo backupvolumes | head -5" Sleep 400ms Enter
Sleep 3500ms
