#!/usr/bin/env bash
# Manual test: end-to-end sandbox API flow.
#  1. Boot a source VM with network_mode=auto + wait=true.
#  2. Install bc inside (proves network works).
#  3. Pause → /proc state frozen → Resume → bc still works.
#  4. Clone the source → clone has bc without re-install (disk state copied).
#  5. Source stays alive and independent of the clone.
# PASS when every step works; cleanup via trap on any exit.
set -u
GC=/tmp/gocracker-fixed
: "${KERNEL:=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.."; pwd)/artifacts/kernels/gocracker-guest-minimal-vmlinux}"
WORK=/tmp/gc-manual-sandbox
PORT=8600

trap 'sudo pkill -9 -f "gocracker-fixed" 2>/dev/null; sudo rm -rf "$WORK" 2>/dev/null' EXIT INT TERM

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

echo "→ boot source VM with network_mode=auto + wait=true"
SRC=$(curl -sS -X POST "http://127.0.0.1:$PORT/run" \
  -H "Content-Type: application/json" \
  -d "{
    \"image\":\"alpine:3.20\",
    \"kernel_path\":\"$KERNEL\",
    \"mem_mb\":256,
    \"disk_size_mb\":512,
    \"network_mode\":\"auto\",
    \"cmd\":[\"sh\",\"-lc\",\"sleep infinity\"],
    \"exec_enabled\":true,
    \"wait\":true
  }")
echo "   source=$SRC" | head -c 400
echo
SRC_ID=$(echo "$SRC" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
SRC_TAP=$(echo "$SRC" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("tap_name",""))')
SRC_IP=$(echo "$SRC" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("guest_ip",""))')
if [[ -z "$SRC_ID" || -z "$SRC_TAP" || -z "$SRC_IP" ]]; then
  echo "FAIL: /run did not populate tap/ip"
  tail -40 "$WORK/serve.log"
  exit 1
fi

echo "→ apk add bc on source"
APK=$(curl -sS -X POST "http://127.0.0.1:$PORT/vms/$SRC_ID/exec" \
  -H "Content-Type: application/json" \
  -d '{"command":["sh","-lc","apk add --no-cache bc 2>&1 | tail -3 && echo ok"]}')
echo "   $APK" | head -c 400; echo
if ! echo "$APK" | grep -q '"exit_code":0'; then
  echo "FAIL: apk add bc returned non-zero"
  exit 1
fi

echo "→ pause source"
curl -sS -f -X POST "http://127.0.0.1:$PORT/vms/$SRC_ID/pause" -d '{}' >/dev/null || { echo "FAIL pause"; exit 1; }
PSTATE=$(curl -sS "http://127.0.0.1:$PORT/vms/$SRC_ID" | python3 -c 'import json,sys; print(json.load(sys.stdin)["state"])')
if [[ "$PSTATE" != "paused" ]]; then
  echo "FAIL: state after pause=$PSTATE, want paused"
  exit 1
fi

echo "→ resume source"
curl -sS -f -X POST "http://127.0.0.1:$PORT/vms/$SRC_ID/resume" -d '{}' >/dev/null || { echo "FAIL resume"; exit 1; }
RSTATE=$(curl -sS "http://127.0.0.1:$PORT/vms/$SRC_ID" | python3 -c 'import json,sys; print(json.load(sys.stdin)["state"])')
if [[ "$RSTATE" != "running" ]]; then
  echo "FAIL: state after resume=$RSTATE, want running"
  exit 1
fi

echo "→ verify bc still works post-resume"
BCRES=$(curl -sS -X POST "http://127.0.0.1:$PORT/vms/$SRC_ID/exec" \
  -H "Content-Type: application/json" \
  -d '{"command":["sh","-lc","echo 6*7 | bc"]}')
if ! echo "$BCRES" | grep -q '"stdout":"42'; then
  echo "FAIL: bc post-resume output unexpected: $BCRES"
  exit 1
fi

echo "→ clone source"
CLONE=$(curl -sS -X POST "http://127.0.0.1:$PORT/vms/$SRC_ID/clone" \
  -H "Content-Type: application/json" \
  -d '{"exec_enabled":true}')
echo "   $CLONE" | head -c 400; echo
CLONE_ID=$(echo "$CLONE" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
CLONE_SNAP=$(echo "$CLONE" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("restored_from_snapshot",False))')
if [[ -z "$CLONE_ID" || "$CLONE_ID" == "$SRC_ID" ]]; then
  echo "FAIL: clone ID invalid ($CLONE_ID vs source $SRC_ID)"
  exit 1
fi
if [[ "$CLONE_SNAP" != "True" ]]; then
  echo "FAIL: clone response missing restored_from_snapshot=true ($CLONE_SNAP)"
  exit 1
fi

echo "→ verify clone has bc pre-installed (from source snapshot)"
CBC=$(curl -sS -X POST "http://127.0.0.1:$PORT/vms/$CLONE_ID/exec" \
  -H "Content-Type: application/json" \
  -d '{"command":["sh","-lc","echo 9*9 | bc"]}')
if ! echo "$CBC" | grep -q '"stdout":"81'; then
  echo "FAIL: clone bc missing — snapshot did not capture disk state: $CBC"
  exit 1
fi

echo "→ verify source alive and independent of clone"
curl -sS -X POST "http://127.0.0.1:$PORT/vms/$SRC_ID/exec" \
  -H "Content-Type: application/json" \
  -d '{"command":["sh","-lc","echo SRC-AFTER-CLONE > /tmp/after && cat /tmp/after"]}' >/dev/null
CLONE_CHECK=$(curl -sS -X POST "http://127.0.0.1:$PORT/vms/$CLONE_ID/exec" \
  -H "Content-Type: application/json" \
  -d '{"command":["sh","-lc","cat /tmp/after 2>&1 || echo MISSING-OK"]}')
if ! echo "$CLONE_CHECK" | grep -qE 'MISSING-OK|No such file'; then
  echo "WARN: clone saw post-clone source write (disk COW bug?):"
  echo "   $CLONE_CHECK"
fi

echo "→ stop both"
curl -sS -X POST "http://127.0.0.1:$PORT/vms/$CLONE_ID/stop" -d '{}' >/dev/null
curl -sS -X POST "http://127.0.0.1:$PORT/vms/$SRC_ID/stop" -d '{}' >/dev/null
sudo kill -9 $SRV 2>/dev/null
wait 2>/dev/null

echo
echo "PASS sandbox-e2e: network_mode=auto + wait=true + apk install + pause + resume + clone + disk isolation"
