#!/usr/bin/env bash
# Manual test: virtio-fs shared mount — host writes a file, guest reads it,
# guest writes back, host reads it. Bidirectional bind via virtio-fs.
set -u
GC=/tmp/gocracker-fixed
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.."; pwd)"
: "${KERNEL:=$REPO/artifacts/kernels/gocracker-guest-virtiofs-vmlinux}"
[[ -f "$KERNEL" ]] || KERNEL="$REPO/artifacts/kernels/gocracker-guest-minimal-vmlinux"
WORK=/tmp/gc-manual-shared
PORT=8595

# Cleanup on any exit (PASS, FAIL, Ctrl-C, SIGTERM): kill stray gocracker
# procs and wipe our workdir so /tmp doesn't accumulate GiBs across runs.
trap 'sudo pkill -9 -f "gocracker-fixed" 2>/dev/null; sudo rm -rf "$WORK" "$WORK_A" "$WORK_B" "$MIGR_DIR" 2>/dev/null' EXIT INT TERM


sudo pkill -9 -f "gocracker-fixed serve" 2>/dev/null
sudo rm -rf "$WORK"; mkdir -p "$WORK/share"

# Host writes a marker that the guest must see
echo "from-host" > "$WORK/share/host-wrote.txt"
chmod 666 "$WORK/share/host-wrote.txt"
chmod 777 "$WORK/share"

sudo -E "$GC" serve -addr "127.0.0.1:$PORT" -cache-dir "$WORK/cache" \
    -jailer off \
    -trusted-kernel-dir "$(dirname "$KERNEL")" \
    -trusted-snapshot-dir "$WORK" \
    -trusted-work-dir "$WORK" \
    >"$WORK/serve.log" 2>&1 &
SRV=$!
sleep 2

echo "→ run alpine VM with virtio-fs mount $WORK/share -> /mnt/shared"
RES=$(curl -s -X POST "http://127.0.0.1:$PORT/run" \
  -H "Content-Type: application/json" \
  -d "{
    \"image\":\"alpine:3.20\",
    \"kernel_path\":\"$KERNEL\",
    \"mem_mb\":256,
    \"disk_size_mb\":256,
    \"cmd\":[\"sh\",\"-lc\",\"sleep infinity\"],
    \"exec_enabled\":true,
    \"mounts\":[{\"source\":\"$WORK/share\",\"target\":\"/mnt/shared\",\"backend\":\"virtiofs\",\"read_only\":false}]
  }")
echo "   $RES" | head -c 200; echo
VM=$(echo "$RES" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("id",""))' 2>/dev/null)
if [[ -z "$VM" ]]; then
  echo "FAIL: /run rejected the mount"
  tail -25 "$WORK/serve.log"
  sudo kill -9 $SRV 2>/dev/null; exit 1
fi

for _ in $(seq 1 90); do
  S=$(curl -s "http://127.0.0.1:$PORT/vms/$VM" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("state",""))' 2>/dev/null)
  [[ "$S" == "running" ]] && break; sleep 1
done
echo "   state: $S"

# 1. guest reads the host-written file
echo "→ guest cat /mnt/shared/host-wrote.txt"
RES1=$(curl -s -X POST "http://127.0.0.1:$PORT/vms/$VM/exec" \
  -H "Content-Type: application/json" \
  -d '{"command":["cat","/mnt/shared/host-wrote.txt"]}')
echo "   $RES1"
GUEST_READ_OK=0
echo "$RES1" | grep -q "from-host" && GUEST_READ_OK=1

# 2. guest writes a marker the host must see
echo "→ guest write /mnt/shared/guest-wrote.txt"
GMARK="from-guest-$(date +%s)"
curl -s -X POST "http://127.0.0.1:$PORT/vms/$VM/exec" \
  -H "Content-Type: application/json" \
  -d "{\"command\":[\"sh\",\"-c\",\"echo $GMARK > /mnt/shared/guest-wrote.txt && sync\"]}" | head -c 200; echo
sleep 1

echo "→ host cat $WORK/share/guest-wrote.txt"
HOST_READ_OK=0
if [[ -f "$WORK/share/guest-wrote.txt" ]] && grep -q "$GMARK" "$WORK/share/guest-wrote.txt"; then
  HOST_READ_OK=1
  cat "$WORK/share/guest-wrote.txt"
else
  echo "host cannot see guest's write"
fi

# Cleanup
curl -s -X POST "http://127.0.0.1:$PORT/vms/$VM/stop" -d '{}' >/dev/null
sudo kill -9 $SRV 2>/dev/null
wait 2>/dev/null

if [[ "$GUEST_READ_OK" == "1" && "$HOST_READ_OK" == "1" ]]; then
  echo "PASS shared-disk: host→guest AND guest→host file visibility verified"
else
  echo "FAIL shared-disk: guest_read=$GUEST_READ_OK host_read=$HOST_READ_OK"
  exit 1
fi
