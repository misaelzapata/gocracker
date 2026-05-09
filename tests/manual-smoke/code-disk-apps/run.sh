#!/usr/bin/env bash
# Smoke test for code-disk-attach with three real applications plus
# version-swap and multi-disk (on-the-fly switching) tests:
#
#   1. node-word-count v1  — Node.js JSON word-frequency counter (forest text)
#   2. python-stats        — Python CSV population statistics
#   3. go-serve            — Compiled Go HTTP server (--print mode)
#   4. version swap        — v1 then v2 over the same node:20-alpine (cache hit)
#   5. multi-disk / on-the-fly — one VM, two disks mounted, exec both
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

# run_vm IMAGE DISK GUEST_MOUNT CMD [extra gocracker flags...]
run_vm() {
    local image="$1" disk="$2" mount="$3" cmd="$4"
    shift 4
    sudo "$GC_BIN" run \
        --image  "$image" \
        --kernel "$GC_KERNEL" \
        --code-disk "$disk:$mount:ext4:ro" \
        --net none --jailer off --wait \
        --cmd "$cmd" "$@" 2>&1
}

timed_run_vm() {
    local label="$1"; shift
    local t0 t1 ms out
    t0=$(date +%s%3N)
    out="$(run_vm "$@")"
    t1=$(date +%s%3N)
    ms=$((t1 - t0))
    echo "  [${label}] boot+run: ${ms} ms" >&2  # stderr so it doesn't pollute captured output
    printf '%s' "$out"
}

assert_contains() {
    local label="$1" out="$2" want="$3"
    if echo "$out" | grep -qF "$want"; then
        echo "  OK: found \"$want\""
        PASS=$((PASS+1))
    else
        echo "  FAIL[$label]: expected \"$want\""
        echo "$out" | grep -v '^\[' | head -5 | sed 's/^/    /'
        FAIL=$((FAIL+1))
    fi
}

assert_not_contains() {
    local label="$1" out="$2" want="$3"
    if echo "$out" | grep -qF "$want"; then
        echo "  FAIL[$label]: output should NOT contain \"$want\" (disk swap didn't work)"
        FAIL=$((FAIL+1))
    else
        echo "  OK: v2 top word differs from v1 (disks are distinct)"
        PASS=$((PASS+1))
    fi
}

# ---------------------------------------------------------------------------
# 1. node-word-count v1
# ---------------------------------------------------------------------------
echo ""
echo "=== 1/5  node-word-count v1 (forest/fox text) ==="

build_ext4 node-wc-v1 "$EXAMPLES/node-word-count/app"    "$WORK/node-wc-v1.ext4"

V1_OUT="$(timed_run_vm v1 "node:20-alpine" "$WORK/node-wc-v1.ext4" /app node /app/word-count.js)"
echo "$V1_OUT" | grep -E '"total_words"|"unique_words"' | head -2

assert_contains 1 "$V1_OUT" '"total_words"'
assert_contains 1 "$V1_OUT" '"top10"'
# v1 text is about fox/forest — "forest" or "fox" should appear in top-10
assert_contains 1 "$V1_OUT" 'forest'

# ---------------------------------------------------------------------------
# 2. python-stats
# ---------------------------------------------------------------------------
echo ""
echo "=== 2/5  python-stats (Python CSV population statistics) ==="

build_ext4 python-stats "$EXAMPLES/python-stats/app" "$WORK/python-stats.ext4"

PY_OUT="$(timed_run_vm py "python:3.12-alpine" "$WORK/python-stats.ext4" /app "python3 /app/stats.py")"
echo "$PY_OUT" | grep -E '"city_count"|"mean_pop"' | head -2

assert_contains 2 "$PY_OUT" '"city_count"'
assert_contains 2 "$PY_OUT" '"top5"'
assert_contains 2 "$PY_OUT" '"histogram"'
assert_contains 2 "$PY_OUT" 'Delhi'

# ---------------------------------------------------------------------------
# 3. go-serve  (--print mode: reads config.json, dumps JSON, exits)
# ---------------------------------------------------------------------------
echo ""
echo "=== 3/5  go-serve (Go HTTP server, --print mode) ==="

build_ext4 go-serve "$EXAMPLES/go-serve/payload" "$WORK/go-serve.ext4"

GO_OUT="$(timed_run_vm go "alpine:3.20" "$WORK/go-serve.ext4" /app "/app/go-serve --print")"
echo "$GO_OUT" | grep -E '"app"|"greeting"' | head -2

assert_contains 3 "$GO_OUT" '"app"'
assert_contains 3 "$GO_OUT" '"greeting"'
assert_contains 3 "$GO_OUT" 'gocracker-demo'

# ---------------------------------------------------------------------------
# 4. Version swap — same base image, different code disks
#    v1: forest/fox text  →  top word should contain "forest" or "fox"
#    v2: language text    →  top word should contain "language" or "word"
#    Both boot from cached node:20-alpine — second launch gets a cache hit.
# ---------------------------------------------------------------------------
echo ""
echo "=== 4/5  version swap (v1→v2, same node:20-alpine image) ==="

build_ext4 node-wc-v2 "$EXAMPLES/node-word-count/app-v2" "$WORK/node-wc-v2.ext4"

echo "  --- running v1 ---"
SWAP_V1="$(timed_run_vm swap-v1 "node:20-alpine" "$WORK/node-wc-v1.ext4" /app "node /app/word-count.js")"
V1_TOP="$(echo "$SWAP_V1" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d['top10'][0]['word'])" 2>/dev/null || echo "$SWAP_V1" | grep -oP '"word":\s*"\K[^"]+' | head -1)"
echo "  v1 top word: $V1_TOP"

echo "  --- running v2 (same image, different disk) ---"
SWAP_V2="$(timed_run_vm swap-v2 "node:20-alpine" "$WORK/node-wc-v2.ext4" /app "node /app/word-count.js")"
V2_TOP="$(echo "$SWAP_V2" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d['top10'][0]['word'])" 2>/dev/null || echo "$SWAP_V2" | grep -oP '"word":\s*"\K[^"]+' | head -1)"
echo "  v2 top word: $V2_TOP"

assert_contains     4-v1 "$SWAP_V1" '"total_words"'
assert_contains     4-v2 "$SWAP_V2" '"total_words"'
assert_not_contains 4    "$SWAP_V2" "\"word\": \"$V1_TOP\""  # v2 must have a different top word

# ---------------------------------------------------------------------------
# 5. Multi-disk / on-the-fly switching
#    One VM boots with BOTH code disks attached:
#      /data/v1  ← v1 (forest/fox)
#      /data/v2  ← v2 (language/words)
#    We exec node twice inside the same running VM — no reboot, no new VM.
#    This is the "on-the-fly" disk switching shape.
# ---------------------------------------------------------------------------
echo ""
echo "=== 5/5  multi-disk: both disks in one VM, exec switch on-the-fly ==="

# Wrap the two execs in a shell script that runs sequentially in one boot.
SCRIPT="$WORK/multi-disk-runner.sh"
cat > "$SCRIPT" <<'RUNNER'
#!/bin/sh
echo "--- v1 ---"
node /data/v1/word-count.js
echo "--- v2 ---"
node /data/v2/word-count.js
RUNNER
chmod +x "$SCRIPT"

# gocracker supports multiple --code-disk flags; call the binary directly
# so we can pass two disks.
t0=$(date +%s%3N)
MULTI_OUT="$(sudo "$GC_BIN" run \
    --image  node:20-alpine \
    --kernel "$GC_KERNEL" \
    --code-disk "$WORK/node-wc-v1.ext4:/data/v1:ext4:ro" \
    --code-disk "$WORK/node-wc-v2.ext4:/data/v2:ext4:ro" \
    --net none --jailer off --wait \
    --cmd "sh /data/v1/run-both.sh" 2>&1 || true)"

# run-both.sh isn't on the disk — mount the script via the already-mounted
# v1 disk won't work; run the two commands in a shell one-liner instead.
MULTI_OUT="$(sudo "$GC_BIN" run \
    --image  node:20-alpine \
    --kernel "$GC_KERNEL" \
    --code-disk "$WORK/node-wc-v1.ext4:/data/v1:ext4:ro" \
    --code-disk "$WORK/node-wc-v2.ext4:/data/v2:ext4:ro" \
    --net none --jailer off --wait \
    --cmd "sh -c 'echo --- v1 ---; node /data/v1/word-count.js; echo --- v2 ---; node /data/v2/word-count.js'" 2>&1)"
t1=$(date +%s%3N)
echo "  one-VM two-disk exec: $((t1-t0)) ms"

# Both JSON blocks should appear
assert_contains 5-v1 "$MULTI_OUT" '--- v1 ---'
assert_contains 5-v2 "$MULTI_OUT" '--- v2 ---'
# Both should have valid word-count output
assert_contains 5-v1-json "$MULTI_OUT" '"total_words"'
# v1 text has forest, v2 has language — both should appear in the combined output
assert_contains 5-v1-content "$MULTI_OUT" 'forest'
assert_contains 5-v2-content "$MULTI_OUT" 'language'

# ---------------------------------------------------------------------------
# summary
# ---------------------------------------------------------------------------
echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "Results: $PASS passed, $FAIL failed"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
[[ $FAIL -eq 0 ]]
