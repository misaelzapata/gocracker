# Manual Smoke Pack

English version. For the original Spanish text, see
[`README-ES.md`](README-ES.md).

This folder provides a reproducible manual validation pack for `gocracker`
without depending on Docker Engine to run the VMs.

The scripts live in the repository, but logs and artifacts are written to:

```bash
/tmp/gocracker-manual-smoke/<timestamp>
```

## Prerequisites

- Linux with `/dev/kvm`
- `GOCRACKER_KERNEL` pointing to a valid guest kernel. Recommended:
  - `./tools/build-guest-kernel.sh`
  - `GOCRACKER_KERNEL=./artifacts/kernels/gocracker-guest-standard-vmlinux`
- `sudo -v` already run before launching the pack
- `debugfs`
- `script`
- `timeout`
- `curl`
- `ip`
- `pgrep`
- outbound network access for registry pulls

## Recommended Validation Flow

First run the automated baseline:

```bash
./tools/build-guest-kernel.sh
go test ./...
GOCRACKER_KERNEL=./artifacts/kernels/gocracker-guest-standard-vmlinux go test -tags integration ./tests/integration/...
```

Then run the manual matrix:

```bash
sudo -v
GOCRACKER_KERNEL=./artifacts/kernels/gocracker-guest-standard-vmlinux tests/manual-smoke/run_all.sh
```

The script:

- builds `./gocracker`
- validates prerequisites
- runs the matrix by group
- writes one log per case
- exits with code `0` only if everything passes

## Available Cases

- `images`: `alpine:latest`, `busybox:latest`, `ubuntu:22.04`, `nginx:latest`, plus an override with `python:3.11-slim`
- `dockerfiles`: `tests/examples/hello-world`, `tests/examples/static-site`, `tests/examples/python-api`
- `extras`: shell-form fixture and `USER` fixture
- `compose`: fixture with `ports`, fixture with `depends_on: service_healthy`, fixture with bind volume + sync-back, and TODO + PostgreSQL fixture
- `exec`: direct `compose exec` over `serve`, without keys or user setup

By default it runs everything. To limit the groups:

```bash
GOCRACKER_KERNEL=./artifacts/kernels/gocracker-guest-standard-vmlinux \
SMOKE_CASES=images,dockerfiles \
tests/manual-smoke/run_all.sh
```

Useful variables:

```bash
GOCRACKER_KERNEL=./artifacts/kernels/gocracker-guest-standard-vmlinux
GC_BIN=./gocracker
SMOKE_LOG_DIR=/tmp/gocracker-manual-smoke/custom-run
SMOKE_CASES=all
```

## Registry Auth

### Without login

The normal public matrix works without login. That is the default path.

### Optional login

Login is only needed for private images or if you hit registry rate limits.

`gocracker` does not need Docker Engine to run, but the easiest compatible way
to provide credentials is still writing a Docker-style `config.json`:

```bash
docker login
```

If you do not want to use `docker login`, you can populate
`~/.docker/config.json` manually with the registry credentials.

### Important note when using `sudo`

The smoke pack cases use `sudo ./gocracker ...`. That means if auth is needed,
the credentials must also exist for root.

The simplest way is:

```bash
sudo docker login
```

or place the file at:

```bash
/root/.docker/config.json
```

## Reviewing Failures

Each case leaves:

- serial log: `/tmp/gocracker-manual-smoke/<timestamp>/<case>.log`
- the real `disk.ext4` path reported by `gocracker run`

Logs are stored under:

```bash
/tmp/gocracker-manual-smoke/<timestamp>
```

The real disk path should no longer be assumed to live at
`/tmp/gocracker-<case-id>/disk.ext4`. Resolve it from the case log instead:

```bash
log=/tmp/gocracker-manual-smoke/<timestamp>/<case-id>.log
disk=$(grep -o '/tmp/gocracker-[^[:space:]]*/disk.ext4' "$log" | tail -n1)
echo "$disk"
```

To inspect a file inside the disk:

```bash
debugfs -R "cat /result.txt" "$disk"
debugfs -R "stat /work/runtime-user.txt" "$disk"
```

To quickly inspect a log:

```bash
tail -n 80 /tmp/gocracker-manual-smoke/<timestamp>/<case-id>.log
```

Extra fixtures live in:

- `tests/manual-smoke/fixtures/shellform/Dockerfile`
- `tests/manual-smoke/fixtures/user/Dockerfile`
- `tests/manual-smoke/fixtures/compose-basic/docker-compose.yml`
- `tests/manual-smoke/fixtures/compose-health/docker-compose.yml`
- `tests/manual-smoke/fixtures/compose-volume/docker-compose.yml`
- `tests/manual-smoke/fixtures/compose-todo-postgres/docker-compose.yml`
- `tests/manual-smoke/fixtures/compose-exec/docker-compose.yml`

## Compose Coverage

The `compose` group validates four practical scenarios:

- `compose-basic`: starts an HTTP service, publishes `18080:8080`, and verifies the response with `curl`
- `compose-health`: uses `depends_on.condition: service_healthy` and does not treat the stack as ready until the web service responds
- `compose-volume`: mounts `./data:/data`, shuts the stack down cleanly, and validates that the synced file made it back to the host
- `compose-todo-postgres`: starts `postgres` plus a TODO app, creates a real task over HTTP, and verifies that it can be read back from PostgreSQL

If you want to run only Compose:

```bash
sudo -v
GOCRACKER_KERNEL=./artifacts/kernels/gocracker-guest-standard-vmlinux \
SMOKE_CASES=compose \
tests/manual-smoke/run_all.sh
```

To test a real app + database case outside the full harness:

```bash
cd /home/misael/Desktop/projects/gocracker
go build -o ./gocracker ./cmd/gocracker
sudo -v

sudo env GOCRACKER_KERNEL=./artifacts/kernels/gocracker-guest-standard-vmlinux \
  ./gocracker compose \
  --file tests/manual-smoke/fixtures/compose-todo-postgres/docker-compose.yml \
  --kernel ./artifacts/kernels/gocracker-guest-standard-vmlinux \
  --mem 256 \
  --wait
```

In another terminal:

```bash
curl -fsS http://127.0.0.1:18081/health
curl -fsS -X POST -H 'Content-Type: application/json' \
  -d '{"title":"buy milk"}' \
  http://127.0.0.1:18081/api/todos
curl -fsS http://127.0.0.1:18081/api/todos
```

If the last request returns the created item, the app and PostgreSQL are
communicating correctly inside the Compose network.

New operational notes:

- Shared cache now lives by default in `/tmp/gocracker/cache`; if you repeat a normal run you should see `artifact cache hit`, `reusing cached disk`, and `reusing cached initrd` without extra setup.
- If you want the stack to remain visible in the API, start `serve` and use `compose --server http://127.0.0.1:8080`.
- In `compose --server`, the netns/TAP and published ports now live on the `serve` side; the client command can exit and the ports remain alive while the stack stays up.
- Compose healthchecks now run inside the guest through the exec agent, so `CMD` and `CMD-SHELL` can use container-local binaries and scripts instead of depending on host-side translations.
- `compose exec --server ...` only needs reachability to the API server; it does not need a guest IP, `ports: 22`, a user, or SSH keys.
- Real example with `compose --server` + `compose exec`:

```bash
./gocracker serve --addr :8080 --cache-dir /tmp/gocracker/cache

sudo ./gocracker compose \
  --server http://127.0.0.1:8080 \
  --file tests/manual-smoke/fixtures/compose-exec/docker-compose.yml \
  --kernel ./artifacts/kernels/gocracker-guest-standard-vmlinux \
  --cache-dir /tmp/gocracker/cache

curl 'http://127.0.0.1:8080/vms?orchestrator=compose&stack=compose-exec&service=debug'

./gocracker compose exec \
  --server http://127.0.0.1:8080 \
  --file tests/manual-smoke/fixtures/compose-exec/docker-compose.yml \
  debug -- echo compose-exec-ok
```

- Real example with `/run` + API exec:

```bash
./gocracker serve --addr :8080 --cache-dir /tmp/gocracker/cache

curl -fsS -X POST http://127.0.0.1:8080/run \
  -H 'Content-Type: application/json' \
  -d '{
    "image":"alpine:3.19",
    "kernel_path":"./artifacts/kernels/gocracker-guest-standard-vmlinux",
    "mem_mb":256,
    "cmd":["sleep","600"],
    "exec_enabled":true
  }'

curl -fsS -X POST http://127.0.0.1:8080/vms/<vm-id>/exec \
  -H 'Content-Type: application/json' \
  -d '{"command":["echo","api-exec-ok"]}'
```

To run only these manual checks:

```bash
sudo -v
GOCRACKER_KERNEL=./artifacts/kernels/gocracker-guest-standard-vmlinux \
SMOKE_CASES=exec \
tests/manual-smoke/run_all.sh
```
