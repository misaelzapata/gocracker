#!/usr/bin/env bash
# Manual smoke test for the code-disk-attach Phase 1 feature.
#
# Builds a tiny ext4 image carrying a single shell script, attaches it to
# a gocracker microVM via --code-disk, and asserts the script's stdout
# reaches the host. Then repeats with a *different* code disk over the
# same Alpine template to demonstrate the "swap the code, keep the
# template" shape that Phase 1 enables.
#
# Requires: sudo, mkfs.ext4, gocracker binary, a guest kernel.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
GC_BIN="${GC_BIN:-$REPO_ROOT/bin/gocracker}"
GC_KERNEL="${GC_KERNEL:-$REPO_ROOT/artifacts/kernels/gocracker-guest-standard-vmlinux}"
WORK="${WORK:-/tmp/gc-code-disk-smoke}"

[[ -x "$GC_BIN" ]] || { echo "missing $GC_BIN — run: go build -o bin/gocracker ./cmd/gocracker" >&2; exit 2; }
[[ -f "$GC_KERNEL" ]] || { echo "missing kernel $GC_KERNEL — see make kernel-unpack" >&2; exit 2; }
command -v mkfs.ext4 >/dev/null 2>&1 || { echo "mkfs.ext4 not on PATH (apt: e2fsprogs)" >&2; exit 2; }

sudo rm -rf "$WORK"
mkdir -p "$WORK/v1/payload" "$WORK/v2/payload"

cat > "$WORK/v1/payload/main.sh" <<'EOF'
#!/bin/sh
echo "code-disk version v1: alpha"
EOF
cat > "$WORK/v2/payload/main.sh" <<'EOF'
#!/bin/sh
echo "code-disk version v2: bravo"
EOF
chmod +x "$WORK/v1/payload/main.sh" "$WORK/v2/payload/main.sh"

build_disk() {
    local label="$1" payload="$2" img="$3"
    truncate -s 32M "$img"
    mkfs.ext4 -F -L "$label" -d "$payload" "$img" >/dev/null
}
build_disk code-v1 "$WORK/v1/payload" "$WORK/v1/code.ext4"
build_disk code-v2 "$WORK/v2/payload" "$WORK/v2/code.ext4"

run_one() {
    local label="$1" img="$2" want="$3"
    local out
    out="$(sudo "$GC_BIN" run \
        --image alpine:3.20 \
        --kernel "$GC_KERNEL" \
        --code-disk "$img:/app:ext4:ro" \
        --net none --jailer off --wait \
        --cmd "/app/main.sh" 2>&1 | grep -E "code-disk version" || true)"
    if [[ "$out" != *"$want"* ]]; then
        echo "FAIL [$label]: stdout did not contain $want; got: $out" >&2
        return 1
    fi
    echo "OK [$label]: $out"
}

run_one v1 "$WORK/v1/code.ext4" "code-disk version v1: alpha"
run_one v2 "$WORK/v2/code.ext4" "code-disk version v2: bravo"

echo
echo "code-disk smoke OK — same alpine template, two distinct code disks."
