#!/usr/bin/env bash
# Smoke test for code-disk-attach with three real applications:
#   1. node-word-count  — Node.js JSON word-frequency counter
#   2. python-stats     — Python CSV population statistics
#   3. go-serve         — Compiled Go HTTP server (--print mode)
#
# Each test builds an ext4 code disk from the examples/ directory,
# boots a microVM with that disk attached, and asserts expected output.
#
# Requires: sudo, mkfs.ext4, gocracker binary, guest kernel.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
GC_BIN="${GC_BIN:-$REPO_ROOT/bin/gocracker}"
GC_KERNEL="${GC_KERNEL:-$REPO_ROOT/artifacts/kernels/gocracker-guest-standard-vmlinux}"
WORK="${WORK:-/tmp/gc-code-disk-apps-smoke}"
EXAMPLES="$REPO_ROOT/examples/code-disk"

PASS=0
FAIL=0

[[ -x "$GC_BIN"    ]] || { echo "missing $GC_BIN — run: go build -o bin/gocracker ./cmd/gocracker" >&2; exit 2; }
[[ -f "$GC_KERNEL" ]] || { echo "missing kernel $GC_KERNEL — see make kernel-unpack" >&2; exit 2; }
command -v mkfs.ext4 >/dev/null 2>&1 || { echo "mkfs.ext4 not found (apt: e2fsprogs)" >&2; exit 2; }

sudo rm -rf "$WORK" && mkdir -p "$WORK"

# ---------------------------------------------------------------------------
# helpers
# ---------------------------------------------------------------------------
build_ext4() {
    local label="$1" src="$2" out="$3"
    truncate -s 32M "$out"
    mkfs.ext4 -F -L "$label" -d "$src" "$out" >/dev/null
}

run_vm() {
    local image="$1" disk="$2" cmd="$3"
    sudo "$GC_BIN" run \
        --image  "$image" \
        --kernel "$GC_KERNEL" \
        --code-disk "$disk:/app:ext4:ro" \
        --net none --jailer off --wait \
        --cmd "$cmd" 2>&1
}

assert_contains() {
    local label="$1" out="$2" want="$3"
    if echo "$out" | grep -qF "$want"; then
        echo "  OK: found \"$want\""
        PASS=$((PASS+1))
    else
        echo "  FAIL: expected \"$want\" in output:"
        echo "$out" | sed 's/^/    /'
        FAIL=$((FAIL+1))
    fi
}

# ---------------------------------------------------------------------------
# 1. node-word-count
# ---------------------------------------------------------------------------
echo ""
echo "=== 1/3  node-word-count (Node.js JSON word frequency) ==="

build_ext4 node-word-count "$EXAMPLES/node-word-count/app" "$WORK/node-word-count.ext4"

NODE_OUT="$(run_vm "node:20-alpine" "$WORK/node-word-count.ext4" "node /app/word-count.js")"
echo "$NODE_OUT" | grep -E "total_words|top10" | head -5

assert_contains node-word-count "$NODE_OUT" '"total_words"'
assert_contains node-word-count "$NODE_OUT" '"top10"'
assert_contains node-word-count "$NODE_OUT" '"word"'

# ---------------------------------------------------------------------------
# 2. python-stats
# ---------------------------------------------------------------------------
echo ""
echo "=== 2/3  python-stats (Python CSV population statistics) ==="

build_ext4 python-stats "$EXAMPLES/python-stats/app" "$WORK/python-stats.ext4"

PY_OUT="$(run_vm "python:3.12-alpine" "$WORK/python-stats.ext4" "python3 /app/stats.py")"
echo "$PY_OUT" | grep -E "city_count|top5|histogram" | head -5

assert_contains python-stats "$PY_OUT" '"city_count"'
assert_contains python-stats "$PY_OUT" '"top5"'
assert_contains python-stats "$PY_OUT" '"histogram"'
assert_contains python-stats "$PY_OUT" 'Delhi'

# ---------------------------------------------------------------------------
# 3. go-serve  (--print mode: reads config.json, dumps JSON, exits)
# ---------------------------------------------------------------------------
echo ""
echo "=== 3/3  go-serve (Go HTTP server config print) ==="

build_ext4 go-serve "$EXAMPLES/go-serve/payload" "$WORK/go-serve.ext4"

GO_OUT="$(run_vm "alpine:3.20" "$WORK/go-serve.ext4" "/app/go-serve --print")"
echo "$GO_OUT" | grep -E "app|greeting|version" | head -5

assert_contains go-serve "$GO_OUT" '"app"'
assert_contains go-serve "$GO_OUT" '"greeting"'
assert_contains go-serve "$GO_OUT" 'gocracker-demo'

# ---------------------------------------------------------------------------
# summary
# ---------------------------------------------------------------------------
echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "Results: $PASS passed, $FAIL failed"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
[[ $FAIL -eq 0 ]]
