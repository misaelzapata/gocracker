#!/usr/bin/env bash
# Build an ext4 code disk from node-word-count app-v2 (language/words text).
# Output: node-word-count-v2.ext4
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OUT="$DIR/node-word-count-v2.ext4"

command -v mkfs.ext4 >/dev/null 2>&1 || { echo "mkfs.ext4 not found (apt: e2fsprogs)" >&2; exit 1; }

truncate -s 32M "$OUT"
mkfs.ext4 -F -L node-wc-v2 -d "$DIR/app-v2" "$OUT" >/dev/null
echo "built: $OUT  ($(du -h "$OUT" | cut -f1))"
