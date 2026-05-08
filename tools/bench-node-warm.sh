#!/usr/bin/env bash
# bench-node-warm.sh — TTI bench for the node-warm path.
#
# Workflow:
#   1. Start sandboxd in the background with a kernel path. It auto-
#      registers base-node-warm; first run cold-boots node:22-alpine,
#      waits for the toolbox agent to mark `/runtime/node/ready` 200,
#      then snapshots. ~10-30s warmup the first time.
#   2. Iterate: lease(template=base-node-warm) → exec(["node-warm",
#      "console.log(process.version)"]) → release. Time each round.
#
# This isolates the BIG win of the warm path: exec cost goes from
# ~50 ms (fresh `node -v` startup) to ~5-10 ms (UDS dial + JSON eval)
# because V8 is already initialised inside the snapshot.
#
# Usage:
#   sudo ./tools/bench-node-warm.sh [ITERATIONS]
#   GC_KERNEL=/path/to/kernel ITERATIONS=20 sudo ./tools/bench-node-warm.sh

set -eu

ITER="${1:-${ITERATIONS:-10}}"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GC_BIN="${GC_BIN:-$REPO_ROOT/bin/gocracker}"
GC_SANDBOXD="${GC_SANDBOXD:-$REPO_ROOT/bin/gocracker-sandboxd}"
GC_KERNEL="${GC_KERNEL:-$REPO_ROOT/artifacts/kernels/gocracker-guest-standard-vmlinux}"
ADDR="${ADDR:-127.0.0.1:9091}"
STATE_DIR="${STATE_DIR:-/tmp/gc-warm-bench-state}"
LOG="${LOG:-/tmp/gc-warm-bench.log}"

[[ -x "$GC_BIN" ]] || { echo "gocracker CLI not found at $GC_BIN" >&2; exit 2; }
[[ -x "$GC_SANDBOXD" ]] || { echo "sandboxd not found at $GC_SANDBOXD" >&2; exit 2; }
[[ -f "$GC_KERNEL" ]] || { echo "kernel not found at $GC_KERNEL" >&2; exit 2; }

cleanup() {
    pkill -f gocracker-sandboxd 2>/dev/null || true
    sleep 1
}
trap cleanup EXIT

# Boot sandboxd in the background.
rm -rf "$STATE_DIR"
mkdir -p "$STATE_DIR"
"$GC_SANDBOXD" serve \
    --addr "$ADDR" \
    --state-dir "$STATE_DIR" \
    --kernel-path "$GC_KERNEL" \
    > "$LOG" 2>&1 &

# Wait for /healthz.
echo -n "waiting for sandboxd at $ADDR ..."
for i in $(seq 1 60); do
    if curl -sS --max-time 1 "http://$ADDR/healthz" > /dev/null 2>&1; then
        echo " up"
        break
    fi
    sleep 1
done

# Wait for base-node-warm to reach state=ready (the cold-boot + warm
# runner ready + snapshot path). First time this may take 30-60s.
echo -n "waiting for base-node-warm template "
for i in $(seq 1 240); do
    state=$(curl -sS "http://$ADDR/templates" | python3 -c '
import json, sys
d = json.load(sys.stdin)
for t in d.get("templates", []):
    if t.get("id") == "base-node-warm":
        print(t.get("state", ""))
        break
else:
    print("missing")
')
    case "$state" in
        ready)  echo " ready"; break ;;
        error)
            echo " ERROR — template build failed; sandboxd log tail:" >&2
            tail -30 "$LOG" >&2
            exit 1
            ;;
        *)
            echo -n "."
            sleep 1
            ;;
    esac
done
[[ "$state" == "ready" ]] || { echo "base-node-warm never became ready (last=$state)"; exit 1; }

# One warmup iter to exercise the lease path.
echo "warmup..."
curl -sS -X POST "http://$ADDR/sandboxes/lease" \
    -H 'Content-Type: application/json' \
    -d '{"template_id":"base-node-warm"}' > /tmp/warmup-lease.json
SB_ID=$(python3 -c 'import json;print(json.load(open("/tmp/warmup-lease.json"))["sandbox"]["id"])')
curl -sS -X POST "http://$ADDR/sandboxes/$SB_ID/exec" \
    -H 'Content-Type: application/json' \
    -d '{"command":["node-warm","console.log(process.version)"]}' > /tmp/warmup-exec.json
cat /tmp/warmup-exec.json | python3 -c 'import json,sys; d=json.load(sys.stdin); print("warmup stdout:", d.get("stdout","").strip(), "stderr:", d.get("stderr","").strip(), "exit:", d.get("exit_code",-1))'
curl -sS -X DELETE "http://$ADDR/sandboxes/$SB_ID" > /dev/null

# Timed loop.
samples=$(mktemp); trap 'rm -f $samples' EXIT
fails=0
echo "=== $ITER timed runs (lease + node-warm eval) ==="
for i in $(seq 1 "$ITER"); do
    t0="$(date +%s%3N)"
    LEASE=$(curl -sS -X POST "http://$ADDR/sandboxes/lease" \
        -H 'Content-Type: application/json' \
        -d '{"template_id":"base-node-warm"}')
    sb_id=$(echo "$LEASE" | python3 -c 'import json,sys;print(json.load(sys.stdin)["sandbox"]["id"])')
    EXEC=$(curl -sS -X POST "http://$ADDR/sandboxes/$sb_id/exec" \
        -H 'Content-Type: application/json' \
        -d '{"command":["node-warm","process.stdout.write(process.version)"]}')
    t1="$(date +%s%3N)"
    out=$(echo "$EXEC" | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d.get("stdout",""))')
    if [[ "$out" =~ ^v[0-9] ]]; then
        tti=$((t1 - t0))
        echo "TTI $i: ${tti}ms (out=${out:0:20})"
        echo "$tti" >> "$samples"
    else
        echo "TTI $i: FAIL (output=$out)" >&2
        fails=$((fails+1))
    fi
    curl -sS -X DELETE "http://$ADDR/sandboxes/$sb_id" > /dev/null 2>&1 || true
done

python3 - <<PY
import statistics as st
nums = sorted(int(l) for l in open("$samples") if l.strip())
if not nums:
    print("no successful samples"); raise SystemExit(1)
p95 = nums[max(0, int(round(0.95 * (len(nums)-1))))]
print()
print("--- summary ---")
print(f"runs     = {len(nums)} ({$fails} failed)")
print(f"median   = {int(st.median(nums))} ms")
print(f"mean     = {int(st.mean(nums))} ms")
print(f"min/max  = {min(nums)} / {max(nums)} ms")
print(f"p95      = {p95} ms")
PY

exit $(( fails == 0 ? 0 : 1 ))
