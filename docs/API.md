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

Response: `{"id": "vm-abc123", "state": "running"}`

Request body fields: `image`, `dockerfile`, `context`, `vcpu_count`, `mem_mb`,
`arch`, `kernel_path`, `tap_name`, `x86_boot`, `cmd`, `entrypoint`, `env`,
`workdir`, `pid1_mode`, `build_args`, `disk_size_mb`, `mounts`, `drives`,
`snapshot_dir`, `static_ip`, `gateway`, `cache_dir`, `metadata`,
`exec_enabled`, `balloon`, `memory_hotplug`.

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
