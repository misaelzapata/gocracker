# Compose

gocracker reads standard `docker-compose.yml` files and boots each service as
its own microVM. Your existing compose files work without modification.

## Overview

Each service in the compose file becomes a separate VM with its own kernel,
disk image, and network interface. Services communicate over an isolated
virtual network where they can reach each other by name.

## Supported Features

| Feature | Supported | Notes |
|---------|-----------|-------|
| `services` | Yes | Each service becomes a VM |
| `image` | Yes | Any OCI image reference |
| `build` | Yes | Dockerfile builds (context, dockerfile, args) |
| `command` | Yes | Overrides the image CMD |
| `entrypoint` | Yes | Overrides the image ENTRYPOINT |
| `environment` | Yes | Passed to the guest init |
| `ports` | Yes | Host-to-container forwarding via userspace proxy |
| `depends_on` | Yes | With conditions: `service_started`, `service_healthy`, `service_completed_successfully` |
| `healthcheck` | Yes | CMD, CMD-SHELL, interval, timeout, retries |
| `volumes` (bind) | Yes | Host paths materialized into the guest disk or live via virtio-fs |
| `working_dir` | Yes | Sets the guest working directory |
| `mem_limit` | Yes | Converted to VM memory size |
| `networks` | Partial | Default bridge only; custom drivers ignored |
| `deploy` | No | Swarm/Kubernetes deploy config not applicable |
| `configs` / `secrets` | No | Use environment variables instead |
| `named volumes` | No | Use bind mounts |

## Full Example: Flask + PostgreSQL

This example runs a Python Flask API backed by PostgreSQL. Both services run
as separate VMs.

### docker-compose.yml

```yaml
version: "3.9"
services:
  postgres:
    image: postgres:16-alpine
    environment:
      POSTGRES_DB: todos
      POSTGRES_USER: todos
      POSTGRES_PASSWORD: todos
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U todos -d todos"]
      interval: 1s
      timeout: 3s
      retries: 20

  app:
    build:
      context: .
    depends_on:
      postgres:
        condition: service_healthy
    environment:
      DATABASE_URL: postgresql://todos:todos@postgres:5432/todos
    ports:
      - "18081:8080"
```

### Dockerfile

```dockerfile
FROM python:3.12-slim

ENV PYTHONDONTWRITEBYTECODE=1
ENV PYTHONUNBUFFERED=1

WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt
COPY app.py .

CMD ["python", "app.py"]
```

### requirements.txt

```
Flask==3.0.3
psycopg[binary]==3.2.3
```

## Running

```bash
sudo ./gocracker compose \
  --file docker-compose.yml \
  --kernel ./artifacts/kernels/gocracker-guest-standard-vmlinux \
  --wait
```

gocracker will:

1. Parse the compose file (using the `compose-go` library for full compatibility).
2. Sort services by dependency order.
3. Allocate a /24 subnet from `198.18.0.0/15` and create an isolated network
   namespace with a bridge.
4. Start `postgres` first (no dependencies).
5. Run the healthcheck (`pg_isready`) inside the Postgres VM via the exec agent.
6. Once Postgres is healthy, build and start `app`.
7. Forward host port `18081` to the app VM's port `8080`.

The output shows a status table:

```
SERVICE              STATE      IP              TAP        PORTS
postgres             running    198.18.42.2     gct-pg     -
app                  running    198.18.42.3     gct-app    18081:8080
```

Press `Ctrl-C` to stop all VMs and clean up the network namespace.

### Additional Compose Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--file` | `docker-compose.yml` | Path to the compose file |
| `--kernel` | (required) | Guest kernel for all services |
| `--mem` | `256` | Default RAM per service (MiB) |
| `--disk` | `4096` | Default disk per service (MiB) |
| `--tap-prefix` | `gc` | Prefix for auto-generated TAP names |
| `--cache-dir` | `/tmp/gocracker/cache` | Persistent artifact cache |
| `--snapshot` | | Snapshot directory (restore or save) |
| `--save-snapshot` | `false` | Save snapshots on stop |
| `--jailer` | `on` | Privilege model: `on` or `off` |
| `--wait` | `false` | Block until all VMs stop |

## Networking

Each compose stack gets its own network namespace (`gcns-<project>`). Inside
it, a bridge connects all service TAP devices. A veth pair links the namespace
to the host for port forwarding.

- **Service DNS**: Services resolve each other by name. The guest init writes
  `/etc/hosts` entries mapping each service name to its assigned IP.

- **Port publishing**: The `ports` directive (`"host:container"`) is handled
  by a userspace TCP/UDP proxy on the host side. No iptables DNAT is needed
  for compose networking.

- **Isolation**: Services in different compose stacks cannot reach each other.
  Each stack has its own namespace and subnet.

## Health Checks

Health checks run inside the guest VM via the exec agent (virtio-vsock).

Supported formats:

```yaml
healthcheck:
  test: ["CMD", "pg_isready", "-U", "todos"]
  interval: 5s
  timeout: 3s
  retries: 10
```

```yaml
healthcheck:
  test: ["CMD-SHELL", "curl -f http://localhost:8080/health || exit 1"]
  interval: 10s
  timeout: 5s
  retries: 5
```

Parameters:

- **interval**: Time between checks (default: 30s).
- **timeout**: Maximum time for a single check (default: 30s).
- **retries**: Number of consecutive failures before unhealthy (default: 3).
- **start_period**: Grace period before checks count as failures.

Health checks from the image's `HEALTHCHECK` instruction are also honored if
the compose file does not override them.

## depends_on Conditions

```yaml
depends_on:
  db:
    condition: service_healthy     # wait for healthcheck to pass
  cache:
    condition: service_started     # wait for VM to start (default)
  migrations:
    condition: service_completed_successfully  # wait for VM to exit 0
```

gocracker starts services in topological order. When a dependency has a
condition, the orchestrator blocks until that condition is met before starting
the dependent service.

## Limitations

- **No custom network drivers.** Only the default bridge is supported. Custom
  network configurations in the compose file are parsed but the driver setting
  is ignored.

- **No `deploy:` section.** Swarm and Kubernetes deploy options (replicas,
  placement, resources) are not applicable to microVMs.

- **No `configs:` or `secrets:`.** Use `environment:` variables or bind-mount
  the files you need.

- **No named volumes.** Use bind mounts (`./data:/data`) instead. The host
  path is materialized into the guest ext4 image at build time, or served
  live via virtio-fs.

- **Same-architecture only.** All services in a stack run on the host
  architecture. Cross-arch emulation is not supported.

---

## More Documentation

- [Getting Started](GETTING_STARTED.md) | [Networking](NETWORKING.md) | [Architecture](ARCHITECTURE.md) | [Compose](COMPOSE.md)
- [API Reference](API.md) | [CLI Reference](CLI_REFERENCE.md) | [Snapshots](SNAPSHOTS.md)
- [Examples](EXAMPLES.md) | [Validated Projects](VALIDATED_PROJECTS.md) | [Troubleshooting](TROUBLESHOOTING.md)
- [Security Policy](../SECURITY.md)

