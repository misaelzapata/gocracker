#!/usr/bin/env bash
# bench-node-tti.sh — Time-to-Interactive benchmark for gocracker.
#
# Times `gocracker run` pointed at node:20-alpine + `node -v`, from
# process start to the first stdout byte. Two modes:
#
#   default (cold-CLI): `gocracker run --dockerfile ...` against a warm
#                       OCI artifact cache. Equivalent to ComputeSDK's
#                       leaderboard methodology when called WITHOUT a
#                       snapshot pool. ~225–235 ms median on the
#                       reference host.
#
#   WARM=1 (snapshot-pool): `gocracker run --image ... --warm` against
#                           a warm artifact cache PLUS a warm snapshot
#                           pool (auto-captured on first run, restored
#                           on subsequent). Equivalent to how Daytona,
#                           Vercel and other providers actually back
#                           their public TTI numbers — they don't really
#                           cold-boot per request. ~95–125 ms median on
#                           the reference host, in Daytona's published
#                           ~100 ms territory.
#
# Usage:
#   ./tools/bench-node-tti.sh [ITERATIONS]                # cold-CLI
#   WARM=1 ./tools/bench-node-tti.sh [ITERATIONS]         # snapshot-pool
#   GC_KERNEL=... GC_BIN=... ./tools/bench-node-tti.sh    # overrides
#
# Output: one "TTI %d: %dms" line per iteration, then a median / mean /
# p95 summary. Exit 0 on success, 1 if any run failed to print the
# node version.

set -eu

ITER="${1:-10}"
WARM="${WARM:-0}"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GC_BIN="${GC_BIN:-$REPO_ROOT/bin/gocracker}"
GC_KERNEL="${GC_KERNEL:-$REPO_ROOT/artifacts/kernels/gocracker-guest-minimal-vmlinux}"
WORKDIR="${BENCH_WORKDIR:-/tmp/gc-node-tti}"
CACHE="$WORKDIR/cache"

[[ -x "$GC_BIN" ]] || { echo "error: gocracker CLI not found at $GC_BIN (build with 'make' or set GC_BIN)" >&2; exit 2; }
[[ -f "$GC_KERNEL" ]] || { echo "error: guest kernel not found at $GC_KERNEL (build it or set GC_KERNEL)" >&2; exit 2; }

# Optional CPU affinity + scheduler priority. Pinning the boot path to a
# dedicated core + FIFO priority knocks down jitter (p95/p99) and brings
# the median a few ms tighter — both matter for a latency SLO. If taskset
# or chrt aren't present, or TTI_PIN_CPUS is empty, fall back to plain sudo.
# Override with TTI_PIN_CPUS="" to disable pinning entirely.
: "${TTI_PIN_CPUS:=0}"
PIN_WRAPPER=()
if [[ -n "$TTI_PIN_CPUS" ]] && command -v taskset >/dev/null 2>&1; then
    PIN_WRAPPER=(taskset -c "$TTI_PIN_CPUS")
    if command -v chrt >/dev/null 2>&1; then
        PIN_WRAPPER=(chrt -r 50 "${PIN_WRAPPER[@]}")
    fi
fi

mkdir -p "$WORKDIR"

if [[ "$WARM" = "1" ]]; then
    # Snapshot-pool mode. Build the gocracker run argv that takes the
    # warm path: --image (NOT --dockerfile, see warmCacheInputsReady in
    # pkg/container/warmcache.go), --warm (auto-snapshot on miss,
    # restore on hit), --cmd 'node -v' (snapshot is captured in
    # InteractiveExec mode and the cmd is exec'd post-restore via the
    # toolbox).
    GC_ARGS=(
        -image node:20-alpine
        -kernel "$GC_KERNEL"
        -mem 256 -disk 1024 -net none -jailer off -wait
        -warm -cmd 'node -v'
        -cache-dir "$CACHE"
    )
    LABEL="snapshot-pool (-warm)"
else
    # Cold-CLI mode. node:20-alpine is built via Dockerfile, the artifact
    # cache caches the resulting ext4 image, every iteration cold-boots.
    cat > "$WORKDIR/Dockerfile" <<'EOF'
FROM node:20-alpine
CMD ["node","-v"]
EOF
    GC_ARGS=(
        -dockerfile "$WORKDIR/Dockerfile"
        -context "$WORKDIR"
        -kernel "$GC_KERNEL"
        -mem 256 -disk 1024 -net none -jailer off -wait
        -cache-dir "$CACHE"
    )
    LABEL="cold-CLI (no -warm)"
fi

# Warm the cache. In WARM=1 this also seeds the snapshot pool (the
# capture goroutine runs in the background and persists the snapshot
# under ~/.cache/gocracker/snapshots before we exit). The first
# invocation pays cold-boot + snapshot-capture cost; we don't time it.
echo "mode: $LABEL"
echo "warming caches (first run may take ~10-30 s)..."
sudo "${PIN_WRAPPER[@]}" "$GC_BIN" run "${GC_ARGS[@]}" > "$WORKDIR/warmup.log" 2>&1

if ! grep -q '^v[0-9]' "$WORKDIR/warmup.log"; then
    echo "error: warmup run did not print a node version; log at $WORKDIR/warmup.log" >&2
    tail -20 "$WORKDIR/warmup.log" >&2
    exit 1
fi

# In WARM mode, give the background snapshot-capture goroutine time to
# persist the snapshot before the timed loop. Without this the first
# few timed runs still go through the cold-boot path.
if [[ "$WARM" = "1" ]]; then
    sleep 3
fi

# Timed iterations.
samples_file="$(mktemp)"
trap 'rm -f "$samples_file"' EXIT
fails=0
echo "=== $ITER timed runs ($LABEL) ==="
for i in $(seq 1 "$ITER"); do
    t0="$(date +%s%3N)"
    if sudo "${PIN_WRAPPER[@]}" "$GC_BIN" run "${GC_ARGS[@]}" 2>&1 | grep -q '^v[0-9]'; then
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
print(f"mode     = $LABEL")
print(f"runs     = {len(nums)} ({$fails} failed)")
print(f"median   = {int(st.median(nums))} ms")
print(f"mean     = {int(st.mean(nums))} ms")
print(f"min/max  = {min(nums)} / {max(nums)} ms")
print(f"p95      = {p95} ms")
PY

exit $(( fails == 0 ? 0 : 1 ))
