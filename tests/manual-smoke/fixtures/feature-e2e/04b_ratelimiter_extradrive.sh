#!/usr/bin/env bash
# Manual test: block rate limiter with an EXTRA drive attached (so writes
# bypass the rootfs tmpfs overlay and actually go through virtio-blk).
# PASS: with 1 MB/s sustained limit on the extra drive, writing 10 MB to it
# takes >= 9 seconds (vs ~25ms unlimited).
set -u
GC=/tmp/gocracker-fixed
KERNEL=/home/misael/Desktop/projects/gocracker/artifacts/kernels/gocracker-guest-minimal-vmlinux
WORK=/tmp/gc-manual-rate2
PORT=8561

# Cleanup on any exit (PASS, FAIL, Ctrl-C, SIGTERM): kill stray gocracker
# procs and wipe our workdir so /tmp doesn't accumulate GiBs across runs.
trap 'sudo pkill -9 -f "gocracker-fixed" 2>/dev/null; sudo rm -rf "$WORK" "$WORK_A" "$WORK_B" "$MIGR_DIR" 2>/dev/null' EXIT INT TERM

EXTRA_IMG="$WORK/extra.img"

sudo pkill -9 -f "gocracker-fixed serve" 2>/dev/null
sudo rm -rf "$WORK"; mkdir -p "$WORK"

# Make a 64 MiB raw image to use as virtio-blk
dd if=/dev/zero of="$EXTRA_IMG" bs=1M count=64 status=none
mkfs.ext4 -F -q "$EXTRA_IMG"
chmod 666 "$EXTRA_IMG"

sudo -E "$GC" serve -addr "127.0.0.1:$PORT" -cache-dir "$WORK/cache" \
    -jailer off \
    -trusted-kernel-dir "$(dirname "$KERNEL")" \
    -trusted-snapshot-dir "$WORK" \
    -trusted-work-dir "$WORK" \
    >"$WORK/serve.log" 2>&1 &
SRV=$!
sleep 2

echo "→ run alpine VM with extra drive at 1 MB/s limit"
RES=$(curl -s -X POST "http://127.0.0.1:$PORT/run" \
  -H "Content-Type: application/json" \
  -d "{
    \"image\":\"alpine:3.20\",
    \"kernel_path\":\"$KERNEL\",
    \"mem_mb\":256,
    \"disk_size_mb\":256,
    \"cmd\":[\"sh\",\"-lc\",\"sleep infinity\"],
    \"exec_enabled\":true,
    \"drives\":[{
      \"drive_id\":\"extra\",
      \"path_on_host\":\"$EXTRA_IMG\",
      \"is_root_device\":false,
      \"is_read_only\":false,
      \"rate_limiter\":{\"bandwidth\":{\"size\":1048576,\"refill_time_ms\":1000}}
    }]
  }")
echo "   $RES" | head -c 200; echo
VM=$(echo "$RES" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')

for _ in $(seq 1 60); do
  S=$(curl -s "http://127.0.0.1:$PORT/vms/$VM" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("state",""))' 2>/dev/null)
  [[ "$S" == "running" ]] && break; sleep 1
done
echo "   state: $S"

# Find the extra block device (typically /dev/vdb), mount it, dd onto it.
echo "→ identify + mount extra drive"
curl -s -X POST "http://127.0.0.1:$PORT/vms/$VM/exec" \
  -H "Content-Type: application/json" \
  -d '{"command":["sh","-c","ls /dev/vd* && mkdir -p /mnt/extra && mount /dev/vdb /mnt/extra && df /mnt/extra"]}' | head -c 500; echo

echo "→ dd 10M to /mnt/extra (rate limited @ 1 MB/s)"
T0=$(date +%s%3N)
RES_DD=$(curl -s -X POST "http://127.0.0.1:$PORT/vms/$VM/exec" \
  -H "Content-Type: application/json" \
  -d '{"command":["sh","-c","dd if=/dev/zero of=/mnt/extra/big bs=1M count=10 conv=fsync 2>&1; sync"]}')
T1=$(date +%s%3N)
echo "   $RES_DD" | head -c 300; echo
ELAPSED=$(( T1 - T0 ))
echo "   elapsed: ${ELAPSED} ms"

# Cleanup
curl -s -X POST "http://127.0.0.1:$PORT/vms/$VM/stop" -d '{}' >/dev/null
sudo kill -9 $SRV 2>/dev/null
wait 2>/dev/null
sudo rm -f "$EXTRA_IMG"

if [[ "$ELAPSED" -ge 9000 ]]; then
  echo "PASS rate-limiter: 10 MB write took ${ELAPSED}ms (>= 9000 expected)"
else
  echo "FAIL rate-limiter: 10 MB write took only ${ELAPSED}ms (limiter did not enforce on attached drive)"
  exit 1
fi
