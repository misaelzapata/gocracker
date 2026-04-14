# External-repos regression gate

Last updated: 2026-04-14

This directory defines a real-world regression gate for gocracker: each run boots
a microVM per listed repo (Dockerfile or compose) and verifies the guest reaches
`VM is running`. A PASS proves the full build + boot pipeline — OCI pull, layer
extraction, Dockerfile evaluation, rootfs materialization, kernel boot, and
guest init — end-to-end on real upstream code.

## Scope

| List | Count | Meaning |
| --- | --- | --- |
| [`historical-pass.ids`](historical-pass.ids) | 115 | Confirmed PASS in a single sweep on 2026-04-14 |
| [`historical-unstable.ids`](historical-unstable.ids) | 113 | Excluded from the gate with a documented reason (upstream drift, release-tarball pattern, heavy build, etc.) |
| [`everything.ids`](everything.ids) | 201 | Original manifest entries (historical + unstable combined) |

Additional 50 freshly curated repos were added to [`manifest.tsv`](manifest.tsv)
via `git ls-remote` on 2026-04-14 covering under-represented stacks (Elixir,
Haskell, OCaml, Zig, Crystal, Nim, Kotlin, Ruby, Deno, Nix, Gleam, V, Janet,
Idris, Racket, CL, etc.); pending validation.

## Methodology

Each candidate case in [`manifest.tsv`](manifest.tsv) is run with a fresh
cache via:

```bash
sudo -E env PATH="$PATH" ./gocracker repo \
    --url "$URL" --ref "$REF" --subdir "$SUBDIR" \
    --kernel artifacts/kernels/gocracker-guest-standard-vmlinux \
    --mem $MEM_MB --disk $DISK_MB \
    --tty off --jailer off \
    --cache-dir /tmp/gocracker-shared-cache
```

Success criteria: log contains `VM .* is running` or `started id=` before the
`boot_timeout` (default 900s). The shared cache dir amortizes base-image pulls
across sibling cases so a sweep completes within Docker Hub pull limits.

Harness: [`sweep.py`](sweep.py) — Python thread pool (concurrency 3) with
per-case logs under `/tmp/sweep/<id>.log` and a consolidated TSV of results.

## Current gate (2026-04-15, post-fix-batch)

```
./gocracker repo ...  →  126 PASS / 0 FAIL (single sweep, no retries)
```

Last revalidation included gocracker fixes: `USER` build-arg expansion
([internal/dockerfile/dockerfile.go](internal/dockerfile/dockerfile.go)),
`ARG NAME` declared-but-unset → empty-string env (BuildKit semantics, same
file `handleARG`), jailer worker rundir+drive chown
([internal/worker/vmm.go](internal/worker/vmm.go)), live-migrate vsock
quiesce ([pkg/vmm/migration.go](pkg/vmm/migration.go) FinalizeMigrationBundle),
discovery deterministic tie-break ([internal/discovery/discovery.go](internal/discovery/discovery.go)),
net rx/tx separate rate-limiter ([internal/virtio/net.go](internal/virtio/net.go),
[pkg/vmm/vmm.go](pkg/vmm/vmm.go) UpdateNetRateLimiters,
[internal/api/api.go](internal/api/api.go) handleVMNetRateLimiter envelope).
+1 manifest rescue: gotenberg-gotenberg via new `dockerfile=build/Dockerfile`
column.

### Snapshot / restore / exec bench (Firecracker-parity fixes landed 2026-04-15)

| flow | Node 20 alpine | Bun 1 alpine | note |
|---|---|---|---|
| cold → exec | 225 ms | 204 ms | baseline |
| **resume → exec (pool, warm server)** | **71 ms** | **36 ms** | pre-built snapshot on disk, restore+exec per iter |

Lower bound on resume+exec is dominated by the runtime's intrinsic cold-start
(`node -v` = ~45 ms, `bun --version` = ~15 ms); further reduction requires
pre-warming the runtime process inside the guest before the snapshot.

### Manual e2e verification (2026-04-15)

Each feature exercised with real commands, observed output, reported honestly:

| feature | result | evidence |
|---|---|---|
| compose multi-VM | PASS | `gocracker compose` boots 2 VMs, `POST /vms/{id}/exec hostname` returns `gocracker` for both |
| live-migrate | PASS | marker file written on server A, preserved and readable after `POST /vms/{id}/migrate` to server B |
| network (static IP + outbound) | PASS | guest eth0 gets `10.45.0.2/24`, `wget http://10.45.0.1:8090/` from guest returns host payload |
| balloon inflate | PASS | control VM `MemAvailable = 213 MB`; inflated VM (balloon=128 MiB) `MemAvailable = 82 MB`; Δ = 128 MiB exactly |
| vsock user-level | PASS | guest static Go listener on `AF_VSOCK:13000` echoes `ping\n` → `echo:ping` via host HTTP-upgrade at `/vms/{id}/vsock/connect` |
| rate-limiter (block) | PARTIAL | `PUT /vms/{id}/rate-limiters/block` returns 204 and accepts the token bucket; empirical bandwidth validation blocked by gocracker's default tmpfs overlay rootfs (writes absorbed before block device). Needs `--rootfs-persistent` or an attached extra drive to prove the limiter enforces |
| jailer=on | PASS | after fixing [internal/worker/vmm.go](internal/worker/vmm.go) to chown the worker run-dir AND every rw drive image to the configured UID before jailer spawn, the VMM child runs under UID 1000, `exec whoami` round-trips through the jailed worker, and `/run` returns 200. The original bug was the worker couldn't create `/worker/vmm.sock` (root-owned bind mount) or open `/worker/drives/0` (root-owned disk image) |

Manual test scripts at `/tmp/manual-tests/*.sh`. Each launches `gocracker serve`, hits the real API, observes the guest, kills everything on exit. Sixty-second timeout per script.

Covers: Go services, Rust CLIs, Python web stacks (Django, FastAPI, Flask),
Node/JS apps, Ruby (Rails), Elixir apps, PHP (BookStack), object storage
(Distribution Registry), databases (dendrite, litefs), observability
(Grafana Tempo), self-hosted platforms (outline, umami, Chatwoot, Gitea, Gogs,
Gitness), build tools, and more.

## Recent fixes that expanded the gate (this PR)

| Fix | Commit(s) | Repos rescued |
| --- | --- | --- |
| `subdir="."` walks the tree instead of exact-root-only lookup | `3ed2074` | grafana-tempo, chatwoot, envoy-envoy, httpbin, kestra-kestra, paperless-ngx, prefect-prefect, typesense-typesense, victoria-metrics, bookstack |
| OCI layer apply: symlinks with link target `..` (e.g. `usr/lib/llvm-14/build/Debug+Asserts -> ..`) no longer rejected as escape | pending | dufs-rs and any image based on `messense/rust-musl-cross` |
| Snapshot bundling: `TakeSnapshotWithOptions` now copies kernel/initrd/disk into the snapshot dir so restore works after the runs/ dir is cleaned | pending | (benchmark reliability, no gate impact) |

## Failure categories (for `historical-unstable.ids`)

1. **Upstream drift** — Dockerfile moved or deleted from manifest path.
2. **Release-tarball pattern** — Dockerfile expects a pre-built binary in the
   build context (minio, prometheus-node-exporter, hashicorp-http-echo, …).
3. **Upstream toolchain broken** — Go too old, Makefile rot (mailhog, grafana-loki,
   influxdata-telegraf, …).
4. **`ARG BASE` without default** — requires `--build-arg` to build (opa).
5. **Too heavy** — build time >10 min, usually bind pulls (grafana-grafana).
6. **NPM 404** — upstream dependency removed from registry (lobe-chat:
   `@react-pdf/svg` returns 404).

## Reproducing locally

```bash
# Build the binary
go build -o gocracker ./cmd/gocracker

# Configure Docker Hub auth so OCI pulls authenticate (otherwise you'll get
# rate-limited to 100 pulls / 6h as anon).
sudo mkdir -p /root/.docker
sudo tee /root/.docker/config.json > /dev/null <<EOF
{ "auths": { "https://index.docker.io/v1/": { "auth": "<base64 user:pat>" } } }
EOF

# Warm the shared cache
sudo mkdir -p /tmp/gocracker-shared-cache

# Run the gate
python3 tests/external-repos/sweep.py tests/external-repos/historical-pass.ids \
    --concurrency 3 --boot-timeout 900 \
    --log-dir /tmp/sweep --results /tmp/sweep.tsv
```

Expected: `PASS: 115 / FAIL: 0`.

## Related
- [`manifest.tsv`](manifest.tsv) — source of truth for every candidate (id, url, ref, subdir, mem, disk).
- [`sweep.py`](sweep.py) — harness.
