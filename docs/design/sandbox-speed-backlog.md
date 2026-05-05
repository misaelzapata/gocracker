# Sandbox speed backlog

Captured during the multi-agent analysis on
`feat/slirp-net-and-atomic-disk-meta`. Items shipped on that branch are
listed for context; the rest are deferred work, ordered by ROI.

## Already shipped on the branch

| Optimization | Where | Headline |
| --- | --- | --- |
| Atomic snapshot/migration metadata writes | [pkg/vmm/migration.go](../../pkg/vmm/migration.go) | tmp+rename + fsync of file & parent dir |
| sync.Cond pause/resume signaling | [pkg/vmm/vmm.go](../../pkg/vmm/vmm.go) | Resume worst-case 67 µs vs ~10 ms before (150×) |
| Quiesce post-sleep elimination | [sandboxes/internal/templates/builder.go](../../sandboxes/internal/templates/builder.go) | 50 ms → 1 ms per readiness-snapshot template build |
| Readiness probe exponential backoff | [sandboxes/internal/templates/builder.go](../../sandboxes/internal/templates/builder.go) | 500 ms fixed tick → 1→100 ms ramp |
| `conn.Write` coalesce in toolbox client | [internal/toolbox/client/client.go](../../internal/toolbox/client/client.go) | One `writev(2)` instead of two host syscalls per RPC |
| Pool O(1) FIFO buckets | [sandboxes/internal/pool/pool.go](../../sandboxes/internal/pool/pool.go) | O(n) entries-map scan → list.Front() per Acquire |
| `ReconcileInterval` default 500 → 50 ms | [sandboxes/internal/pool/pool.go](../../sandboxes/internal/pool/pool.go) | Burst-empty refill convergence |
| `socketWaitStep` 25 → 1 ms | [internal/worker/vmm.go](../../internal/worker/vmm.go) | Worker-startup polling fallback |
| Async lease-timing log | [sandboxes/internal/pool/pool.go](../../sandboxes/internal/pool/pool.go) | Off the request goroutine; helps p99 jitter |

## Deferred — high ROI, medium effort

### B1. mem.bin restore via `mmap`

**Where:** `pkg/vmm/migration.go writeMemoryFile` and the restore-side
`os.ReadFile` of `mem.bin`.

**Problem:** restore reads the full memory blob synchronously into a
single `[]byte` then copies it into the KVM-mapped guest RAM region.
For 256 MiB+ guests this stalls the restore path for tens of ms even
with hot OS page cache.

**Fix:** `mmap(MAP_PRIVATE)` the snapshot's `mem.bin` directly into the
guest physical address space. The kernel demand-pages on first touch.
Cost: a mmap + careful lifetime around `kvmVM.Close`.

**Risk:** medium — interactions with KVM dirty-page tracking and with
copy-on-write semantics for write-back if the snapshot is mutated by
the restored VM.

**Expected impact:** -5–20 ms on warm-cache restore paths, larger for
bigger guests.

### B2. Toolbox client conn pooling

**Where:** [internal/toolbox/client/client.go:343](../../internal/toolbox/client/client.go).

**Problem:** every `Exec`, `SetNetwork`, etc. opens a fresh UDS, sends
"CONNECT 10023\n", reads "OK\n", then runs HTTP/1.0. The dial+CONNECT
handshake is sub-millisecond on its own, but at 20 execs per
short-lived sandbox the total adds up.

**Decision documented as intentional** in the package comment: "There
is no connection pool — UDS+CONNECT is sub-millisecond and pooling
would just hide per-conn lifetime bugs."

**Reversal cost:** medium. A keep-alive pool keyed by `(udsPath, port)`
with TTL ~500 ms covers the burst case without growing live conns
arbitrarily. Singleflight on `dialAndConnect` handles concurrent
fan-out.

**Expected impact:** -5–10 ms per sandbox in the "lease + N execs"
shape; -0 ms in single-exec lease shape.

### B3. Initrd cache key under burst contention

**Where:** [pkg/container/container.go shouldReuseCachedInitrd](../../pkg/container/container.go).

**Problem:** the cache key is the on-disk `specPath`, not a content
hash. Concurrent cold-boots that derive the same spec rebuild the
initrd N times instead of N-1 cache hits + 1 build.

**Fix:** SHA-256(InitDigest || GuestSpecJSON) → in-memory map (LRU). A
small `sync.Map` indexed by content hash is enough.

**Expected impact:** -50–150 ms × (N-1) on N concurrent cold-boots
sharing a spec.

## Deferred — medium ROI, high effort

### C1. OCI layer extract in parallel

**Where:** [internal/oci/oci.go:351](../../internal/oci/oci.go).

**Problem:** layers extracted strictly serially. Multi-MB images pay
N × extract latency.

**Fix:** pipeline layers through goroutines — but **whiteouts cross
layer**, so naive parallelism corrupts the rootfs. Need either
per-layer staging dirs + a final merge, or a topological order where
parallelism is bounded to "layers without whiteout dependencies."

**Risk:** high — easy to get wrong; correctness regression worse than
the latency win.

**Expected impact:** -200–800 ms cold-pull on multi-MB images.

### C2. ext4 build overlapped with OCI extract

**Where:** [pkg/container/container.go](../../pkg/container/container.go) and [internal/oci/oci.go BuildExt4](../../internal/oci/oci.go).

**Problem:** mkfs runs after extract finishes. They could partially
overlap (mkfs the empty image while extract continues; populate via
a streaming tar pipe).

**Fix:** non-trivial refactor to a streaming pipeline.

**Expected impact:** -500–1000 ms cold path on big images.

## Deferred — low ROI

### D1. Boot fork+exec worker subprocess overlap

Pre-allocate the worker run dir + chown in a goroutine while the
kernel decompresses. Saves a few ms; risk of races in chroot setup.

### D2. Targeted wake-up for `AcquireWait`

Replace `signalWarmAvailableLocked` close-and-replace with a counted
semaphore, so refill events wake exactly N waiters instead of all.
Helps thundering herd; low impact in practice.

## Notes on benchmarking

The pool-bench tool with `-exec` is the simplest end-to-end
comparison; SDK-side `tti_node.py` covers the customer-visible TTI but
needs an external sandboxd. Both are documented in
[../../tools/](../../tools/) and
[../../sandboxes/examples/python/bench/](../../sandboxes/examples/python/bench/).
Numbers are sensitive to system load and warmup; run 10+ iterations
after a clean state.
