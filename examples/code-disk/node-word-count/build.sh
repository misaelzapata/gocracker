#!/usr/bin/env bash
# Build an ext4 code disk from the node-word-count app directory.
# Output: node-word-count.ext4  (in the same directory as this script)
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OUT="$DIR/node-word-count.ext4"

command -v mkfs.ext4 >/dev/null 2>&1 || { echo "mkfs.ext4 not found (apt: e2fsprogs)" >&2; exit 1; }

truncate -s 32M "$OUT"
mkfs.ext4 -F -L node-word-count -d "$DIR/app" "$OUT" >/dev/null
echo "built: $OUT  ($(du -h "$OUT" | cut -f1))"
