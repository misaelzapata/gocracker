# gocracker REST API

gocracker exposes a REST API that is wire-compatible with the Firecracker
pre-boot flow and adds extended endpoints for multi-VM management, OCI builds,
snapshots, exec, and live migration.

## Starting the API Server

```bash
sudo gocracker serve --addr :8080 --jailer off
```

Key flags:

| Flag | Default | Description |
|------|---------|-------------|
| `--addr` | (none; uses Unix socket) | TCP listen address (e.g. `:8080`) |
| `--sock` | `/tmp/gocracker.sock` | Unix socket path (used when `--addr` is empty) |
| `--auth-token` | `$GOCRACKER_API_TOKEN` | Bearer token for request authentication |
| `--x86-boot` | `auto` | Default x86 boot mode: `auto`, `acpi`, or `legacy` |
| `--jailer` | `on` | Privilege model: `on` or `off` |
| `--state-dir` | `/tmp/gocracker-serve-state` | Persistent supervisor state directory |
| `--cache-dir` | `/tmp/gocracker/cache` | Persistent build/OCI cache directory |
| `--uid` / `--gid` | caller's UID/GID | UID and GID for jailed workers |
| `--trusted-kernel-dir` | (auto) | Trusted kernel directory (repeatable) |
| `--trusted-work-dir` | (auto) | Trusted workspace directory (repeatable) |
| `--trusted-snapshot-dir` | (auto) | Trusted snapshot directory (repeatable) |

When `--addr` is not a loopback address, `--auth-token` is required.

---

## Authentication

When `--auth-token` is set (or `GOCRACKER_API_TOKEN` is exported), every
request must include the header:

```
Authorization: Bearer <token>
```

Requests without a valid token receive `401 Unauthorized`.

---

## Firecracker-Compatible Pre-boot Flow

These endpoints configure a single VM before starting it, matching the
Firecracker API contract.

### 1. Set boot source

```bash
curl -s --unix-socket /tmp/gocracker.sock -X PUT http://localhost/boot-source \
  -d '{
    "kernel_image_path": "/path/to/vmlinux",
    "boot_args": "console=ttyS0 reboot=k panic=1",
    "initrd_path": "/path/to/initrd"
  }'
```

### 2. Set machine config

```bash
curl -s --unix-socket /tmp/gocracker.sock -X PUT http://localhost/machine-config \
  -d '{"vcpu_count": 2, "mem_size_mib": 512}'
```

### 3. Attach drives

```bash
curl -s --unix-socket /tmp/gocracker.sock -X PUT http://localhost/drives/rootfs \
  -d '{
    "drive_id": "rootfs",
    "path_on_host": "/path/to/rootfs.ext4",
    "is_root_device": true,
    "is_read_only": false
  }'
```

### 4. Attach network interfaces

```bash
curl -s --unix-socket /tmp/gocracker.sock -X PUT http://localhost/network-interfaces/eth0 \
  -d '{
    "iface_id": "eth0",
    "host_dev_name": "tap0",
    "guest_mac": "AA:FC:00:00:00:01"
  }'
```

### 5. Start the VM

```bash
curl -s --unix-socket /tmp/gocracker.sock -X PUT http://localhost/actions \
  -d '{"action_type": "InstanceStart"}'
```

### Additional Firecracker-compatible endpoints

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/` | Instance info (app name, version, state) |
| `GET` | `/balloon` | Get balloon config |
| `PUT` | `/balloon` | Set balloon config |
| `PATCH` | `/balloon` | Update balloon target |
| `GET` | `/balloon/statistics` | Get balloon statistics |
| `PATCH` | `/balloon/statistics` | Update stats polling interval |
| `GET` | `/hotplug/memory` | Get memory hotplug status |
| `PUT` | `/hotplug/memory` | Configure memory hotplug |
| `PATCH` | `/hotplug/memory` | Update hotplug memory size |

---

## Extended Endpoints

### POST /run -- One-shot build and boot

Build an OCI image or Dockerfile and boot it as a VM in one call.

```bash
curl -s http://localhost:8080/run -X POST \
  -H "Authorization: Bearer $TOKEN" \
  -d '{
    "image": "ubuntu:22.04",
    "kernel_path": "/path/to/vmlinux",
    "vcpu_count": 1,
    "mem_mb": 256,
    "disk_size_mb": 2048
  }'
```

Response (sync path, `wait=true`):

```json
{
  "id": "vm-abc123",
  "state": "running",
  "message": "VM is running",
  "tap_name": "gct-gc-12345",
  "guest_ip": "198.18.0.2",
  "gateway": "198.18.0.1",
  "network_mode": "auto",
  "restored_from_snapshot": false
}
```

Async path (default) returns only `id`, `state="starting"`, and `network_mode`
echoed back; poll `GET /vms/{id}` for the allocated tap/ip/gateway.

Request body fields: `image`, `dockerfile`, `context`, `vcpu_count`, `mem_mb`,
`arch`, `kernel_path`, `tap_name`, `x86_boot`, `cmd`, `entrypoint`, `env`,
`workdir`, `pid1_mode`, `build_args`, `disk_size_mb`, `mounts`, `drives`,
`snapshot_dir`, `static_ip`, `gateway`, `network_mode`, `cache_dir`,
`metadata`, `exec_enabled`, `balloon`, `memory_hotplug`, `wait`.

**Permission**: `network_mode=auto` requires the `gocracker serve` process
to hold root or `CAP_NET_ADMIN` (TAP devices cannot be created otherwise).
The server probes this at startup and logs a warning; `/run` and `/clone`
return **403** with an actionable message if the capability is missing.
Run with `sudo` or `setcap cap_net_admin+ep ./gocracker` to enable it, or
pre-create a TAP and pass explicit `tap_name` / `static_ip` / `gateway`.

**`network_mode`** selects how the guest NIC is provisioned:
- `""` / `"none"` — use `tap_name` / `static_ip` / `gateway` exactly as supplied
  (today's behaviour).
- `"auto"` — the server picks a free `/30` subnet via `hostnet.AutoNetwork`,
  brings up the tap, adds NAT, and returns the resolved tap/ip/gateway in the
  response. Mutually exclusive with an explicit `static_ip` or `gateway`.
  On snapshot-restore, `auto` is accepted but the guest's frozen IP wins —
  the tap is allocated fresh while the IP plan stays with the snapshot. See
  [SNAPSHOTS.md](SNAPSHOTS.md#sandbox-template-flow) for the template pattern
  that works end-to-end.

**`wait`** (bool; default `false`). When `true`, the handler blocks until the
VM reaches `state=running` and the response includes the resolved network
fields. For snapshot-restore this is typically single-digit ms; for a fresh
boot it blocks for the whole kernel+init duration.

### POST /build -- Build image only (no boot)

```bash
curl -s http://localhost:8080/build -X POST \
  -d '{"image": "python:3.12-slim", "disk_size_mb": 2048}'
```

### GET /vms -- List all VMs

```bash
curl -s http://localhost:8080/vms
```

Returns an array of `VMInfo` objects with `id`, `state`, `uptime`, `mem_mb`,
`kernel`, `events`, `devices`, and `metadata`.

### GET /vms/{id} -- VM details

```bash
curl -s http://localhost:8080/vms/vm-abc123
```

### POST /vms/{id}/stop -- Stop a VM

```bash
curl -s -X POST http://localhost:8080/vms/vm-abc123/stop
```

### POST /vms/{id}/pause -- Freeze vCPUs

Pauses the guest — the vCPUs stop executing but memory and open files stay.
Returns `204 No Content` on success, `400 Bad Request` if the VM is not in a
pausable state.

```bash
curl -s -X POST http://localhost:8080/vms/vm-abc123/pause -d '{}'
```

### POST /vms/{id}/resume -- Unfreeze vCPUs

Resumes a paused VM. `204 No Content` on success.

```bash
curl -s -X POST http://localhost:8080/vms/vm-abc123/resume -d '{}'
```

### POST /vms/{id}/clone -- In-place snapshot+restore

Snapshots the source VM and restores it as a new VM on the same server in one
atomic call. The source stays running. The clone gets a fresh ID, a
server-minted `tclone-<N>` tap (unique per clone), and, when
`network_mode=auto` and `exec_enabled=true`, eth0 is re-addressed inside
the guest post-restore so the clone has working outbound networking. Useful
for sandbox-pool warm-starts without standing up a second `gocracker serve`.

```bash
curl -s -X POST http://localhost:8080/vms/vm-abc123/clone \
  -H 'Content-Type: application/json' \
  -d '{
    "exec_enabled": true,
    "network_mode": "auto"
  }'
```

Response:

```json
{
  "id": "gc-49501",
  "state": "running",
  "message": "cloned from vm-abc123",
  "tap_name": "tclone-49501",
  "guest_ip": "198.18.137.2",
  "gateway": "198.18.137.1",
  "network_mode": "auto",
  "restored_from_snapshot": true
}
```

Fields: `snapshot_dir` (optional; if set, the snapshot persists at that
path and can later be restored via `/run snapshot_dir=…`), `tap_name` /
`static_ip` / `gateway` (explicit network override, mutually exclusive with
`network_mode=auto`), `network_mode` (`"auto"` requires `exec_enabled=true`
so the clone can re-IP), `exec_enabled`, `metadata`. Metadata key
`cloned_from` on the new VM points back at the source.

**Virtio-fs mounts on the source**: `/clone` returns 400 if the source VM
holds live virtio-fs mounts (`shared_fs` entries). The Linux virtio-fs
driver's in-flight queue state cannot currently be migrated to a fresh
virtiofsd, so the endpoint refuses rather than silently hang. Workarounds:
umount the virtio-fs target on the source before cloning, OR use virtio-blk
(`drives`) for per-sandbox state instead of virtio-fs.

### POST /vms/{id}/snapshot -- Take snapshot

```bash
curl -s -X POST http://localhost:8080/vms/vm-abc123/snapshot \
  -d '{"dest_dir": "/tmp/snap-abc"}'
```

### POST /vms/{id}/exec -- Execute command in guest

```bash
curl -s -X POST http://localhost:8080/vms/vm-abc123/exec \
  -d '{"command": ["uname", "-a"]}'
```

Response: `{"stdout": "Linux ...\n", "stderr": "", "exit_code": 0}`

Additional exec fields: `columns`, `rows`, `stdin`, `env`, `workdir`.

### POST /vms/{id}/exec/stream -- Interactive exec (WebSocket-upgraded)

Opens a bidirectional stream for interactive shell sessions.

### GET /vms/{id}/logs -- UART console output

```bash
curl -s http://localhost:8080/vms/vm-abc123/logs
```

Returns the 64 KB UART output ring buffer as plain text.

### GET /vms/{id}/events -- Event log (polling)

```bash
curl -s "http://localhost:8080/vms/vm-abc123/events?since=2026-04-09T00:00:00Z"
```

### GET /vms/{id}/events/stream -- SSE event stream

```bash
curl -sN http://localhost:8080/vms/vm-abc123/events/stream
```

Streams Server-Sent Events in real time. Each event is JSON with type and
timestamp.

### POST /vms/{id}/migrate -- Live-migrate VM

```bash
curl -s -X POST http://localhost:8080/vms/vm-abc123/migrate \
  -d '{"destination_url": "http://host-b:8080"}'
```

### Rate limiter updates

| Method | Path | Description |
|--------|------|-------------|
| `PUT` | `/vms/{id}/rate-limiters/net` | Update network rate limiter |
| `PUT` | `/vms/{id}/rate-limiters/block` | Update block device rate limiter |
| `PUT` | `/vms/{id}/rate-limiters/rng` | Update RNG rate limiter |

### GET /vms/{id}/vsock/connect -- Vsock WebSocket bridge

Upgrades the connection to a WebSocket bridged to the guest vsock.

### Migration orchestration endpoints

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/migrations/load` | Load a migration bundle on destination |
| `POST` | `/migrations/prepare` | Prepare pre-copy migration on source |
| `POST` | `/migrations/finalize` | Finalize migration (pause + ship delta) |
| `POST` | `/migrations/abort` | Abort an in-progress migration |

---

## More Documentation

- [Getting Started](GETTING_STARTED.md) | [Networking](NETWORKING.md) | [Architecture](ARCHITECTURE.md) | [Compose](COMPOSE.md)
- [API Reference](API.md) | [CLI Reference](CLI_REFERENCE.md) | [Snapshots](SNAPSHOTS.md)
- [Examples](EXAMPLES.md) | [Validated Projects](VALIDATED_PROJECTS.md) | [Troubleshooting](TROUBLESHOOTING.md)
- [How gocracker Fits In](COMPETITIVE_ANALYSIS.md) | [Security Policy](../SECURITY.md)

