# gocracker

Micro-VMM in Go using KVM. Runs OCI images, Dockerfiles, or git repos as microVMs.
**No Docker daemon required.**

## Features

Use this table for shipped behavior. Design choices, limitations, and sweep status live in the sections below.

| Feature | Status |
|---------|--------|
| KVM bindings (VM, vCPU, ioctl) | Done |
| x86-64 long mode (4-level page tables, GDT) | Done |
| bzImage loader compatibility path (legacy, not the recommended x86 path) | Experimental |
| ELF vmlinux loader | Done |
| UART 16550A (full register set, IRQ) | Done |
| KVM IRQ injection (`KVM_IRQ_LINE`) | Done |
| Virtio MMIO transport (virtio 1.1) | Done |
| virtio-net (TAP backend, TX/RX, IRQ) | Done |
| Standalone auto networking (`--net auto`: TAP + IPv4 + NAT) | Done |
| virtio-blk (raw image, read/write/flush/discard/getID) | Done |
| virtio-rng (entropy for /dev/random) | Done |
| virtio-vsock (host-guest streams, RX queue) | Done |
| virtio-balloon (manual target updates, stats polling, conservative auto reclaim) | Done |
| Memory hotplug (`/hotplug/memory`, online guest memory blocks) | Done |
| FDT/DTB generator | Done |
| SMP / multi-vCPU guest boot (`--cpus`, 2/4 vCPU validated) | Done |
| OCI puller (any registry, gzip/zstd/uncompressed layers) | Done |
| OCI layer extraction preserves ownership and file metadata | Done |
| Dockerfile parser/executor (BuildKit AST + gocracker executor) | Done |
| Dockerfile remote stage imports (`COPY --from=<registry/image[:tag]>`) | Done |
| Dockerfile `RUN --mount` subset (`bind`, `cache`, `tmpfs`) | Done |
| Privileged Dockerfile `RUN` isolation (private mount namespace + minimal private `/dev` + read-only `/proc`/`/sys`) | Done |
| Git repo cloner | Done |
| Docker Compose orchestrator (`compose-go` parser + gocracker runtime) | Done |
| Compose `.env`, labels list/map, and interpolation via `compose-go` | Done |
| Compose one-shot dependencies (`service_completed_successfully`) with supervised exit-code capture | Done |
| Compose in-guest healthchecks via exec-agent (`CMD` / `CMD-SHELL`, including container-local binaries/scripts) | Done |
| Snapshot/restore (RAM + vCPU + device state) | Done |
| Live migration (stop-and-copy between API servers) | Done |
| REST API (Firecracker-compatible + extended) | Done |
| Pure Go initrd builder (no cpio/gzip binaries) | Done |
| Pure Go ext4 disk builder (no `mkfs.ext4`) | Done |
| Recursive repo/Compose discovery | Done |
| Host device / PTY preflight (`check-host-devices.sh`) | Done |
| Interactive serial console PTY isolation | Done |
| x86 ACPI boot path (`--x86-boot=auto|acpi|legacy`) | Done |
| Guest-kernel tooling (`prepare-kernel.sh`, `build-guest-kernel.sh`) | Done |
| Host `vmlinuz` / `bzImage` normalization to ELF `vmlinux` at load time | Done |
| virtio-fs shared filesystem backend | Done |
| Guest compatibility sysctl for non-root low ports (`ip_unprivileged_port_start=0`) | Done |
| Firecracker-style guest handoff: stage-0 initrd bootstrap, then final workload becomes guest `PID 1` | Done |
| Deterministic unique guest MAC assignment per VM/network interface | Done |
| Single-VM Firecracker-compatible worker (`gocracker-vmm`) | Done |
| Firecracker-style low-level jailer (`gocracker-jailer`) | Done |
| Jailed `build-worker` for Dockerfile/image-prep flows | Done |
| API VM registry over abstract `vmm.Handle` (local or worker-backed) | Done |
| `serve --jailer=on|off` plumbing for `/run` and `/build` | Done |
| Compose per-stack network namespace isolation | Done |
| Migration fallback for worker-backed VMs via snapshot handoff | Done |
| Virtio device rate limiter core (`virtio-net`, `virtio-blk`, `virtio-rng`) | Done |
| Public device rate limiter API wiring (create + runtime update via worker API and `serve`) | Done |
| Worker-backed migration pre-copy control plane (`prepare` / `finalize` / `reset`) | Done |
| Compose shutdown waits for full VM stop before disk sync/cleanup | Done |

### ARM64 / x86-64 Feature Comparison

gocracker supports both x86-64 and ARM64 (AArch64). The table below shows per-architecture status for each subsystem.

| Subsystem | x86-64 | ARM64 | Notes |
|-----------|--------|-------|-------|
| KVM bindings | `KVM_GET/SET_REGS` | `KVM_GET/SET_ONE_REG` | ARM64 uses per-register ioctls |
| Interrupt controller | IOAPIC + LAPIC | GICv2 / GICv3 (in-kernel) | Auto-probed; GICv2 preferred on Graviton 1 |
| IRQ delivery | `KVM_IRQ_LINE` | irqfd (eventfd) | ARM64 matches Firecracker's irqfd approach |
| Serial console | UART 16550A (I/O port 0x3F8) | UART 16550A (MMIO 0x40002000) | Same device, different transport |
| Boot protocol | bzImage / ELF vmlinux | ARM64 Image / Image.gz / ELF | PC=entry, X0=DTB address |
| Device tree | ACPI (x86) | FDT/DTB (generated) | GIC, timer, PSCI, UART, virtio nodes |
| Firmware tables | MP table, ACPI DSDT/MADT | PSCI v0.2 (via KVM) | Shutdown/reboot via PSCI SYSTEM_OFF/RESET |
| SMP boot | INIT/SIPI sequence | PSCI CPU_ON | Secondary vCPUs start with POWER_OFF |
| Virtio MMIO transport | 0xD0000000+ | 0x40003000+ | Firecracker-compatible layout on ARM64 |
| virtio-net | Done | Done | Link activation on DRIVER_OK for carrier detect |
| virtio-blk | Done | Done | GPA translation for ARM64 RAM base (0x80000000) |
| virtio-rng | Done | Done | |
| virtio-vsock | Done | Done | Requires kernel with `CONFIG_VIRTIO_VSOCKETS=y` |
| virtio-balloon | Done | Done | |
| virtio-fs | Done | Done | |
| Snapshot (vCPU state) | `KVM_GET_REGS` / `KVM_GET_SREGS` | `KVM_GET_ONE_REG` (core regs) | X0-X30, SP, PC, PSTATE |
| Snapshot (GIC state) | IOAPIC/PIC/PIT state | Not yet | GIC distributor/redistributor save deferred |
| Restore | Done | Done | Uses `PreferredARM64Target()` per host |
| Jailer | Done | Done | seccomp filter compiled per-arch |
| Compose networking | Done | Done | TAP + bridge + userspace port proxy |
| Guest init | `init_amd64.bin` | `init_arm64.bin` | Static ELF, embedded at build time |
| Guest kernel | `build-guest-kernel.sh` | `build-guest-kernel-arm64.sh` | ARM64 kernel: 6.17.13 with vsock built-in |
| Memory layout | RAM at GPA 0x0 | RAM at GPA 0x80000000 | ARM64 reserves low 2 GB for MMIO |

**Tested on:** AWS a1.metal (Graviton 1 / Cortex-A72), Ubuntu 24.04, kernel 6.17.0-1007-aws.

**ARM64 guest kernel:** `artifacts/kernels/gocracker-guest-standard-arm64-Image` (6.17.13, 55 MB). Built with `tools/build-guest-kernel-arm64.sh` from the Ubuntu AWS base config with virtio + vsock compiled in (not modules).

## Design Choices

- Firecracker is the reference model for x86 boot and isolation decisions. `--x86-boot=acpi` is the pure ACPI path; `--x86-boot=auto` intentionally remains the compatibility bridge during the transition.
- The reproducible guest-kernel path is `./tools/build-guest-kernel.sh`, built from Firecracker's 6.1 x86_64 guest config baseline plus gocracker fragments. The recommended runtime artifact is the ELF `vmlinux`.
- `./tools/prepare-kernel.sh` is only a convenience tool to pin an existing host kernel into the repo. Host-pinned `host-current-vmlinuz` paths are not the baseline contract.
- Compose parsing and normalization go through `compose-go`; Dockerfile parsing goes through BuildKit's official AST. That choice is deliberate and preferred over custom parsers.
- Discovery is recursive but conservative: explicit `--subdir` or `--file` wins, canonical names rank first, and ambiguity fails instead of guessing.
- Networking is intentionally split into two modes: `--tap` is the low-level manual path, while `--net auto` is the built-in standalone IPv4 + NAT path.
- Compose stacks now get a dedicated Linux network namespace by default. Services within one stack can talk to each other, but different Compose stacks and ad hoc VMs are isolated unless connectivity is explicitly published.
- Firecracker-style low-level parity now lives in `gocracker-vmm`, `gocracker-jailer`, and the jailed `build-worker`. The higher-level `run` / `repo` / `compose` / `restore` / `build` flows now route through those workers by default, and `serve` now supervises `gocracker-jailer -> gocracker-vmm` as its default Firecracker-like VM path while `gocracker-vmm` remains usable standalone.

## Recent Runtime Fixes (2026-04-07 / 2026-04-08)

The runtime has caught up with Dockerfile features that were silently breaking
real-world boilerplates. Bugs that previously gated big chunks of the manifest
are now closed, with regression coverage in `internal/dockerfile/...` and
`internal/oci/...`:

| Fix | Where | Affected repos |
|---|---|---|
| `pkg/vmm` `Restore` path now initializes `doneCh` so snapshot restore stops cleanly | [pkg/vmm/vmm.go](pkg/vmm/vmm.go) | live migration, snapshot tests |
| Live migration sends the prepare bundle BEFORE finalize (not an empty dir) | [internal/api/migration.go](internal/api/migration.go) | `TestLiveMigrationStopAndCopy` |
| `vmmserver` now has a `PUT /shared-fs/{tag}` endpoint and `worker.LaunchVMM` propagates SharedFS mounts from the worker to the in-jail VMM, with virtiofsd spawned host-side and the socket bind-mounted into the jail | [internal/vmmserver/server.go](internal/vmmserver/server.go), [internal/worker/vmm.go](internal/worker/vmm.go), [internal/sharedfs/backend.go](internal/sharedfs/backend.go) | virtio-fs through the jailer path |
| `gocracker repo --ref <commit-sha>` now uses `git init + fetch + checkout` (with short-SHA fallback to a full clone) and post-fetches tags so `git describe --tags` works inside builds | [internal/repo/repo.go](internal/repo/repo.go) | gitleaks, dex, lego, traefik-whoami, dozens of repos with SHA refs |
| Dockerfile parser supports `RUN <<EOF ... EOF` heredoc form (single and multi-block) | [internal/dockerfile/dockerfile.go](internal/dockerfile/dockerfile.go) | git-cliff, modern BuildKit Dockerfiles |
| `RUN --mount=type=bind` accepts an omitted `target=` and uses `source=` (BuildKit semantics) | [internal/dockerfile/rootless_linux.go](internal/dockerfile/rootless_linux.go) | navidrome and others |
| `RUN --mount=type=secret` is now implemented end-to-end with file-based secrets at `$GOCRACKER_BUILD_SECRETS_DIR/<id>`, strict `0600`/`0700` permission checks, path-traversal rejection, and a graceful empty-file fallback for `required=false`. Env-var-based secrets are intentionally NOT supported (they leak through `/proc/<pid>/environ`) | [internal/dockerfile/rootless_linux.go](internal/dockerfile/rootless_linux.go) | rubygems, plenty of Rails/PHP Dockerfiles |
| `HEALTHCHECK --start-interval` is recognized | [internal/dockerfile/dockerfile.go](internal/dockerfile/dockerfile.go), [internal/oci/oci.go](internal/oci/oci.go) | mailpit and other modern Dockerfiles |
| Privileged build helper now exports `HOME`, `USER`, `LOGNAME` and `chdir`s into the resolved home **after** `setuid()` so non-root `USER` can write into its own home (uv, pip, cargo, npm, etc.). Previously every `USER python` Dockerfile crashed with `Permission denied: /root/.cache/...` | [internal/dockerfile/rootless_linux.go](internal/dockerfile/rootless_linux.go) | nickjj/docker-django-example, docker-flask-example, docker-rails-example, docker-phoenix-example, fastapi-sqlalchemy-asyncpg, dexidp/dex, ~50 production templates |
| `COPY` with multiple wildcard sources now tolerates patterns that match nothing as long as at least one src contributes files (BuildKit semantics) | [internal/dockerfile/transfer.go](internal/dockerfile/transfer.go) | symfony-docker family, frankenphp templates |
| OCI tar extraction now `Lchown` BEFORE `chmod` and uses `syscall.Chmod` with the raw unix mode so `setuid`, `setgid` and `sticky` bits survive into the rootfs. The kernel auto-strips suid bits on chown, so any image that ships `/usr/bin/sudo` previously lost the setuid bit during extraction | [internal/oci/oci.go](internal/oci/oci.go) | rust-web-example (ekidd/rust-musl-builder), every base image with suid binaries |
| `tests/integration` has a real `TestMain` that dispatches the re-exec'd `vmm`/`jailer` subcommands (instead of recursing into the test runner) and disables seccomp for in-process tests | [tests/integration/main_test.go](tests/integration/main_test.go) | the entire integration suite went from "harness broken" to 9/9 PASS |
| Compose healthchecks now execute inside the guest through the exec-agent, `depends_on.required=false` no longer blocks optional deps, Dockerfile autodiscovery prefers runtime candidates over `.devcontainer`/`examples`, and `compose --server` keeps stack netns/TAP/published-port ownership on the API server so ports survive after the client exits | [internal/compose/health.go](internal/compose/health.go), [internal/compose/compose.go](internal/compose/compose.go), [internal/discovery/discovery.go](internal/discovery/discovery.go), [internal/api/api.go](internal/api/api.go), [internal/stacknet/stacknet.go](internal/stacknet/stacknet.go) | compose-health, compose-todo-postgres, compose-exec, stack isolation integration tests |

Combined effect on the external-repo sweep: **104 → 116+ PASS** while the
total candidate list grew from `~85` curated repos to `200+` (split between the
historical baseline, expanded sets, and discovered framework boilerplates).

## Current Limitations

- `--net auto` is IPv4-only today, requires host `ip` plus `iptables-nft` or `iptables`, picks the host-side IPv4 default route as the NAT upstream, and is not supported with snapshot restore yet.
- Compose supports `depends_on.condition` values `service_started`, `service_healthy`, and `service_completed_successfully`, and now honors `required: false`; `depends_on.restart` still fails explicitly because there is no restart-manager semantics behind it yet.
- Compose healthchecks now run in-guest through the exec-agent and support arbitrary `CMD` / `CMD-SHELL` probes with `interval`, `timeout`, `retries`, `start_period`, and `start_interval`. The remaining gap is not probe translation anymore; it is healthcheck behavior that depends on unsupported Compose lifecycle features outside the current VM model.
- Compose does not support non-`local` named volume drivers yet, and `driver: local` still rejects unsupported `driver_opts` instead of silently accepting them.
- Compose supports short and long syntax for `ports` and `volumes`, including TCP/UDP, simple ranges, `ports.name`, `ports.mode`, `ports.app_protocol`, `bind.create_host_path`, `volume.nocopy`, `volume.subpath`, `tmpfs.size`, and `tmpfs.mode`. Unsupported options such as bind `propagation`, non-empty volume `consistency`, and unsupported local driver opts now fail explicitly during normalization.
- Dockerfile parsing comes from BuildKit, and the executor supports the common `RUN --mount` subset (`bind`, `cache`, `tmpfs`, `secret` with file backing). Remaining gaps: `--mount=type=ssh` (only an empty-file fallback for non-required), and extra `COPY`/`ADD` flags beyond `--from`/`--chown`/`--chmod`/`--link`/`--exclude`/`--parents`/`--keep-git-dir`/`--checksum`/`--unpack`.
- Repo Dockerfile autodiscovery is now runtime-oriented and deliberately de-prioritizes `.devcontainer`, `examples`, `docs`, and `tests`. The remaining limitation is ambiguity: if several runtime-looking Dockerfiles still rank similarly, autodiscovery fails and asks for an explicit `--subdir` or `--dockerfile`.
- Firecracker-style ballooning is now supported preboot and at runtime. `virtio-balloon` exposes manual target updates, stats polling, and a host-side `conservative` auto mode. The reclaim semantics are intentionally best-effort within the fixed `--mem` budget, not a hard promise that every guest will yield the same host RSS drop.
- Memory hotplug is now supported for online grow/shrink beyond the fixed base `--mem` budget via Firecracker-style `PUT/PATCH/GET /hotplug/memory` and `--hotplug-*` creation flags. The shipped implementation uses explicit guest memory-block probe/online/offline flows over the exec-agent, not `virtio-mem`; snapshot/restore/migration with hotplug stays explicitly unsupported until multi-region state handling is validated end-to-end.
- `--arch` is now a public part of the VM contract and is persisted into VM/snapshot metadata. The shipped execution backend is still `amd64`, so `--arch arm64` currently fails early with a clear error instead of staying implicit. Snapshot, restore, and migration are `same-arch only`, and API migration now preflights the destination `host_arch` before shipping a bundle. The repo now ships per-arch guest-init payloads (`init_amd64.bin`, `init_arm64.bin`), an `arm64` seccomp profile, and `make build` now follows the host architecture by default instead of hardcoding `amd64`.
- `run`, `repo`, `compose`, `restore`, and `build` now default to jailed workers (`gocracker-jailer -> gocracker-vmm` for VM execution, `gocracker-jailer -> build-worker` for image prep). In `serve`, the Firecracker-compatible preboot flow (`/boot-source` + `/actions`) now launches the root VM through `gocracker-jailer -> gocracker-vmm` as well, and `cmdServe` now defaults to that worker-backed mode.
- The API server now stores abstract VM handles instead of requiring only local `*vmm.VM`, so info/logs/events/stop/snapshot work for local and worker-backed VMs through the same public endpoints. Worker metadata is now persisted under `--state-dir`, and `serve` reattaches to live worker-backed VMs on restart instead of treating them as request-scoped state.
- Worker-backed migration already exposes the same `prepare` / `finalize` / `reset` pre-copy control plane as local VMs. The older snapshot handoff path remains as an explicit compatibility fallback for non-migration-capable backends.
- The jailed worker path needs privileges sufficient for mount namespace isolation (`CLONE_NEWNS` / `pivot_root`). In practice that currently means running those flows with the same elevated privileges you would use for Firecracker's jailer path.
- API request throttling is still not implemented.

## Safety And Operations

- The guest `/init` refuses to run unless it is `PID 1` inside the VM, so it cannot accidentally mutate host `/dev` nodes if invoked in the wrong context.
- Host-side preflight fail-closes on broken `/dev/null`, `/dev/random`, `/dev/urandom`, `/dev/tty`, `/dev/kvm`, or `/dev/net/tun`. Use `./tools/check-host-devices.sh` before privileged runs.
- `./tools/trace-dev-access.sh` exists to trace host `/dev` and PTY access when debugging safety regressions.
- The most likely historical `/dev` corruption path is now identified: the old guest `/init` could mutate `/dev` directly if run outside the VM. That path is now fail-closed via the `PID 1` guard.
- The privileged Dockerfile `RUN` path no longer mounts `proc`/`sys`/`dev` into the host mount namespace. It now follows the same isolation order Firecracker's jailer uses at a smaller scale: private mount namespace, bind the build root over itself, `pivot_root`, detach `old_root`, mount read-only `/proc` and `/sys`, build a minimal private `/dev` (including `/dev/null`, `/dev/zero`, `/dev/random`, `/dev/urandom`, `/dev/tty`, `/dev/ptmx`, and `devpts`), and let all those mounts disappear automatically when the helper exits.
- `Compose.Down()` now waits for the VM to stop fully before sync/cleanup, and it skips loop-mounting the guest disk entirely when no volume actually needs copy-back.

## External Sweep Policy

- The checked-in sweep manifest currently carries `328` pinned entries: `278` Dockerfile and `50` Compose.
- Every row carries `disk_mb`. The harness defaults to `4096 MiB`, and larger stacks can override that per entry.
- The external-repo harness now treats per-case VM caches and cloned repo worktrees as disposable by default: it uses a per-case VM cache directory and deletes both that cache and the cloned checkout after each case, so ext4 disks, initrds, rootfs trees, and repo worktrees do not accumulate across the sweep. Set `EXT_REPO_KEEP_VM_CACHE=1` or `EXT_REPO_KEEP_REPO_CACHE=1` only when intentionally debugging one case.
- Setup-heavy or release-shaped repos do not belong in the main sweep. They should move to the excluded list instead of being treated as product regressions.
- The harness now separates launch time from probe time: image pull/build + VM launch are one phase, and post-startup checks are another. That keeps slow builds from being mislabeled as guest boot failures.

## Current Targeted Sweep Findings

- `spring-petclinic` and `appsmith` already pass in the newer targeted reruns, even though the last completed full snapshot below still shows their older state.
- `fastapi-template` now passes in the latest directed rerun. The fixes that closed it were: remote-image `COPY --from=ghcr.io/astral-sh/uv:0.9.26 ...`, OCI layer extraction that preserves ownership/metadata so non-root images like `adminer` can write inside their declared working trees, deterministic per-VM guest MAC assignment, and supervised handling of `service_completed_successfully` one-shot services (`prestart`) without depending on host loop mounts.
- `appwrite` no longer fails on the old fixed-IP Compose networking bug, the old `coredns` low-port bind failure, or the old `4096 MiB` disk budget; the checked-in manifest now gives it `8192 MiB` of disk and the current rerun is reaching much deeper into the stack.
- `gotify` still carries a required build-arg placeholder in `FROM`, which makes it a release-shaped input rather than a clean self-contained Dockerfile target.
- `rustdesk-server` now passes in the latest directed rerun after the Firecracker-style guest handoff change that lets the image entrypoint become the final guest `PID 1`.
- `mealie` and `bookstack-compose` currently hit upstream Debian repo expiry during `apt-get update`; those are environment/upstream failures, not parser/runtime regressions.
- `filebrowser` is currently failing as a release-shaped Dockerfile that expects a prebuilt `filebrowser` binary in the checkout.
- The fixed-50 sweep selection now follows the exclusion policy below: release-shaped Dockerfiles such as `minio-minio`, `hashicorp-http-echo`, and `chartmuseum-chartmuseum`, plus invalid root-target entries such as `caddyserver-caddy`, are not part of the main checked-in fixed batch.
- `mailhog-mailhog` is no longer a clean regression gate: its pinned Dockerfile still uses `go install github.com/mailhog/MailHog@latest` on `golang:1.18-alpine`, so upstream module drift can break it without any runtime regression.
- `prometheus-node-exporter` and `prom-prometheus` are currently behaving as release-shaped Dockerfiles that expect checked-out `.build/...` artifacts rather than a clean source-only build.
- Recently fixed on the current branch: large runtime specs now ride in the initrd instead of the kernel cmdline, the guest retries `runtime.json` after entering the mounted rootfs, shared writable file/socket mounts such as `/var/run/docker.sock` are promoted to a shared parent-directory mount, guest `/run` now materializes `/run/...` directories declared in `tmpfiles.d` before the OCI process starts, and Compose now reads one-shot service exit codes directly from the ext4 image without host loop-device mounts.

## Excluded / Setup-Heavy Examples

- `hashicorp-nomad`: release-shaped Dockerfile that expects `dist/$TARGETOS/$TARGETARCH/nomad`.
- `aquasecurity-trivy`: release-shaped Dockerfile that expects a prebuilt `trivy` artifact.
- `keycloak`: checked-in Dockerfile expects operator build outputs under `operator/target/...`.
- `gotify`: release-shaped Dockerfile with a required build-arg placeholder in `FROM`.
- `minio-minio`: checked-in Dockerfile expects release-shaped `minio-${TARGETARCH}.${RELEASE}` artifacts in the checkout.
- `hashicorp-http-echo`: checked-in Dockerfile expects prebuilt `dist/linux/amd64` artifacts.
- `chartmuseum-chartmuseum`: checked-in Dockerfile expects prebuilt `_dist/linux-amd64/chartmuseum`.
- `caddyserver-caddy`: the pinned ref does not ship a usable checked-in root Dockerfile for the sweep contract.
- `filebrowser`: checked-in Dockerfile expects a prebuilt `filebrowser` binary in the checkout.
- `drupal`: repo does not ship a usable checked-in Dockerfile/Compose target for the sweep contract.
- `nexus-public`: repo does not ship a usable checked-in Dockerfile/Compose target for the sweep contract.
- `ghost`: this ref only ships development Compose overlays around `yarn dev`; it is not a clean self-contained deployment target for the main sweep.
- `mailcow-compose`: large mail-stack Compose tree that requires heavier setup than a clean checkout.

### Roadmap

Firecracker is the reference model for the security and reliability work below. The priority is to copy the shape that already works there instead of growing ad hoc host-side cleanup and operational heuristics.

#### Execution Plan

1. `serve` supervisor/proxy de workers
Status: `done`
Why it exists: `serve` ahora arranca en modo worker-backed por default, valida `gocracker-jailer` y `gocracker-vmm`, y el flujo Firecracker-compatible de preboot (`startPrebootVM`) ya crea la VM como proceso externo supervisado.
Acceptance: crear VMs worker-backed por defecto, tratarlas como procesos externos por VM, y dejar que `serve` posea start/stop/cleanup sin depender del request path.

2. Registro persistente de workers y cleanup
Status: `done`
Why it exists: el proceso API ahora persiste `kind`, `socket_path`, `worker_pid`, `jail_root`, `run_dir`, `created_at`, `vmm.Config`, bundle dir y marca de root slot bajo `--state-dir`, y rehidrata/reatacha ese estado al reiniciar.
Acceptance: el registro por VM debe sobrevivir lo necesario para reinicios controlados del plano de control y mantener cleanup determinista de sockets/jails/workdirs.

3. SSE/events/logs proxy sobre worker sockets
Status: `done`
Why it exists: la API pública ya usa `vmm.Handle`, y el handle remoto ya refresca `info`, `events` y `logs` desde el socket del worker sin exponer rutas separadas.
Acceptance: `/vms`, `/vms/{id}`, `/logs`, `/events` y `/events/stream` deben funcionar igual para VMs locales o worker-backed sin rutas especiales ni estado implícito.

4. Migración pre-copy remota para workers
Status: `done`
Why it exists: los workers ya exponen `prepare` / `finalize` / `reset`, y `serve` ya usa esa misma ruta pública también para handles remotos antes de caer al fallback de snapshot.
Acceptance: una VM worker-backed debe migrar por la misma ruta pública que una VM local, manteniendo bundle pre-copy, finalize y reset tracking consistentes.

5. Retiro del snapshot handoff como ruta principal
Status: `pending`
Why it exists: el fallback de snapshot sigue siendo útil como compatibilidad, pero ya no debe ser la ruta objetivo cuando el backend soporta pre-copy remoto.
Acceptance: el fallback queda solo como compatibilidad explícita para backends que no implementen el control plane completo de migración.

6. API pública de rate limiters create/update
Status: `done`
Why it exists: los token buckets ya existían dentro de `virtio`, pero faltaba subirlos al control plane.
Acceptance: create-time por `/machine-config`, `/drives/{id}`, `/network-interfaces/{id}` y runtime update por `serve`/worker API para `net`, `blk` y `rng`.

7. Persistencia de limiters en snapshot/restore
Status: `done`
Why it exists: el estado/config de rate limiting debe viajar con la VM igual que el resto del config de dispositivos.
Acceptance: snapshot/restore conserva `net_rate_limiter`, `block_rate_limiter` y `rng_rate_limiter` porque ya viven dentro de `vmm.Config`.

8. Hardening y tests de aislamiento Compose por stack
Status: `done`
Why it exists: el aislamiento host-side ya no queda en best-effort del proceso cliente; `serve` ahora es dueño del netns/TAP/forwards del stack cuando usas `compose --server`, y eso quedó cubierto con integration tests privilegiados.
Acceptance: dos stacks simultáneas no se alcanzan entre sí por default, una VM de `run` no alcanza una stack Compose por default, los `ports:` publicados siguen accesibles y cleanup elimina netns/enlaces host-side.

8.1. Reemplazar el canal interno de `compose exec` por un guest agent nativo sobre `vsock`
Status: `done`
Why it exists: la UX pública ya es `compose exec`, y ahora el backend quedó alineado con eso usando un guest agent nativo sobre `virtio-vsock`, sin depender de un stack SSH paralelo.
Acceptance: `compose exec` abre shell y ejecuta comandos por nombre de servicio a través de `serve`, y la API expone `POST /vms/{id}/exec` y `POST /vms/{id}/exec/stream` sin requerir claves/usuario del usuario ni depender de un camino SSH público.

9. Seccomp baseline por perfil para `gocracker-vmm`
Status: `done`
Why it exists: el worker VMM ya no corre sin filtro syscall; ahora instala un perfil base del proceso worker y un overlay más estricto en cada thread de vCPU, siguiendo el split API/VMM/vCPU hasta donde permite el runtime de Go.
Acceptance: `gocracker-vmm` instala seccomp por default al escuchar en su Unix socket, los vCPU threads fijados con `runtime.LockOSThread()` apilan un perfil `vcpu`, y existe un escape hatch operativo vía `GOCRACKER_SECCOMP=off`.

10. Validación preboot fuerte para API Firecracker-like
Status: `done`
Why it exists: `serve` y `gocracker-vmm` ya no aceptan silenciosamente payloads incompletos o campos desconocidos antes de bootear.
Acceptance: `/boot-source`, `/machine-config`, `/drives/{id}` y `/network-interfaces/{id}` usan JSON estricto, validan límites de `vcpu_count`, `mem_size_mib` y `boot_args`, rechazan topologías no soportadas antes de `InstanceStart`, y exponen errores de estado/argumento de forma consistente.

- Firecracker-style metrics and logging surfaces: structured metrics, per-device counters, latency metrics for pause/resume/snapshot operations, and a clearer contract for what is reconfigured or reset after restore.
- Snapshot/restore contract hardening: explicit snapshot format/version policy, integrity and size validation before deserialize, restore-time overrides for network/vsock paths, and documented compatibility rules for host kernel, CPU model, and external resources.
- Graceful shutdown and lifecycle supervision: a stronger state machine around `preboot/running/paused/restored`, an x86 shutdown path equivalent to `SendCtrlAltDel`, and a documented host-side overwatcher model for wedged VMMs or external helpers.
- Rate-limiter hardening beyond the newly wired public API: CLI examples, richer compatibility docs for restore, and any missing parity edges between local and worker-backed device update paths.
- Closer Firecracker parity for memory hotplug: `virtio-mem` and richer multi-region lifecycle compatibility beyond the shipped `/hotplug/memory` online grow/shrink path.
- Broader `vhost-user` integration beyond `virtio-fs`, but isolated the way Firecracker treats external backends: backend-specific sandboxing, explicit lifecycle management, and no silent assumption that frontend rate limiting covers backend behavior.
- Make the pure ACPI x86 path the default and retire the current compatibility hybrid in `--x86-boot=auto`.
- Better guest-kernel tooling and docs for `virtio-fs`-capable kernels, including a more explicit supported-kernel policy similar to Firecracker's guest-kernel documentation.
- Compose support for additional volume drivers and more advanced port/volume options once the lower-level isolation and lifecycle story is stronger.
- Hot-path performance tuning: preallocated buffers in vCPU/device run loops, reduced heap churn, and evaluated low-GC modes (`GOGC` tuning, including dedicated-deployment experiments with `GOGC=off`).

## Build

```bash
# Build the CLI binary
go build -o gocracker ./cmd/gocracker

# Build the low-level Firecracker-like worker and jailer
go build -o gocracker-vmm ./cmd/gocracker-vmm
go build -o gocracker-jailer ./cmd/gocracker-jailer

# Build every package in the module
go build ./...

# Static CLI binary
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -ldflags="-s -w" -o gocracker ./cmd/gocracker
```

## Test

```bash
# Build an in-repo guest kernel first
./tools/build-guest-kernel.sh

# Unit tests (no root, no KVM required)
go test ./...

# Integration tests (requires KVM + root + kernel)
GOCRACKER_KERNEL=./artifacts/kernels/gocracker-guest-standard-vmlinux \
  go test -tags integration ./tests/integration/

# Darwin feature e2e (requires signed Apple Silicon host, kernel, and prebuilt signed binaries)
DARWIN_SIGN_IDENTITY='Developer ID Application: <Team> (<TEAMID>)' \
  make build-darwin-e2e
GOCRACKER_E2E=1 \
  go test -tags e2e ./tests/e2e/darwin/...

# Optional when the signed binaries live outside the repo root
GOCRACKER_E2E=1 GOCRACKER_E2E_BIN_DIR=/path/to/signed/bin \
  go test -tags e2e ./tests/e2e/darwin/...

# Host sanity check before privileged runs
./tools/check-host-devices.sh
```

<!-- EXTERNAL_SWEEP_REPORT:START -->

## External Repo Sweep

Latest recorded sweep snapshot: `/home/misael/Desktop/projects/gocracker/.tmp/external-sweep-merged`

- Updated from `results.tsv` on 2026-04-03 11:04:42
- Last completed full snapshot: `100/150`
- `PASS`: `73`
- `FAIL`: `27`

The checked-in external manifest now carries `328` pinned entries (`278` Dockerfile, `50` Compose) and every row carries `disk_mb`. The last completed full rerun below still predates the current checked-in manifest shape, so it remains the latest *completed* sweep snapshot, not the final result for the present 328-entry inventory.

The current regression-validation workflow is now split into reproducible gates: keep `tests/external-repos/historical-pass.ids` as the baseline, move non-hermetic entries into `tests/external-repos/historical-unstable.ids`, validate interactive shell behavior with `tests/external-repos/historical-tty.tsv` and `tests/external-repos/compose-tty.tsv`, and only run the fixed-50 after those gates are green.

The previous targeted rerun workspace was intentionally cleaned after the host `/dev` repair and temporary-disk cleanup. The next rerun should start from that clean host state, with the current blockers now narrowed to product/runtime issues rather than broken `/dev`, `sudo`, or KVM/TUN preflight.

### Manual Validation Snapshot (2026-04-06)

Run these from the repo root once per session before the one-by-one checks:

```bash
go build -o ./gocracker ./cmd/gocracker
go build -o ./gocracker-vmm ./cmd/gocracker-vmm
go build -o ./gocracker-jailer ./cmd/gocracker-jailer
sudo -v
```

Current reproducible manual checks:

| Case | Status on 2026-04-09 | Exact command | Expected result |
| --- | --- | --- | --- |
| Alpine one-shot guest smoke | `PASS` | `sudo -n ./gocracker run --image alpine:3.20 --kernel ./artifacts/kernels/gocracker-guest-standard-vmlinux --cmd 'echo alpine-ok' --wait --tty off` | prints `alpine-ok` and stops cleanly |
| Alpine interactive guest + TTY smoke | `PASS` | `sudo -n ./gocracker run --image alpine:3.20 --kernel ./artifacts/kernels/gocracker-guest-standard-vmlinux --wait --tty force` | shell prompt appears; `echo A_B_C`, backspace, `Ctrl-C`, and `exit` all work through the exec-agent PTY path |
| Balloon reclaim integration: local + worker | `PASS` | `sudo -n env GOCRACKER_KERNEL=./artifacts/kernels/gocracker-guest-standard-vmlinux go test -tags integration ./tests/integration -run 'TestVirtioBalloon(LocalStatsAndExec|WorkerReclaimsRSS)' -count=1 -v` | both tests pass; local balloon stats populate, worker-backed VM shows observable RSS drop after a balloon target update, and guest `exec` still responds |
| Historical TTY gate: `distribution-registry` | `PASS` | `GOCRACKER_KERNEL=./artifacts/kernels/gocracker-guest-standard-vmlinux EXT_REPO_IDS=distribution-registry tests/external-repos/run_historical_tty.sh` | ends with `PASS distribution-registry` |
| Compose TTY gate: `actual-server-compose` | `PASS` | `GOCRACKER_KERNEL=./artifacts/kernels/gocracker-guest-standard-vmlinux EXT_REPO_IDS=actual-server-compose tests/external-repos/run_compose_tty.sh` | ends with `PASS actual-server-compose` |
| Historical TTY gate: `go-gitea-gitea` | `Pending rerun` | `GOCRACKER_KERNEL=./artifacts/kernels/gocracker-guest-standard-vmlinux EXT_REPO_IDS=go-gitea-gitea tests/external-repos/run_historical_tty.sh` | as of 2026-04-06 the rerun was still inside Dockerfile build (`make backend`) and had not reached guest boot yet, so there was no guest/TTY verdict yet |

Memory controls snapshot on 2026-04-09:

- `virtio-balloon` is shipped and covered in `go test ./...` plus the privileged integration command above.
- `--balloon-target-mib`, `--balloon-deflate-on-oom`, `--balloon-stats-interval-s`, and `--balloon-auto=off|conservative` are available on `run` and `repo`.
- Firecracker-style `GET/PUT/PATCH /balloon` and `GET/PATCH /balloon/statistics` are implemented in `gocracker-vmm` and `serve`.
- `--hotplug-total-mib`, `--hotplug-slot-mib`, and `--hotplug-block-mib` are available on `run` and `repo`.
- Firecracker-style `GET/PUT/PATCH /hotplug/memory` is implemented in `gocracker-vmm` and `serve`.
- The shipped hotplug path is real online grow/shrink on top of the base `--mem` allocation, validated on local and worker-backed VMs. It currently uses guest memory-block probe/online/offline flows rather than `virtio-mem`.
- Snapshot/restore/migration with hotplug is still rejected explicitly.

Terminal-noise note:

- Raw `\x1b[1;1R` is a terminal cursor-position reply, not guest workload output.
- Raw `\x1b[?2004h` and `\x1b[?2004l` are bracketed-paste toggles from interactive shells such as `bash`.
- The 2026-04-06 `PASS` transcripts for `distribution-registry` and `actual-server-compose` were rechecked and did not include those raw sequences.
- On 2026-04-09, the Compose/UX fixes in this section were revalidated with `go test ./...` green plus privileged integration coverage for `TestAPIServeRunExec`, `TestCLIComposeServeExec`, `TestCLIRunInteractiveExec`, `TestCLIComposeExecInteractive`, `TestCLIComposeServeHealthcheckExecBinary`, and `TestComposeStackIsolationAndCleanup`.

Recommended validation order:

1. minimal guest smoke (`alpine`, one-shot and PTY)
2. `tests/external-repos/run_historical_pass.sh`
3. `tests/external-repos/run_historical_tty.sh`
4. `tests/external-repos/run_compose_tty.sh`
5. `tests/external-repos/run_fixed_50.sh`

Current tested repos:

| ID | Kind | Stack | Status | Ref | Failure | Notes |
|----|------|-------|--------|-----|---------|-------|
| `traefik-whoami` | `dockerfile` | `go` | `PASS` | `3c6fc4814630` | `` | small http service |
| `minio-minio` | `dockerfile` | `go` | `PASS` | `7aac2a2c5b7c` | `` | object storage |
| `go-gitea-gitea` | `dockerfile` | `go` | `PASS` | `4fa319b9dca4` | `` | forge |
| `distribution-registry` | `dockerfile` | `go` | `PASS` | `4045624d53b8` | `` | registry |
| `caddyserver-caddy` | `dockerfile` | `go` | `PASS` | `4f504588669e` | `` | web server |
| `mailhog-mailhog` | `dockerfile` | `go` | `PASS` | `e6fa06877ef6` | `` | smtp sink |
| `prometheus-node-exporter` | `dockerfile` | `go` | `PASS` | `b99dcb64f95f` | `` | exporter |
| `prom-prometheus` | `dockerfile` | `go` | `PASS` | `f1b226a2f355` | `` | metrics server |
| `grafana-grafana` | `dockerfile` | `go` | `PASS` | `04aa182bf8fe` | `` | dashboard |
| `grafana-loki` | `dockerfile` | `go` | `PASS` | `a2c96224b3d0` | `` | logs |
| `grafana-tempo` | `dockerfile` | `go` | `PASS` | `7f19357e6803` | `` | tracing |
| `influxdata-telegraf` | `dockerfile` | `go` | `PASS` | `bfd46ca8648d` | `` | agent |
| `influxdata-influxdb` | `dockerfile` | `go` | `PASS` | `79a63cc7d5d7` | `` | database |
| `open-policy-agent-opa` | `dockerfile` | `go` | `PASS` | `7d266cb687ca` | `` | policy engine |
| `ollama-ollama` | `dockerfile` | `go` | `PASS` | `3536ef58f613` | `` | llm runtime |
| `hashicorp-http-echo` | `dockerfile` | `go` | `PASS` | `767bbd9c3d19` | `` | tiny service |
| `chartmuseum-chartmuseum` | `dockerfile` | `go` | `PASS` | `7508866ec401` | `` | chart repo |
| `qdrant-qdrant` | `dockerfile` | `rust` | `PASS` | `eabee371fda4` | `` | vector db |
| `meilisearch-meilisearch` | `dockerfile` | `rust` | `PASS` | `14efbaeef1b8` | `` | search engine |
| `coredns-coredns` | `dockerfile` | `go` | `PASS` | `510977c47660` | `` | dns server |
| `gotenberg-gotenberg` | `dockerfile` | `go` | `PASS` | `4811a005439f` | `` | document api |
| `gogs-gogs` | `dockerfile` | `go` | `PASS` | `b53d316233f9` | `` | forge |
| `woodpecker-ci-woodpecker` | `dockerfile` | `go` | `PASS` | `2496287790b7` | `` | ci |
| `drone-drone` | `dockerfile` | `go` | `PASS` | `20da7b5320d9` | `` | ci |
| `openfaas-faas` | `dockerfile` | `go` | `PASS` | `f4dc39f8d87b` | `` | faas gateway |
| `etcd-io-etcd` | `dockerfile` | `go` | `PASS` | `c64f8fd68c2e` | `` | kv store |
| `cockroachdb-cockroach` | `dockerfile` | `go` | `PASS` | `aa7c2d664ef5` | `` | sql db |
| `victoria-metrics` | `dockerfile` | `go` | `PASS` | `3e51f277bd4f` | `` | time series |
| `seaweedfs-seaweedfs` | `dockerfile` | `go` | `PASS` | `059bee683f6b` | `` | object store |
| `apache-apisix` | `dockerfile` | `lua` | `PASS` | `90a15cf6e611` | `` | api gateway |
| `rabbitmq-server` | `dockerfile` | `erlang` | `PASS` | `3134e1787d59` | `` | broker |
| `redis-redis` | `dockerfile` | `c` | `PASS` | `a0bad9a0486f` | `` | cache |
| `valkey-valkey` | `dockerfile` | `c` | `PASS` | `fb655dbf5cfa` | `` | cache fork |
| `memcached-memcached` | `dockerfile` | `c` | `PASS` | `3824603965ec` | `` | cache |
| `envoy-envoy` | `dockerfile` | `cpp` | `PASS` | `1679e848a673` | `` | proxy |
| `haproxy-haproxy` | `dockerfile` | `c` | `PASS` | `6df366207782` | `` | proxy |
| `eclipse-mosquitto` | `dockerfile` | `c` | `PASS` | `b3b4d77ef3fa` | `` | mqtt broker |
| `nats-server` | `dockerfile` | `go` | `PASS` | `74cbb964b554` | `` | messaging |
| `emqx-emqx` | `dockerfile` | `erlang` | `PASS` | `941e17fdadcd` | `` | mqtt platform |
| `dragonflydb-dragonfly` | `dockerfile` | `cpp` | `PASS` | `7eb8ed894804` | `` | cache |
| `milvus-milvus` | `dockerfile` | `go` | `PASS` | `5ea65db873af` | `` | vector db |
| `typesense-typesense` | `dockerfile` | `cpp` | `PASS` | `e0b182623dff` | `` | search |
| `quickwit-quickwit` | `dockerfile` | `rust` | `PASS` | `0b69dddf81cb` | `` | search |
| `kestra-kestra` | `dockerfile` | `java` | `PASS` | `ee8624b1c1f2` | `` | orchestrator |
| `mlflow-mlflow` | `dockerfile` | `python` | `PASS` | `dc9a58145642` | `` | ml platform |
| `bentoml-bentoml` | `dockerfile` | `python` | `PASS` | `009d68ea713c` | `` | model serving |
| `prefect-prefect` | `dockerfile` | `python` | `PASS` | `34f66a7cfc98` | `` | orchestration |
| `dagster-dagster` | `dockerfile` | `python` | `PASS` | `fbee33ebb8ca` | `` | orchestration |
| `gradio-gradio` | `dockerfile` | `python` | `PASS` | `b4c801ef3c9a` | `` | ui |
| `streamlit-streamlit` | `dockerfile` | `python` | `PASS` | `3b6b46bc2bca` | `` | ui |
| `locust-locust` | `dockerfile` | `python` | `PASS` | `2c8f587a60a0` | `` | load testing |
| `celery-celery` | `dockerfile` | `python` | `PASS` | `f9ea6771e672` | `` | worker |
| `httpbin` | `dockerfile` | `python` | `PASS` | `f8ec666b4d1b` | `` | test api |
| `uvicorn-gunicorn-fastapi` | `dockerfile` | `python` | `PASS` | `339a5c92e9ef` | `` | fastapi base |
| `plotly-dash` | `dockerfile` | `python` | `PASS` | `fa21fcb2e955` | `` | dashboard |
| `wger` | `dockerfile` | `python` | `PASS` | `2e5feb62385d` | `` | django app |
| `semaphore` | `dockerfile` | `go` | `PASS` | `84c9b8ae186a` | `` | ansible ui |
| `paperless-ngx` | `dockerfile` | `python` | `PASS` | `83501757dfe2` | `` | documents |
| `jupyterhub-jupyterhub` | `dockerfile` | `python` | `PASS` | `cad2ffb75413` | `` | notebooks |
| `jupyter-docker-stacks` | `dockerfile` | `python` | `PASS` | `4b3c903cc4f4` | `` | notebook image |
| `livebook` | `dockerfile` | `elixir` | `PASS` | `131ef751ddb6` | `` | notebook |
| `umami` | `dockerfile` | `node` | `PASS` | `0a838649b773` | `` | analytics |
| `metabase` | `dockerfile` | `java` | `PASS` | `cbc40606efe4` | `` | analytics |
| `directus` | `dockerfile` | `node` | `PASS` | `80bca160e043` | `` | headless cms |
| `strapi` | `dockerfile` | `node` | `PASS` | `21402c2d26f0` | `` | cms |
| `n8n` | `dockerfile` | `node` | `PASS` | `f96cdb17db71` | `` | automation |
| `hoppscotch` | `dockerfile` | `node` | `PASS` | `d45903d2e536` | `` | api client |
| `outline` | `dockerfile` | `node` | `PASS` | `5d5213101ef5` | `` | knowledge base |
| `chatwoot` | `dockerfile` | `ruby` | `PASS` | `6f5ad8f3724b` | `` | support inbox |
| `pocketbase` | `dockerfile` | `go` | `PASS` | `78dc12dc2971` | `` | backend |
| `bookstack` | `dockerfile` | `php` | `PASS` | `25790fd02452` | `` | docs wiki |
| `nextcloud-server` | `dockerfile` | `php` | `PASS` | `ae45f67a7554` | `` | groupware |
| `wallabag` | `dockerfile` | `php` | `PASS` | `52ac14ae9f8f` | `` | read later |
| `phpmyadmin` | `dockerfile` | `php` | `FAIL` | `` | `` | db admin |
| `drupal` | `dockerfile` | `php` | `FAIL` | `` | `` | cms |
| `jellyfin` | `dockerfile` | `dotnet` | `FAIL` | `` | `` | media server |
| `gotify` | `dockerfile` | `go` | `FAIL` | `` | `` | notifications |
| `rustdesk-server` | `dockerfile` | `rust` | `FAIL` | `` | `` | remote desktop |
| `mealie` | `dockerfile` | `python` | `FAIL` | `` | `` | recipes |
| `spring-petclinic` | `dockerfile` | `java` | `FAIL` | `` | `` | java app |
| `keycloak` | `dockerfile` | `java` | `FAIL` | `` | `` | auth |
| `nexus-public` | `dockerfile` | `java` | `FAIL` | `` | `` | repository manager |
| `eshoponweb` | `dockerfile` | `dotnet` | `FAIL` | `` | `` | ecommerce |
| `nopcommerce` | `dockerfile` | `dotnet` | `FAIL` | `` | `` | ecommerce |
| `mastodon` | `dockerfile` | `ruby` | `FAIL` | `` | `` | social network |
| `plausible-analytics` | `compose` | `node` | `FAIL` | `` | `` | compose analytics |
| `fastapi-template` | `compose` | `python` | `FAIL` | `` | `` | full stack fastapi |
| `fastapi-postgresql` | `compose` | `python` | `FAIL` | `` | `` | fastapi postgres |
| `appsmith` | `compose` | `node` | `FAIL` | `` | `` | low code |
| `appwrite` | `compose` | `php` | `FAIL` | `` | `` | baas |
| `supabase` | `compose` | `node` | `FAIL` | `` | `` | db platform |
| `dify` | `compose` | `python` | `FAIL` | `` | `` | llm stack |
| `open-webui` | `compose` | `python` | `FAIL` | `` | `` | ui |
| `immich` | `compose` | `node` | `FAIL` | `` | `` | photos |
| `firezone` | `compose` | `elixir` | `FAIL` | `` | `` | vpn |
| `coolify` | `compose` | `php` | `FAIL` | `` | `` | paas |
| `budibase` | `compose` | `node` | `FAIL` | `` | `` | low code |
| `calcom` | `compose` | `node` | `FAIL` | `` | `` | scheduling |
| `redmine` | `compose` | `ruby` | `FAIL` | `` | `` | project management |
| `ghost` | `compose` | `node` | `FAIL` | `` | `` | blog platform |

This section is generated from `tests/external-repos` output. Re-run `./tools/update-external-sweep-report.py` after the sweep advances or finishes.

<!-- EXTERNAL_SWEEP_REPORT:END -->

## Kernel Prep

```bash
# Convenience: pin an existing host kernel into the repo
./tools/prepare-kernel.sh

# Build a guest kernel for normal microVMs
./tools/build-guest-kernel.sh

# Build a guest kernel with virtio-fs built-in
./tools/build-guest-kernel.sh --profile virtiofs

# If you explicitly want the matching bzImage too
./tools/build-guest-kernel.sh --emit-bzimage

# Convenience: pin a host kernel that already has virtio-fs support
./tools/prepare-kernel.sh --profile virtiofs
```

This creates stable repo-local artifacts under:

```bash
./artifacts/kernels/gocracker-guest-standard-vmlinux
./artifacts/kernels/gocracker-guest-virtiofs-vmlinux
```

If you pass `--emit-bzimage`, the matching `*-bzImage` artifact is materialized too.

`prepare-kernel.sh` also creates `host-current-vmlinuz` and `host-current-virtiofs-vmlinuz` symlinks when you want to pin an existing host kernel into the repo, but those are convenience paths rather than the recommended guest-kernel baseline. In practice, distro kernels such as Ubuntu's `/boot/vmlinuz-*` are compressed `bzImage` artifacts; `gocracker` normalizes them to the embedded ELF `vmlinux` at runtime for standard boots, but the repo-built guest kernels remain the stable, validated path.

`prepare-kernel.sh` does not compile anything: it pins an existing host kernel into the repo as a symlink and validates the config/profile. `build-guest-kernel.sh` is the in-repo path for a raw guest kernel built from Linux sources, using Firecracker's x86_64 guest config as the baseline plus gocracker fragments. By default it materializes the validated `vmlinux` artifact; `bzImage` is opt-in via `--emit-bzimage`.

The in-repo x86 guest profiles are intentionally built for the pure ACPI path: `CONFIG_ACPI=y`, `CONFIG_PCI=y`, `CONFIG_X86_MPPARSE=n`, and `CONFIG_VIRTIO_MMIO_CMDLINE_DEVICES=n`.

Validated guest-kernel paths today:

- `./artifacts/kernels/gocracker-guest-standard-vmlinux`
  - validated for normal `run`, SMP (`--cpus 2/4`), pure `--x86-boot acpi`, and standalone `--net auto`
- `./artifacts/kernels/gocracker-guest-virtiofs-vmlinux`
  - validated for pure-ACPI `virtio-fs` mounts and Compose shared read-write volumes via `virtio-fs`

Host-pinned convenience paths such as `./artifacts/kernels/host-current-vmlinuz` now work for straightforward boots too: `gocracker` will normalize a readable distro `vmlinuz`/`bzImage` to the embedded ELF `vmlinux` before loading it. Even so, the in-repo `gocracker-guest-*-vmlinux` artifacts are still the recommended baseline when you want reproducible behavior across hosts and the full validated matrix, especially for `virtio-fs`.

## Usage

### CLI

```bash
# Run from OCI image
sudo ./gocracker run --image ubuntu:22.04 --kernel ./artifacts/kernels/gocracker-guest-standard-vmlinux --mem 256

# Run with multiple vCPUs
sudo ./gocracker run --image alpine:latest --kernel ./artifacts/kernels/gocracker-guest-standard-vmlinux --mem 256 --cpus 4 --wait

# Run with x86 ACPI boot path
sudo ./gocracker run --image alpine:latest --kernel ./artifacts/kernels/gocracker-guest-standard-vmlinux --mem 256 --x86-boot acpi --wait

# Run from Dockerfile
sudo ./gocracker run --dockerfile ./Dockerfile --kernel ./artifacts/kernels/gocracker-guest-standard-vmlinux --mem 512

# Run from git repo (auto-detects Dockerfile)
sudo ./gocracker repo --url https://github.com/user/myapp --kernel ./artifacts/kernels/gocracker-guest-standard-vmlinux

# Run with an existing host TAP device
sudo ./gocracker run --image alpine:latest --kernel ./artifacts/kernels/gocracker-guest-standard-vmlinux --tap tap0 --wait

# Run with automatic standalone networking (TAP + IPv4 + NAT)
sudo ./gocracker run --image alpine:latest --kernel ./artifacts/kernels/gocracker-guest-standard-vmlinux --net auto --wait

# Docker Compose
sudo ./gocracker compose --file ./docker-compose.yml --kernel ./artifacts/kernels/gocracker-guest-standard-vmlinux

# Docker Compose with shared RW volume via virtio-fs
sudo ./gocracker compose --file ./docker-compose.yml --kernel ./artifacts/kernels/gocracker-guest-virtiofs-vmlinux --x86-boot acpi
```

### Networking

- `run --tap` and `repo --tap` are the manual standalone networking modes. The VMM opens the named TAP device and may create it when the host allows that, but it does not configure host-side IPs, forwarding, or NAT around that TAP for you.
- Because of that, seeing guest logs like `network config finished iface="eth0"` or `eth0: link becomes ready` only means the link is up; it does not mean the guest has working Internet access.
- `run --net auto` and `repo --net auto` provision a Firecracker-style standalone path automatically: TAP + static guest IPv4 + host-side gateway IP + IPv4 forwarding + MASQUERADE NAT on the host's default IPv4 route.
- In `--net auto`, the guest gets a static `/30` address and default route without needing DHCP. `gocracker` also ensures `/etc/resolv.conf` exists inside the guest if the image did not ship one.
- `--net auto` currently depends on host `ip` and `iptables-nft`/`iptables`, is IPv4-only, and is not available on snapshot restore yet.
- `compose` is different: it manages TAP names, bridge attachment, and per-service guest IP configuration for the stack automatically.
- If you want full manual control, `--tap` remains available; if you want the simple standalone Internet path, prefer `--net auto`.

### Compose Flow

`gocracker compose` does not build "one big image" for the whole stack. It launches one microVM per Compose service.

- A service with `image:` pulls that OCI image and builds one guest disk for that service.
- A service with `build:` runs the Dockerfile build for that service and builds one guest disk for that service.
- If two services use two different images, you get two separate guest disks and two separate microVMs.
- If two services use the same image ref, they still run as separate microVMs, but OCI layers can be reused from the local cache.

```text
docker-compose.yml
        |
        v
compose-go parse + normalize
        |
        v
dependency order + network plan + volume plan
        |
        +--> service: postgres
        |      |
        |      +--> image: postgres:16
        |      +--> OCI pull / local cache
        |      +--> rootfs -> ext4 disk -> gocracker-vmm worker
        |      `--> VM #1
        |
        `--> service: todo
               |
               +--> build: ./Dockerfile   or   image: ...
               +--> Dockerfile build / OCI pull
               +--> rootfs -> ext4 disk -> gocracker-vmm worker
               `--> VM #2

per-stack network namespace
        |
        +--> bridge inside stack netns
        +--> one TAP per service VM
        +--> static IP per service
        `--> optional host port forwards from `ports:`
```

Actual runtime flow:

1. `compose-go` parses and normalizes `docker-compose.yml`, `.env`, interpolation, labels, and resolved paths.
2. `gocracker compose` computes dependency order from `depends_on`.
3. A dedicated Linux network namespace is created for the stack, with a bridge and one TAP attachment per service VM.
4. Each service is resolved independently:
   - `image:` path: pull OCI image, reusing local cache when present.
   - `build:` path: build the Dockerfile, then materialize the resulting rootfs.
5. Each service becomes its own ext4 guest disk and its own `gocracker-vmm` worker-backed microVM.
6. After boot, the stack bridge attaches the service TAP, assigns the planned guest IP, and exposes any published `ports:` on the host.
7. If `depends_on.condition: service_healthy` is present, startup blocks until the translated healthcheck passes.
8. On shutdown, `compose down` waits for full VM stop, syncs writable volumes back when needed, and removes the stack network namespace.

Shared cache behavior:

- `--cache-dir` is shared across `run`, `repo`, `compose`, `build`, and `serve`.
- OCI pulls reuse the local layer cache under `<cache-dir>/layers`.
- VM artifacts reuse stable workdirs under `<cache-dir>/artifacts` or `<cache-dir>/build-artifacts`.
- On a second identical boot you should see log lines such as `artifact cache hit`, `reusing cached disk`, and `reusing cached initrd`.

How we know it works:

- CLI success means the stack reached the planned boot path and every service VM launched.
- Real verification comes from probing published ports or health endpoints from the host.
- The manual smoke suite exercises real Compose cases:
  - `compose-basic`: published HTTP port responds
  - `compose-health`: `service_healthy` gating works
  - `compose-volume`: writable volume syncs back to host
  - `compose-todo-postgres`: app writes and reads a real TODO through PostgreSQL

Real end-to-end check with the TODO + PostgreSQL fixture:

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

If the last `GET /api/todos` returns the item you just created, the app VM talked to the PostgreSQL VM through the Compose stack network and persisted the row.

Interactive access today:

- `gocracker compose exec` is now the direct, Docker-style path for services created through `serve`.
- `POST /vms/{id}/exec` and `POST /vms/{id}/exec/stream` are the direct API path for VMs created through `serve`.
- You can interact with a service through its published ports, just like any other server.
- You can inspect guest output through logs/events in the API-driven flows, or through the serial/log artifacts used by manual smoke.

Compose via `serve`:

- `gocracker compose --server http://127.0.0.1:8080 ...` creates each service through the API server instead of launching local VMs directly.
- Those VMs appear in `/vms` with metadata such as `orchestrator=compose`, `stack_name`, `service_name`, `guest_ip`, `tap_name`, and `published_ports`.
- `gocracker compose exec` resolves services through that `/vms` metadata, so another terminal can target `debug`, `todo`, or `postgres` by service name.
- `compose --server` now leaves the stack network, TAP attachment, and published-port forwards owned by `serve`, so they survive after the client command exits.
- `compose down --server ...` resolves the stack through `/vms` metadata, stops all matching VMs, and lets `serve` clean the stack netns/TAP/forwards when the last VM is gone.
- OCI and build artifact cache now default to `/tmp/gocracker/cache`, so repeat runs should not need extra cache flags in the common case.
- This mode still assumes `compose` and `serve` are operating on the same host, because the stack bridge/netns/TAP plumbing is host-local and now intentionally owned by the API server.

Example:

```bash
./gocracker serve --addr :8080 --cache-dir /tmp/gocracker/cache

sudo ./gocracker compose \
  --server http://127.0.0.1:8080 \
  --file tests/manual-smoke/fixtures/compose-todo-postgres/docker-compose.yml \
  --kernel ./artifacts/kernels/gocracker-guest-standard-vmlinux \
  --cache-dir /tmp/gocracker/cache
```

Inspecting the stack by API:

```bash
curl http://127.0.0.1:8080/vms?orchestrator=compose
curl http://127.0.0.1:8080/vms?orchestrator=compose\&stack=compose-todo-postgres
curl http://127.0.0.1:8080/vms?orchestrator=compose\&stack=compose-todo-postgres\&service=todo
```

Docker-style exec by service name:

```bash
sudo ./gocracker compose \
  --server http://127.0.0.1:8080 \
  --file tests/manual-smoke/fixtures/compose-exec/docker-compose.yml \
  --kernel ./artifacts/kernels/gocracker-guest-standard-vmlinux \
  --cache-dir /tmp/gocracker/cache

curl http://127.0.0.1:8080/vms?orchestrator=compose\&stack=compose-exec\&service=debug

./gocracker compose exec \
  --server http://127.0.0.1:8080 \
  --file tests/manual-smoke/fixtures/compose-exec/docker-compose.yml \
  debug -- echo compose-exec-ok
```

If the command prints `compose-exec-ok`, the CLI resolved the service through `serve` and executed the command inside the guest without asking for keys, user, socket, or VM id.

Direct API exec by VM id:

```bash
curl -fsS http://127.0.0.1:8080/vms

curl -fsS -X POST http://127.0.0.1:8080/vms/<vm-id>/exec \
  -H 'Content-Type: application/json' \
  -d '{"command":["echo","api-exec-ok"]}'
```

If the response contains `api-exec-ok`, the guest agent executed the command through `serve` without any guest IP, SSH key, or user setup.

### API Server

```bash
# Start API server
./gocracker serve --addr :8080
```

```bash
# Build + boot in one call
curl -X POST http://localhost:8080/run \
  -d '{
    "image": "ubuntu:22.04",
    "kernel_path": "./artifacts/kernels/gocracker-guest-standard-vmlinux",
    "mem_mb": 256
  }'

# Build only (no boot)
curl -X POST http://localhost:8080/build \
  -d '{"image": "alpine:latest"}'

# List VMs
curl http://localhost:8080/vms

# List only Compose VMs from one stack
curl 'http://localhost:8080/vms?orchestrator=compose&stack=compose-todo-postgres'

# Stop VM
curl -X POST http://localhost:8080/vms/{id}/stop

# Snapshot
curl -X POST http://localhost:8080/vms/{id}/snapshot \
  -d '{"dest_dir": "/tmp/snap1"}'

# Restore
sudo ./gocracker restore --snapshot /tmp/snap1 --cpus 4

# Live migration (stop-and-copy)
./gocracker migrate \
  --source http://localhost:8080 \
  --id vm-123 \
  --dest http://host-b:8080
```

### Firecracker-compatible API

```bash
# Configure and boot step-by-step (Firecracker protocol)
curl -X PUT http://localhost:8080/boot-source \
  -d '{"kernel_image_path": "./artifacts/kernels/gocracker-guest-standard-vmlinux", "boot_args": "console=ttyS0"}'

curl -X PUT http://localhost:8080/machine-config \
  -d '{"vcpu_count": 1, "mem_size_mib": 128}'

curl -X PUT http://localhost:8080/drives/rootfs \
  -d '{"drive_id": "rootfs", "path_on_host": "/disk.ext4", "is_root_device": true}'

curl -X PUT http://localhost:8080/actions \
  -d '{"action_type": "InstanceStart"}'
```

## Project Structure

```
gocracker/
├── cmd/gocracker/         # CLI entrypoint
├── internal/
│   ├── api/               # REST API (Firecracker-compatible + extended)
│   ├── compose/           # Docker Compose parser + multi-VM orchestrator
│   ├── dockerfile/        # Dockerfile parser/executor
│   ├── fdt/               # Flattened Device Tree (DTB) generator
│   ├── guest/             # Guest init (PID 1) + initrd builder
│   ├── kvm/               # KVM syscall wrappers + CPU setup
│   ├── loader/            # bzImage + ELF kernel loader
│   ├── oci/               # OCI image puller + ext4 builder
│   ├── repo/              # Git repo cloner
│   ├── uart/              # 16550A UART emulation
│   ├── virtio/            # virtio MMIO transport + net + blk + rng
│   └── vsock/             # virtio-vsock (host↔guest streams)
├── pkg/
│   ├── container/         # High-level runtime: source → VM
│   └── vmm/               # VMM core: kernel, devices, run loop, snapshot
└── tests/
    ├── examples/          # Example apps (hello-world, static-site, python-api)
    └── integration/       # Integration tests (requires KVM)
```

## Dependencies

| Package | Purpose |
|---------|---------|
| `github.com/compose-spec/compose-go/v2` | Docker Compose parser + normalization |
| `github.com/go-chi/chi/v5` | HTTP router |
| `github.com/google/go-containerregistry` | OCI registry client |
| `github.com/klauspost/compress` | zstd decompression (kernels + OCI layers) |
| `github.com/moby/buildkit` | Dockerfile parser / AST |
| `github.com/ulikunitz/xz` | xz/lzma decompression (kernels) |
| `github.com/pierrec/lz4/v4` | lz4 decompression (kernels) |
| `github.com/cavaliergopher/cpio` | cpio archive creation (initrd) |
| `golang.org/x/sys` | Linux syscalls |
| `gopkg.in/yaml.v3` | YAML helpers outside the primary Compose parser path |

## Entropy / Random Seeds

gocracker includes a **virtio-rng** device backed by the host's `/dev/urandom` (via Go's `crypto/rand`). This means databases (MySQL, PostgreSQL), TLS, and any other software that requires entropy will work out of the box — no extra configuration or API calls needed.

## Code Review

Internal audit of the codebase covering security, bugs, code quality, and release readiness. Severity labels follow the standard CRITICAL / HIGH / MEDIUM / LOW scale.

### Security

#### CRITICAL

| ID | Summary | Status | Evidence |
|----|---------|--------|----------|
| S1 | No bounds-check on guest memory access | Closed (2026-04-09) | `GuestRead()` / `GuestWrite()` now reject overflow and out-of-bounds ranges in `internal/virtio/virtio.go`, with regression coverage in `internal/virtio/virtio_test.go`. |
| S2 | Infinite loop in descriptor chain traversal | Closed (2026-04-09) | `WalkChain()` now enforces queue bounds, cycle detection, per-descriptor limits, and chain-size caps in `internal/virtio/virtio.go`. |
| S3 | No authentication on REST API | Closed (2026-04-09) | `serve` now supports bearer-token auth and trusted path policies for kernel/workspace/snapshot inputs; loopback-only unauthenticated TCP remains allowed by explicit bind choice. |
| S4 | No seccomp / AppArmor profile | Closed / stale | `internal/seccomp` is already wired into the worker/VMM launch path; this audit item no longer reflects the current code. |

#### HIGH

| ID | Summary | Status | Evidence |
|----|---------|--------|----------|
| S5 | Path traversal in OCI tar extraction | Closed (2026-04-09) | OCI extraction now normalizes layer entry paths, validates link targets, and enforces rootfs containment in `internal/oci/oci.go`, with coverage in `internal/oci/oci_test.go`. |
| S6 | Race condition in virtio-net rxPump | Closed (2026-04-09) | `virtio-net` queue access now follows the same locked/error-aware queue discipline as the other virtio devices. |
| S7 | TOCTOU in jailer directory creation | Closed (2026-04-09) | The jailer now uses no-symlink directory creation and `O_NOFOLLOW` destination handling for copied files in `internal/jailer/jailer.go`. |
| S8 | No kernel signature verification | Closed (2026-04-09) | The API server now enforces trusted kernel directories instead of accepting arbitrary kernel paths on `serve`. |

#### MEDIUM

| ID | Summary | Status | Evidence |
|----|---------|--------|----------|
| S9 | Uncapped descriptor length in virtio-blk/net | Closed / stale | `WalkChain()` now caps per-descriptor length and total chain bytes in `internal/virtio/virtio.go`, so the audit finding no longer matches the current runtime. |
| S10 | Command arguments from interface names | Open | `exec.Command` prevents shell injection, but interface names are not strictly validated against an allowlist in `internal/hostnet/auto.go`. |
| S11 | Snapshot path not validated | Open | `SnapshotDir` still needs stronger validation/containment checks in `internal/api/api.go`. |
| S12 | Cgroup key injection | Open | User-provided cgroup keys still need stricter validation in `internal/jailer/jailer.go`. |

### Bugs and Code Quality

#### HIGH

| ID | Summary | Status | Evidence |
|----|---------|--------|----------|
| B1 | vCPU mmap regions never unmapped | Closed (2026-04-09) | `internal/kvm/kvm.go` now tracks and unmaps vCPU run regions via `VCPU.Close()`. |
| B2 | Guest memory leak on error path | Closed (2026-04-09) | KVM VM creation now cleans up guest mappings and memfd state on failure paths. |
| B3 | FD leak in virtio-fs eventfd creation | Closed (2026-04-09) | `internal/virtio/fs.go` now allocates eventfds transactionally and closes partial allocations on error. |
| B4 | Flush error silently ignored in virtio-blk | Closed (2026-04-09) | `virtio-blk` now propagates `Sync()` failures back to the guest as I/O error status. |

#### MEDIUM

| ID | Summary | Status | Evidence |
|----|---------|--------|----------|
| B5 | Double-close panic in `Stop()` | Open | `Stop()` still needs a final audit against double-close paths in `pkg/vmm/vmm.go`. |
| B6 | rxPump exits on any error | Closed (2026-04-09) | `virtio-net` now retries transient TAP read errors (`EINTR`, `EAGAIN`, `ENOBUFS`) and only exits on shutdown/fatal errors, with regression coverage in `internal/virtio/net_test.go`. |
| B7 | Pause/Resume TOCTOU race | Open | `Pause()` / `Resume()` still need stronger state-transition hardening in `pkg/vmm/vmm.go`. |
| B8 | Queue size not validated | Closed / stale | Queue size is now normalized and rejected when `0` or `> 256` in `internal/virtio/virtio.go`, so the old finding is stale. |

#### LOW

| ID | Summary | Location | Description |
|----|---------|----------|-------------|
| B9 | Duplicated networking code | `internal/hostnet/auto.go`, `internal/compose/network.go` | Nine identical functions (`occupiedIPv4Networks`, `selectAvailableSubnet`, `subnetAt`, `firstHostIP`, `gatewayCIDR`, `overlapsAny`, `cidrOverlap`, `normalizeIPv4Net`, `runIP`). Should be extracted to a shared package. |
| B10 | Missing context cancellation in API goroutines | `internal/api/api.go:441-450` | Background goroutines spawned from HTTP handlers do not respect the request context. |

### Release Readiness

| ID | Item | Status | Priority |
|----|------|--------|----------|
| D1 | LICENSE file | Present | CRITICAL |
| D2 | CI/CD (GitHub Actions) | Present (`.github/workflows/ci.yml`) | HIGH |
| D3 | CONTRIBUTING.md | Present | HIGH |
| D4 | SECURITY.md | Present | HIGH |
| D5 | CHANGELOG.md | Present | HIGH |
| D6 | .gitignore completeness | Incomplete (`.vscode/`, `.idea/`, `*.swp`, `.DS_Store`, `.tmp/`) | MEDIUM |
| D7 | System requirements table in README | Missing (Linux-only, KVM, `CAP_NET_ADMIN`) | MEDIUM |
| D8 | CODE_OF_CONDUCT.md | Missing | MEDIUM |

### Action Plan

Fixes are grouped into phases so that the most dangerous issues are resolved first.

**Closed in the 2026-04-09 hardening pass**
1. Bounds-check `GuestRead` / `GuestWrite` (S1)
2. Cycle detection + max chain length in `WalkChain` (S2)
3. Path traversal fix in `applyTar` (S5)
4. Cap descriptor lengths in queue walking (covers prior S9 risk)
5. `Munmap` vCPU regions on cleanup (B1)
6. Guest memory cleanup on error path (B2)
7. FD leak fix in virtio-fs (B3)
8. Propagate flush error in virtio-blk (B4)
9. Queue-size validation (prior B8)
10. API auth + trusted-path policy for `serve` (S3, S8)
11. Jailer no-symlink copy/create path (S7)
12. Release-readiness files and CI scaffolding (D1-D5)

**Still open / future work**
13. Pause/Resume TOCTOU (B7)
14. Deduplicate networking code (B9)
15. Context cancellation in API handlers (B10)
16. Firecracker-parity `virtio-mem` support and snapshot/migration compatibility for hotplugged VMs

## Known Broken Upstream

These manifest entries do **not** pass on a clean run, and the failure is in the
upstream Dockerfile / repository, not in gocracker. They live in
[`tests/external-repos/historical-unstable.ids`](tests/external-repos/historical-unstable.ids)
and the runner skips them by default. Each entry has been re-verified after the
runtime fixes listed above; the failure modes below are what stops them from
booting.

**Dockerfile expects a pre-built binary in the build context** (release-shaped
Dockerfile, requires the publishing CI to drop a binary into the checkout
before `docker build`):

- `minio-minio`, `prometheus-node-exporter`, `prom-prometheus`,
  `hashicorp-http-echo`, `chartmuseum-chartmuseum`, `etcd-io-etcd`,
  `node-express-realworld` (`COPY dist/api api`), `whoogle` (`COPY whoogle.env*`)

**No Dockerfile at the manifest path** (upstream moved or removed it):

- `caddyserver-caddy`, `gotenberg-gotenberg`, `woodpecker-ci-woodpecker`,
  `cockroachdb-cockroach`, `seaweedfs-seaweedfs`, `redis-redis`,
  `valkey-valkey`, `memcached-memcached`, `haproxy-haproxy`,
  `eclipse-mosquitto`, `emqx-emqx`, `dragonflydb-dragonfly`, `milvus-milvus`,
  `strapi`, `n8n`, `hoppscotch`, `pocketbase`, `docmost`

**Upstream build script broken with current toolchain or missing targets**:

- `mailhog-mailhog` — `package cmp` not in old Go toolchain
- `grafana-loki` — `make: No rule to make target 'loki'`
- `kong-kic` — `make: No rule to make target '_build'`
- `influxdata-telegraf` — postgres C build error
- `influxdata-influxdb` — fetches python tarball that no longer exists
- `ollama-ollama` — OCI layer apply network error
- `fastapi-realworld` — Poetry 1.1 broken on modern Python (`poetry.core.toml` removed)
- `phoenix-todo-list` — pinned to elixir 1.14, postgrex requires 1.15+
- `fiber-go-template` — pinned to `golang:1.23-alpine`, go.mod requires 1.24+
- `projectdiscovery-nuclei`, `schollz-croc` — pinned to `golang:1-alpine` (1.24), go.mod requires 1.25+
- `symfony-docker` — Dockerfile assumes `composer` is in PATH but never installs it
- `djangox` — `COPY --chown=app:app` references a user that is never created
- `fabio` — Dockerfile downloads consul.zip but `unzip` is not installed in the base

**Dockerfile uses `ARG BASE_IMAGE` / `${BASE}` without a default**:

- `open-policy-agent-opa`, `dagster-dagster`, `gradio-gradio`, `livebook`

**Build is too heavy / too slow / requires unusual resources**:

- `grafana-grafana` (>10 min, downloads glibc tarball at build time)
- `qdrant-qdrant`, `meilisearch-meilisearch`, `coredns-coredns`,
  `apache-apisix`, `rabbitmq-server`, `nats-server`, `quickwit-quickwit`,
  `mlflow-mlflow`, `bentoml-bentoml`, `locust-locust`, `celery-celery`,
  `jupyterhub-jupyterhub`, `jupyter-docker-stacks`, `metabase`, `directus`,
  `nextcloud-server`, `wallabag`, `librechat`, `chat-ui`, `superset`, `redash`,
  `papercups`, `pyload`, `znc`, `zammad`, `microblog`, `djangooscar`,
  `sickchill`, `wasmcloud`, `lnx`, `dnsx`, `zoekt`
- `streamlit-streamlit` — final rootfs >4 GiB, exceeds the `tar2ext4` 4 GiB limit

**Manifest path drift / Dockerfile lives under a subdirectory not pointed to**:

- `quickwit-quickwit`, `nats-server`, `rabbitmq-server`, `openfaas-faas`,
  `wger`, `plotly-dash`, `semaphore`, `uvicorn-gunicorn-fastapi`,
  `pixelfed` (when applicable), `navidrome` (cross-stage `--mount=bind,from=`
  to a path that does not exist in that stage)

If upstream fixes any of these, the entry can move back into
`tests/external-repos/historical-pass.ids` (or one of the `expanded*.ids`
sets) and the next sweep will pick it up automatically.
