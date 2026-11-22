#!/usr/bin/env bash
# Writes nix/release.json (version + SHA256 per Nix system) from a goreleaser
# checksums.txt, for the `lhtx-bin` flake package (prebuilt release binary).
# Usage: nix/update-release.sh <version> <checksums.txt>
set -euo pipefail
version=${1#v}
sums=$2
out=$(dirname "$0")/release.json

hash() { awk -v f="lhtx_${version}_$1.tar.gz" '$2 == f { print $1 }' "$sums"; }

jq -n --arg version "$version" \
  --arg x86_64_linux "$(hash linux_amd64)" \
  --arg aarch64_linux "$(hash linux_arm64)" \
  --arg x86_64_darwin "$(hash darwin_amd64)" \
  --arg aarch64_darwin "$(hash darwin_arm64)" \
  '{version: $version, sha256: {
      "x86_64-linux": $x86_64_linux, "aarch64-linux": $aarch64_linux,
      "x86_64-darwin": $x86_64_darwin, "aarch64-darwin": $aarch64_darwin
    } | with_entries(select(.value != ""))}' > "$out"
echo "wrote $out for $version"
