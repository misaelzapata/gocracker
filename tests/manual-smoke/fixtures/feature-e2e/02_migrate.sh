#!/usr/bin/env bash
# Manual test: live-migrate VM A → B + exec on B preserves state.
# PASS when: VM boots on A, migration reports success, VM appears running
# on B, exec on B returns a file that was written on A (state preserved).
set -u
GC=/tmp/gocracker-fixed
: "${KERNEL:=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.."; pwd)/artifacts/kernels/gocracker-guest-minimal-vmlinux}"
WORK_A=/tmp/gc-manual-mig-A
WORK_B=/tmp/gc-manual-mig-B
MIGR_DIR=/tmp/gc-manual-mig-bundle
PORT_A=8540
PORT_B=8541

# Cleanup on any exit (PASS, FAIL, Ctrl-C, SIGTERM): kill stray gocracker
# procs and wipe workdirs so /tmp doesn't accumulate GiBs across runs.
trap 'sudo pkill -9 -f "gocracker-fixed" 2>/dev/null; sudo rm -rf "$WORK_A" "$WORK_B" "$MIGR_DIR" 2>/dev/null' EXIT INT TERM

sudo pkill -9 -f "gocracker-fixed serve" 2>/dev/null
sudo rm -rf "$WORK_A" "$WORK_B" "$MIGR_DIR"
mkdir -p "$WORK_A" "$WORK_B" "$MIGR_DIR"

# Start server A
sudo -E "$GC" serve -addr "127.0.0.1:$PORT_A" -cache-dir "$WORK_A/cache" \
    -jailer off \
    -trusted-kernel-dir "$(dirname "$KERNEL")" \
    -trusted-snapshot-dir "$MIGR_DIR" \
    >"$WORK_A/serve.log" 2>&1 &
SRV_A=$!
sleep 2

# Start server B
sudo -E "$GC" serve -addr "127.0.0.1:$PORT_B" -cache-dir "$WORK_B/cache" \
    -jailer off \
    -trusted-kernel-dir "$(dirname "$KERNEL")" \
    -trusted-snapshot-dir "$MIGR_DIR" \
    -state-dir "$WORK_B/state" -sock "$WORK_B/sock" \
    >"$WORK_B/serve.log" 2>&1 &
SRV_B=$!
sleep 2

# Launch VM on A with exec enabled
echo "→ launch node VM on A"
RES=$(curl -s -X POST "http://127.0.0.1:$PORT_A/run" \
  -H "Content-Type: application/json" \
  -d "{\"image\":\"node:20-alpine\",\"kernel_path\":\"$KERNEL\",\"mem_mb\":128,\"disk_size_mb\":256,\"cmd\":[\"sh\",\"-lc\",\"sleep infinity\"],\"exec_enabled\":true}")
VM_A=$(echo "$RES" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
echo "   VM on A: $VM_A"

# Wait running
for _ in $(seq 1 60); do
  S=$(curl -s "http://127.0.0.1:$PORT_A/vms/$VM_A" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("state",""))' 2>/dev/null)
  [[ "$S" == "running" ]] && break
  sleep 1
done
echo "   state on A: $S"

# Write state: create a file with timestamp
echo "→ write marker on A"
MARKER="hello-$(date +%s)"
curl -s -X POST "http://127.0.0.1:$PORT_A/vms/$VM_A/exec" \
  -H "Content-Type: application/json" \
  -d "{\"command\":[\"sh\",\"-c\",\"echo $MARKER > /tmp/marker && cat /tmp/marker\"]}" | head -c 200
echo

# Migrate A → B
echo "→ migrate $VM_A to server B"
MIG=$(curl -s -X POST "http://127.0.0.1:$PORT_A/vms/$VM_A/migrate" \
  -H "Content-Type: application/json" \
  -d "{\"destination_url\":\"http://127.0.0.1:$PORT_B\"}")
echo "   migrate resp: $MIG"

# Wait for VM on B
sleep 3
VMS_B=$(curl -s "http://127.0.0.1:$PORT_B/vms")
echo "   vms on B: $VMS_B"
VM_B=$(echo "$VMS_B" | python3 -c 'import json,sys; vms=json.load(sys.stdin); print(vms[0]["id"] if vms else "")' 2>/dev/null)
echo "   VM on B: $VM_B"
if [[ -z "$VM_B" ]]; then
  echo "FAIL: no VM migrated to B"
  tail -20 "$WORK_A/serve.log"; echo "---"; tail -20 "$WORK_B/serve.log"
  sudo kill -9 $SRV_A $SRV_B 2>/dev/null
  exit 1
fi

# Exec on B to read the marker file
echo "→ exec cat /tmp/marker on B"
RES_B=$(curl -s -X POST "http://127.0.0.1:$PORT_B/vms/$VM_B/exec" \
  -H "Content-Type: application/json" \
  -d '{"command":["cat","/tmp/marker"]}')
echo "   $RES_B"
OK=0
if echo "$RES_B" | grep -q "$MARKER"; then OK=1; fi

# Cleanup
curl -s -X POST "http://127.0.0.1:$PORT_B/vms/$VM_B/stop" -d '{}' >/dev/null
sudo kill -9 $SRV_A $SRV_B 2>/dev/null
wait 2>/dev/null

if [[ "$OK" == "1" ]]; then
  echo "PASS migrate: state preserved, marker '$MARKER' visible on B"
else
  echo "FAIL migrate: marker not visible on B"
  exit 1
fi
