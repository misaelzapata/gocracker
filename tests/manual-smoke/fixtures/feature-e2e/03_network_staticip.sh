#!/usr/bin/env bash
# Manual test: static IP + outbound curl from guest to a host HTTP server.
# PASS when: guest's eth0 gets the requested IP, and curl from guest reaches
# an HTTP server the host spins up on the gateway IP.
set -u
GC=/tmp/gocracker-fixed
KERNEL=/home/misael/Desktop/projects/gocracker/artifacts/kernels/gocracker-guest-minimal-vmlinux
WORK=/tmp/gc-manual-net
PORT=8550
TAP=gcmantap0
GUEST_IP="10.45.0.2"
HOST_IP="10.45.0.1"

# Cleanup on any exit (PASS, FAIL, Ctrl-C, SIGTERM): kill stray gocracker
# procs and wipe our workdir so /tmp doesn't accumulate GiBs across runs.
trap 'sudo pkill -9 -f "gocracker-fixed" 2>/dev/null; sudo rm -rf "$WORK" "$WORK_A" "$WORK_B" "$MIGR_DIR" 2>/dev/null' EXIT INT TERM


sudo pkill -9 -f "gocracker-fixed serve" 2>/dev/null
sudo ip link del "$TAP" 2>/dev/null
sudo rm -rf "$WORK"
mkdir -p "$WORK"

# Create tap owned by the invoker's uid, plug it with host-side address.
sudo ip tuntap add mode tap dev "$TAP" user "$(id -u)"
sudo ip addr add "$HOST_IP/24" dev "$TAP"
sudo ip link set "$TAP" up

# Start a trivial HTTP server on the host IP.
python3 -c "
from http.server import BaseHTTPRequestHandler, HTTPServer
class H(BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200); self.send_header('Content-Length','13'); self.end_headers()
        self.wfile.write(b'manual-net-ok')
    def log_message(self,*a,**k): pass
HTTPServer(('$HOST_IP',8090), H).serve_forever()
" &
HTTPD=$!
sleep 1

# Start serve
sudo -E "$GC" serve -addr "127.0.0.1:$PORT" -cache-dir "$WORK/cache" \
    -jailer off \
    -trusted-kernel-dir "$(dirname "$KERNEL")" \
    -trusted-snapshot-dir "$WORK" \
    >"$WORK/serve.log" 2>&1 &
SRV=$!
sleep 2

echo "→ run alpine VM with static IP $GUEST_IP via tap $TAP"
RES=$(curl -s -X POST "http://127.0.0.1:$PORT/run" \
  -H "Content-Type: application/json" \
  -d "{\"image\":\"alpine:3.20\",\"kernel_path\":\"$KERNEL\",\"mem_mb\":128,\"disk_size_mb\":256,\"cmd\":[\"sh\",\"-lc\",\"sleep infinity\"],\"exec_enabled\":true,\"static_ip\":\"$GUEST_IP/24\",\"gateway\":\"$HOST_IP\",\"tap_name\":\"$TAP\"}")
echo "   $RES" | head -c 200; echo
VM=$(echo "$RES" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')

# Wait running
for _ in $(seq 1 60); do
  S=$(curl -s "http://127.0.0.1:$PORT/vms/$VM" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("state",""))' 2>/dev/null)
  [[ "$S" == "running" ]] && break
  sleep 1
done
echo "   state: $S"

# 1. verify guest has the right IP
echo "→ ip -4 addr show eth0"
curl -s -X POST "http://127.0.0.1:$PORT/vms/$VM/exec" \
  -H "Content-Type: application/json" \
  -d '{"command":["ip","-4","addr","show","eth0"]}' | tee "$WORK/ipshow.json" | head -c 400; echo
grep -q "$GUEST_IP" "$WORK/ipshow.json" && IP_OK=1 || IP_OK=0

# 2. curl host HTTP server from guest
echo "→ wget http://$HOST_IP:8090/ from guest"
curl -s -X POST "http://127.0.0.1:$PORT/vms/$VM/exec" \
  -H "Content-Type: application/json" \
  -d "{\"command\":[\"wget\",\"-qO-\",\"http://$HOST_IP:8090/\"]}" | tee "$WORK/wget.json" | head -c 300; echo
grep -q "manual-net-ok" "$WORK/wget.json" && OUT_OK=1 || OUT_OK=0

# Cleanup
curl -s -X POST "http://127.0.0.1:$PORT/vms/$VM/stop" -d '{}' >/dev/null
sudo kill -9 $SRV $HTTPD 2>/dev/null
sudo ip link del "$TAP" 2>/dev/null
wait 2>/dev/null

if [[ "$IP_OK" == "1" && "$OUT_OK" == "1" ]]; then
  echo "PASS network: static IP assigned AND guest → host HTTP reachable"
else
  echo "FAIL network: ip_ok=$IP_OK outbound_ok=$OUT_OK"
  exit 1
fi
