#!/usr/bin/env bash
# Re-records all doc GIFs from the repo root (needs vhs; on Nix:
# nix shell nixpkgs#vhs -c docs/tapes/record.sh).
set -euo pipefail
cd "$(dirname "$0")/../.."
go build -o lhtx ./cmd/lhtx
for t in docs/demo.tape docs/tapes/views.tape docs/tapes/write.tape docs/tapes/switch.tape docs/tapes/freeze.tape docs/tapes/cli.tape; do
  vhs "$t"
done
ls -la docs/*.gif
