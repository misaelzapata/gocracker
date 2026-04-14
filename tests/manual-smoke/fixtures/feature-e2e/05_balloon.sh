#!/usr/bin/env bash
# Manual test: balloon — boot 2 VMs, one with balloon.amount_mib=0, other with 128.
# PASS: the ballooned VM shows MemAvailable at least ~100 MiB lower than the control.
# (The runtime PATCH endpoint /balloon is Firecracker-preboot-root only and not
# wired for /run-created VMs — verified by hitting /vms/{id}/balloon → 404.)
set -u
GC=/tmp/gocracker-fixed
KERNEL=/home/misael/Desktop/projects/gocracker/artifacts/kernels/gocracker-guest-minimal-vmlinux
WORK=/tmp/gc-manual-balloon
PORT=8570

# Cleanup on any exit (PASS, FAIL, Ctrl-C, SIGTERM): kill stray gocracker
# procs and wipe our workdir so /tmp doesn't accumulate GiBs across runs.
trap 'sudo pkill -9 -f "gocracker-fixed" 2>/dev/null; sudo rm -rf "$WORK" "$WORK_A" "$WORK_B" "$MIGR_DIR" 2>/dev/null' EXIT INT TERM


sudo pkill -9 -f "gocracker-fixed serve" 2>/dev/null
sudo rm -rf "$WORK"; mkdir -p "$WORK"

sudo -E "$GC" serve -addr "127.0.0.1:$PORT" -cache-dir "$WORK/cache" \
    -jailer off \
    -trusted-kernel-dir "$(dirname "$KERNEL")" \
    -trusted-snapshot-dir "$WORK" >"$WORK/serve.log" 2>&1 &
SRV=$!
sleep 2

mem_of() {
  local amount=$1
  RES=$(curl -s -X POST "http://127.0.0.1:$PORT/run" \
    -H "Content-Type: application/json" \
    -d "{\"image\":\"alpine:3.20\",\"kernel_path\":\"$KERNEL\",\"mem_mb\":256,\"disk_size_mb\":256,\"cmd\":[\"sh\",\"-lc\",\"sleep infinity\"],\"exec_enabled\":true,\"balloon\":{\"amount_mib\":$amount,\"deflate_on_oom\":true}}")
  VM=$(echo "$RES" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
  for _ in $(seq 1 60); do
    S=$(curl -s "http://127.0.0.1:$PORT/vms/$VM" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("state",""))' 2>/dev/null)
    [[ "$S" == "running" ]] && break; sleep 1
  done
  sleep 2  # let balloon inflate settle
  AVAIL=$(curl -s -X POST "http://127.0.0.1:$PORT/vms/$VM/exec" \
    -H "Content-Type: application/json" \
    -d '{"command":["cat","/proc/meminfo"]}' | python3 -c "
import json,sys,re
j=json.load(sys.stdin)
m=re.search(r'MemAvailable:\s+(\d+)', j.get('stdout',''))
print(m.group(1) if m else '0')
")
  curl -s -X POST "http://127.0.0.1:$PORT/vms/$VM/stop" -d '{}' >/dev/null
  sleep 1
  echo "$AVAIL"
}

echo "→ VM with balloon=0 (control)"
CTRL=$(mem_of 0)
echo "   MemAvailable = ${CTRL} kB"

echo "→ VM with balloon=128 (inflated 128 MiB)"
INFL=$(mem_of 128)
echo "   MemAvailable = ${INFL} kB"

sudo kill -9 $SRV 2>/dev/null
wait 2>/dev/null

DELTA=$(( CTRL - INFL ))
echo "   Δ MemAvailable = ${DELTA} kB (expected ≥ 102400 for 128 MiB inflation)"
if [[ "$DELTA" -ge 102400 ]]; then
  echo "PASS balloon: inflation reduced MemAvailable by ${DELTA} kB"
else
  echo "FAIL balloon: delta only ${DELTA} kB"
  exit 1
fi
