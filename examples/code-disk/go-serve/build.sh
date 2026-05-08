#!/usr/bin/env bash
# Compile go-serve for linux/amd64 (static) and build an ext4 code disk.
# Output: go-serve.ext4  (in the same directory as this script)
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OUT="$DIR/go-serve.ext4"

command -v mkfs.ext4 >/dev/null 2>&1 || { echo "mkfs.ext4 not found (apt: e2fsprogs)" >&2; exit 1; }
command -v go      >/dev/null 2>&1 || { echo "go not on PATH" >&2; exit 1; }

echo "compiling go-serve for linux/amd64..."
mkdir -p "$DIR/payload"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o "$DIR/payload/go-serve" "$DIR/src/"
cp "$DIR/src/config.json" "$DIR/payload/"

truncate -s 32M "$OUT"
mkfs.ext4 -F -L go-serve -d "$DIR/payload" "$OUT" >/dev/null
echo "built: $OUT  ($(du -h "$OUT" | cut -f1))"
