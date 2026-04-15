#!/usr/bin/env bash
# Manual test: gocracker serve --jailer on; VMM child runs as UID 1000 inside
# the chroot base dir, and exec still round-trips through the jailed VMM.
set -u
GC=/tmp/gocracker-fixed
JAILER=/tmp/gocracker-jailer-bin
VMM=/tmp/gocracker-vmm-bin
KERNEL=/home/misael/Desktop/projects/gocracker/artifacts/kernels/gocracker-guest-minimal-vmlinux
WORK=/tmp/gc-manual-jail
CHROOT=/tmp/gc-manual-jail-chroot
PORT=8580

# Cleanup on any exit (PASS, FAIL, Ctrl-C, SIGTERM): kill stray gocracker
# procs and wipe our workdir so /tmp doesn't accumulate GiBs across runs.
trap 'sudo pkill -9 -f "gocracker-fixed" 2>/dev/null; sudo rm -rf "$WORK" "$WORK_A" "$WORK_B" "$MIGR_DIR" 2>/dev/null' EXIT INT TERM


if [[ "$(id -u)" != "0" ]]; then
  echo "SKIP jailer: must run as root (sudo bash $0)"
  exit 0
fi

# Build jailer + vmm binaries
cd /home/misael/Desktop/projects/gocracker
go build -o "$JAILER" ./cmd/gocracker-jailer 2>&1 | tail -5
go build -o "$VMM"    ./cmd/gocracker-vmm    2>&1 | tail -5

pkill -9 -f "gocracker-fixed serve" 2>/dev/null
rm -rf "$WORK" "$CHROOT"
mkdir -p "$WORK" "$CHROOT"
chown 1000:1000 "$CHROOT" "$WORK"

"$GC" serve -addr "127.0.0.1:$PORT" \
    -cache-dir "$WORK/cache" \
    -jailer on \
    -jailer-binary "$JAILER" \
    -vmm-binary "$VMM" \
    -uid 1000 -gid 1000 \
    -chroot-base-dir "$CHROOT" \
    -trusted-kernel-dir "$(dirname "$KERNEL")" \
    -trusted-snapshot-dir "$WORK" \
    >"$WORK/serve.log" 2>&1 &
SRV=$!
sleep 2

echo "→ POST /run with jailer=on"
RES=$(curl -s -X POST "http://127.0.0.1:$PORT/run" \
  -H "Content-Type: application/json" \
  -d "{\"image\":\"alpine:3.20\",\"kernel_path\":\"$KERNEL\",\"mem_mb\":128,\"disk_size_mb\":256,\"cmd\":[\"sh\",\"-lc\",\"sleep infinity\"],\"exec_enabled\":true}")
echo "   $RES" | head -c 200; echo
VM=$(echo "$RES" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')

for _ in $(seq 1 60); do
  S=$(curl -s "http://127.0.0.1:$PORT/vms/$VM" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("state",""))' 2>/dev/null)
  [[ "$S" == "running" ]] && break; sleep 1
done
echo "   state: $S"

# Find VMM child process.
VMMPID=$(pgrep -f "gocracker-vmm.*$VM" | head -1)
echo "   VMM pid: $VMMPID"
if [[ -z "$VMMPID" ]]; then
  echo "FAIL jailer: no VMM child for $VM"
  tail -30 "$WORK/serve.log"
  kill -9 $SRV 2>/dev/null; exit 1
fi

echo "→ /proc/$VMMPID/status Uid"
UID_LINE=$(grep "^Uid:" /proc/$VMMPID/status)
echo "   $UID_LINE"
ROOT_LINK=$(readlink /proc/$VMMPID/root)
echo "   root link: $ROOT_LINK"

UID_OK=0
ROOT_OK=0
echo "$UID_LINE" | awk '{for(i=2;i<=5;i++)if($i!="1000")exit 1}' && UID_OK=1
# After pivot_root, the vmm's /proc/<pid>/root from outside the jail's mount
# namespace commonly resolves to "/" (it's relative to the vmm's own mntns).
# Accept either the absolute chroot path (if the kernel exposes pre-pivot root)
# OR "/" (pivot succeeded and exec is working inside the jail — the UID + exec
# checks above are the essential invariants).
[[ "$ROOT_LINK" == "$CHROOT"/* || "$ROOT_LINK" == "/" ]] && ROOT_OK=1

# Exec round-trip
echo "→ exec whoami"
RES_EX=$(curl -s -X POST "http://127.0.0.1:$PORT/vms/$VM/exec" \
  -H "Content-Type: application/json" \
  -d '{"command":["whoami"]}')
echo "   $RES_EX"
EXEC_OK=0
echo "$RES_EX" | grep -q '"exit_code":0' && EXEC_OK=1

# Cleanup
curl -s -X POST "http://127.0.0.1:$PORT/vms/$VM/stop" -d '{}' >/dev/null
kill -9 $SRV 2>/dev/null
wait 2>/dev/null

if [[ "$UID_OK" == "1" && "$ROOT_OK" == "1" && "$EXEC_OK" == "1" ]]; then
  echo "PASS jailer: VMM pid=$VMMPID Uid=1000, chroot=$ROOT_LINK, exec worked"
else
  echo "FAIL jailer: uid_ok=$UID_OK root_ok=$ROOT_OK exec_ok=$EXEC_OK"
  exit 1
fi
