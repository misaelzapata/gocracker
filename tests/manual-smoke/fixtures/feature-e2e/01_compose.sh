#!/usr/bin/env bash
# Manual test: compose multi-VM boot + exec.
# PASS when: two VMs appear, exec in each returns output.
set -u
GC=/tmp/gocracker-fixed
: "${KERNEL:=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.."; pwd)/artifacts/kernels/gocracker-guest-minimal-vmlinux}"
WORK=/tmp/gc-manual-compose
PORT=8530

# Cleanup on any exit (PASS, FAIL, Ctrl-C, SIGTERM): kill stray gocracker
# procs and wipe our workdir so /tmp doesn't accumulate GiBs across runs.
trap 'sudo pkill -9 -f "gocracker-fixed" 2>/dev/null; sudo rm -rf "$WORK" "$WORK_A" "$WORK_B" "$MIGR_DIR" 2>/dev/null' EXIT INT TERM


sudo pkill -9 -f "gocracker-fixed serve" 2>/dev/null
sudo rm -rf "$WORK"
mkdir -p "$WORK"

cat > "$WORK/docker-compose.yml" <<'EOF'
services:
  web:
    image: nginx:alpine
    command: ["nginx", "-g", "daemon off;"]
  client:
    image: alpine:3.20
    command: ["sh", "-c", "sleep infinity"]
EOF

# Launch serve
sudo -E "$GC" serve -addr "127.0.0.1:$PORT" -cache-dir "$WORK/cache" \
    -jailer off \
    -trusted-kernel-dir "$(dirname "$KERNEL")" \
    -trusted-snapshot-dir "$WORK" \
    -trusted-work-dir "$WORK" \
    >"$WORK/serve.log" 2>&1 &
SRV=$!
sleep 2

echo "→ gocracker compose (against API $PORT)"
sudo -E "$GC" compose \
    -file "$WORK/docker-compose.yml" \
    -server "http://127.0.0.1:$PORT" \
    -kernel "$KERNEL" \
    -jailer off \
    >"$WORK/compose.log" 2>&1 &
CMP=$!

# Wait up to 120s for 2 running VMs
for _ in $(seq 1 120); do
  VMS=$(curl -s "http://127.0.0.1:$PORT/vms" | python3 -c 'import json,sys; d=json.load(sys.stdin); print(" ".join(v["id"]+":"+v["state"] for v in d))' 2>/dev/null)
  RUNNING=$(echo "$VMS" | tr ' ' '\n' | grep -c ":running")
  if [[ "$RUNNING" -ge 2 ]]; then break; fi
  sleep 1
done
echo "VMs: $VMS"
if [[ "$RUNNING" -lt 2 ]]; then
  echo "FAIL: expected 2 running, got $RUNNING"
  tail -30 "$WORK/serve.log"
  sudo kill -9 $CMP $SRV 2>/dev/null
  exit 1
fi

# Exec hostname in each
IDS=$(curl -s "http://127.0.0.1:$PORT/vms" | python3 -c 'import json,sys; print(" ".join(v["id"] for v in json.load(sys.stdin)))')
for id in $IDS; do
  echo "→ exec hostname in $id"
  RES=$(curl -s -X POST "http://127.0.0.1:$PORT/vms/$id/exec" \
    -H "Content-Type: application/json" \
    -d '{"command":["hostname"]}')
  echo "   $RES"
  echo "$RES" | grep -q '"exit_code":0' || { echo "FAIL exec on $id"; sudo kill -9 $CMP $SRV 2>/dev/null; exit 1; }
done

# Cleanup
for id in $IDS; do curl -s -X POST "http://127.0.0.1:$PORT/vms/$id/stop" -d '{}' >/dev/null; done
sudo kill -9 $CMP $SRV 2>/dev/null
wait 2>/dev/null
echo "PASS compose: 2 VMs booted + exec round-tripped"
