#!/usr/bin/env bash
# Manual test: user-level vsock host→guest round-trip (not the exec broker).
# Guest listens on AF_VSOCK port 13000, host dials via gocracker's /vsock/connect,
# sends bytes, reads echo.
set -u
GC=/tmp/gocracker-fixed
: "${KERNEL:=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.."; pwd)/artifacts/kernels/gocracker-guest-minimal-vmlinux}"
WORK=/tmp/gc-manual-vsock
PORT=8590

# Cleanup on any exit (PASS, FAIL, Ctrl-C, SIGTERM): kill stray gocracker
# procs and wipe our workdir so /tmp doesn't accumulate GiBs across runs.
trap 'sudo pkill -9 -f "gocracker-fixed" 2>/dev/null; sudo rm -rf "$WORK" "$WORK_A" "$WORK_B" "$MIGR_DIR" 2>/dev/null' EXIT INT TERM


sudo pkill -9 -f "gocracker-fixed serve" 2>/dev/null
sudo rm -rf "$WORK"; mkdir -p "$WORK"

# Build a static Go listener that binds AF_VSOCK:13000 and echoes "echo:<line>".
cat > "$WORK/listener.go" <<'EOF'
package main

import (
	"bufio"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func main() {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
	if err != nil { fmt.Fprintln(os.Stderr, "socket:", err); os.Exit(1) }
	if err := unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: 13000}); err != nil {
		fmt.Fprintln(os.Stderr, "bind:", err); os.Exit(1)
	}
	if err := unix.Listen(fd, 1); err != nil { fmt.Fprintln(os.Stderr, "listen:", err); os.Exit(1) }
	fmt.Println("listener-ready")
	for {
		cfd, _, err := unix.Accept(fd)
		if err != nil { continue }
		f := os.NewFile(uintptr(cfd), "vsock-client")
		go func() {
			defer f.Close()
			r := bufio.NewScanner(f)
			for r.Scan() {
				fmt.Fprintf(f, "echo:%s\n", r.Text())
			}
		}()
	}
}
EOF
(cd "$WORK" && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -mod=mod -o listener listener.go 2>&1) | tail -5

cat > "$WORK/Dockerfile" <<'EOF'
FROM alpine:3.20
COPY listener /usr/local/bin/listener
CMD ["/usr/local/bin/listener"]
EOF

# Module boilerplate so go build works against the vendored unix
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.."; pwd)"
(cd "$WORK" && cp "$REPO/go.mod" . && cp "$REPO/go.sum" . 2>/dev/null)
(cd "$WORK" && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o listener listener.go 2>&1) | tail -5

sudo -E "$GC" serve -addr "127.0.0.1:$PORT" -cache-dir "$WORK/cache" \
    -jailer off \
    -trusted-kernel-dir "$(dirname "$KERNEL")" \
    -trusted-snapshot-dir "$WORK" \
    -trusted-work-dir "$WORK" \
    >"$WORK/serve.log" 2>&1 &
SRV=$!
sleep 2

echo "→ run VM with our vsock listener as CMD"
RES=$(curl -s -X POST "http://127.0.0.1:$PORT/run" \
  -H "Content-Type: application/json" \
  -d "{\"dockerfile\":\"$WORK/Dockerfile\",\"context\":\"$WORK\",\"kernel_path\":\"$KERNEL\",\"mem_mb\":128,\"disk_size_mb\":256,\"exec_enabled\":true}")
echo "   $RES" | head -c 200; echo
VM=$(echo "$RES" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')

for _ in $(seq 1 60); do
  S=$(curl -s "http://127.0.0.1:$PORT/vms/$VM" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("state",""))' 2>/dev/null)
  [[ "$S" == "running" ]] && break; sleep 1
done
echo "   state: $S"

# Wait for "listener-ready" in the guest console log.
for _ in $(seq 1 30); do
  curl -s "http://127.0.0.1:$PORT/vms/$VM/logs" | grep -q "listener-ready" && break
  sleep 1
done

# Host dials via HTTP upgrade at /vms/{id}/vsock/connect
echo "→ host dial vsock port 13000"
python3 - <<PY
import http.client, json, socket, sys
conn = http.client.HTTPConnection("127.0.0.1", $PORT)
conn.request("GET", "/vms/$VM/vsock/connect?port=13000", headers={"Connection":"upgrade","Upgrade":"vsock"})
resp = conn.getresponse()
if resp.status != 101:
    print("FAIL upgrade status=", resp.status, resp.read()); sys.exit(2)
sock = conn.sock
sock.sendall(b"ping\n")
buf = b""
while b"\n" not in buf:
    chunk = sock.recv(4096)
    if not chunk: break
    buf += chunk
line = buf.decode().strip()
print("got:", line)
sys.exit(0 if line == "echo:ping" else 3)
PY
RC=$?

# Cleanup
curl -s -X POST "http://127.0.0.1:$PORT/vms/$VM/stop" -d '{}' >/dev/null
sudo kill -9 $SRV 2>/dev/null
wait 2>/dev/null

if [[ "$RC" == "0" ]]; then
  echo "PASS vsock-user: host → guest round-trip returned 'echo:ping'"
else
  echo "FAIL vsock-user: rc=$RC"
  exit 1
fi
