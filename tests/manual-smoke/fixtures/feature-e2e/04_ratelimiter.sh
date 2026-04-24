#!/usr/bin/env bash
# Manual test: block-device rate limiter — dd in guest, measure wall time.
# PASS when: with 1 MB/s limit, writing 10 MB takes ≥ 9 seconds (floor).
set -u
GC=/tmp/gocracker-fixed
: "${KERNEL:=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.."; pwd)/artifacts/kernels/gocracker-guest-minimal-vmlinux}"
WORK=/tmp/gc-manual-rate
PORT=8560

# Cleanup on any exit (PASS, FAIL, Ctrl-C, SIGTERM): kill stray gocracker
# procs and wipe our workdir so /tmp doesn't accumulate GiBs across runs.
trap 'sudo pkill -9 -f "gocracker-fixed" 2>/dev/null; sudo rm -rf "$WORK" "$WORK_A" "$WORK_B" "$MIGR_DIR" 2>/dev/null' EXIT INT TERM


sudo pkill -9 -f "gocracker-fixed serve" 2>/dev/null
sudo rm -rf "$WORK"
mkdir -p "$WORK"

sudo -E "$GC" serve -addr "127.0.0.1:$PORT" -cache-dir "$WORK/cache" \
    -jailer off \
    -trusted-kernel-dir "$(dirname "$KERNEL")" \
    -trusted-snapshot-dir "$WORK" \
    >"$WORK/serve.log" 2>&1 &
SRV=$!
sleep 2

echo "→ boot alpine VM"
RES=$(curl -s -X POST "http://127.0.0.1:$PORT/run" \
  -H "Content-Type: application/json" \
  -d "{\"image\":\"alpine:3.20\",\"kernel_path\":\"$KERNEL\",\"mem_mb\":256,\"disk_size_mb\":512,\"cmd\":[\"sh\",\"-lc\",\"sleep infinity\"],\"exec_enabled\":true}")
VM=$(echo "$RES" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')

for _ in $(seq 1 60); do
  S=$(curl -s "http://127.0.0.1:$PORT/vms/$VM" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("state",""))' 2>/dev/null)
  [[ "$S" == "running" ]] && break; sleep 1
done
echo "   state: $S"

# Apply a block-device rate limit: 1 MB/s sustained.
echo "→ PUT /vms/$VM/rate-limiters/block (bandwidth 1MB/s)"
curl -s -X PUT "http://127.0.0.1:$PORT/vms/$VM/rate-limiters/block" \
  -H "Content-Type: application/json" \
  -d '{"bandwidth":{"size":1048576,"refill_time_ms":1000}}' | head -c 300; echo

# dd 10 MB into /root (rootfs = ext4 disk, NOT tmpfs like /tmp). Time from host.
echo "→ dd 10M inside guest (to /root/big, on the rate-limited block device)"
T0=$(date +%s%3N)
curl -s -X POST "http://127.0.0.1:$PORT/vms/$VM/exec" \
  -H "Content-Type: application/json" \
  -d '{"command":["sh","-c","dd if=/dev/zero of=/root/big bs=1M count=10 oflag=direct 2>&1; rm -f /root/big; echo DONE"]}' > "$WORK/dd.json"
T1=$(date +%s%3N)
cat "$WORK/dd.json" | head -c 400; echo
ELAPSED=$(( T1 - T0 ))
echo "   elapsed: ${ELAPSED} ms"

# Cleanup
curl -s -X POST "http://127.0.0.1:$PORT/vms/$VM/stop" -d '{}' >/dev/null
sudo kill -9 $SRV 2>/dev/null
wait 2>/dev/null

# 10 MB @ 1 MB/s floor = 10 seconds. Allow overhead, want ≥ 9000 ms.
if [[ "$ELAPSED" -ge 9000 ]]; then
  echo "PASS rate-limiter: 10 MB write took ${ELAPSED}ms (>= 9000 expected)"
else
  echo "FAIL rate-limiter: 10 MB write took only ${ELAPSED}ms (limiter did not enforce)"
  exit 1
fi
