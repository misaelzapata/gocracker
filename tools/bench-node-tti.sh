#!/usr/bin/env bash
# bench-node-tti.sh — reproduce the ComputeSDK-style Time-to-Interactive
# benchmark (https://www.computesdk.com/benchmarks/) on top of gocracker.
#
# ComputeSDK times `await compute.sandbox.create()` + `await sandbox.runCommand("node -v")`
# until the first successful stdout byte, against a pre-built sandbox image.
# We mirror that here with `gocracker run --dockerfile ... --wait` pointed
# at a tiny node:20-alpine image whose CMD is `node -v`, and a warm artifact
# cache (the image is built on the first iteration and reused after).
#
# Usage:
#   ./tools/bench-node-tti.sh [ITERATIONS]          # default: 10 timed runs
#   GC_KERNEL=... ./tools/bench-node-tti.sh         # override guest kernel
#   GC_BIN=...    ./tools/bench-node-tti.sh         # override gocracker CLI
#
# Output: one "TTI %d: %dms" line per iteration, then a median / mean / p95
# summary. Exit 0 on success, 1 if any run failed to print the node version.

set -eu

ITER="${1:-10}"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GC_BIN="${GC_BIN:-$REPO_ROOT/bin/gocracker}"
GC_KERNEL="${GC_KERNEL:-$REPO_ROOT/artifacts/kernels/gocracker-guest-minimal-vmlinux}"
WORKDIR="${BENCH_WORKDIR:-/tmp/gc-node-tti}"
CACHE="$WORKDIR/cache"

[[ -x "$GC_BIN" ]] || { echo "error: gocracker CLI not found at $GC_BIN (build with 'make' or set GC_BIN)" >&2; exit 2; }
[[ -f "$GC_KERNEL" ]] || { echo "error: guest kernel not found at $GC_KERNEL (build it or set GC_KERNEL)" >&2; exit 2; }

# Dockerfile is the same sandbox shape computesdk's leaderboard assumes:
# a node runtime already installed, booted, first stdout is the version.
mkdir -p "$WORKDIR"
cat > "$WORKDIR/Dockerfile" <<'EOF'
FROM node:20-alpine
CMD ["node","-v"]
EOF

# Warm the cache: pulls node:20-alpine, extracts layers, builds the ext4
# disk image. Subsequent iterations hit this cache and only pay VMM + VM
# boot + node -v stdout, which is what ComputeSDK actually measures (their
# providers also have pre-built sandbox images).
echo "warming artifact cache (first pull may take ~10-30s)..."
sudo "$GC_BIN" run \
    -dockerfile "$WORKDIR/Dockerfile" \
    -context "$WORKDIR" \
    -kernel "$GC_KERNEL" \
    -mem 256 -disk 1024 -net none -jailer off -wait \
    -cache-dir "$CACHE" > "$WORKDIR/warmup.log" 2>&1

if ! grep -q '^v[0-9]' "$WORKDIR/warmup.log"; then
    echo "error: warmup run did not print a node version; log at $WORKDIR/warmup.log" >&2
    tail -20 "$WORKDIR/warmup.log" >&2
    exit 1
fi

# Timed iterations.
samples_file="$(mktemp)"
trap 'rm -f "$samples_file"' EXIT
fails=0
echo "=== $ITER timed runs ==="
for i in $(seq 1 "$ITER"); do
    t0="$(date +%s%3N)"
    if sudo "$GC_BIN" run \
        -dockerfile "$WORKDIR/Dockerfile" \
        -context "$WORKDIR" \
        -kernel "$GC_KERNEL" \
        -mem 256 -disk 1024 -net none -jailer off -wait \
        -cache-dir "$CACHE" 2>&1 | grep -q '^v[0-9]'; then
        t1="$(date +%s%3N)"
        tti=$(( t1 - t0 ))
        echo "TTI $i: ${tti}ms"
        echo "$tti" >> "$samples_file"
    else
        echo "TTI $i: FAIL" >&2
        fails=$(( fails + 1 ))
    fi
done

# Summary via python so we don't reimplement percentile logic in bash.
python3 - <<PY
import statistics as st
nums = sorted(int(l) for l in open("$samples_file") if l.strip())
if not nums:
    print("no successful samples"); raise SystemExit(1)
p95 = nums[max(0, int(round(0.95 * (len(nums)-1))))]
print("--- summary ---")
print(f"runs     = {len(nums)} ({$fails} failed)")
print(f"median   = {int(st.median(nums))} ms")
print(f"mean     = {int(st.mean(nums))} ms")
print(f"min/max  = {min(nums)} / {max(nums)} ms")
print(f"p95      = {p95} ms")
PY

exit $(( fails == 0 ? 0 : 1 ))
