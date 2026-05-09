# gocracker performance snapshot — 2026-05-06

Reference host: AMD Ryzen AI 9 HX 370 (24 threads), Linux 6.17,
`/dev/kvm` available. Guest kernel:
`artifacts/kernels/gocracker-guest-minimal-vmlinux` (Linux 6.1.102,
trimmed initcalls). Image: `node:20-alpine`. Bench harnesses pin
gocracker to CPU 0 with `taskset -c 0` and SCHED_FIFO 50 via `chrt`.

All numbers below are wall-clock from process invocation (or HTTP
request) to first stdout byte starting with `v` (matching the
ComputeSDK leaderboard's `node -v` methodology).

## Summary table

| Path | What's measured | Median | p95 | Min | Max |
|---|---|---:|---:|---:|---:|
| **Cold-CLI** (`gocracker run -dockerfile`) | Real cold create. OCI cache hit, fresh kernel boot, alpine init, `node -v` exec | **240 ms** | 250 ms | 231 ms | 250 ms |
| **WARM-CLI** (`gocracker run -warm`) | Snapshot-pool restore + fork+exec `node -v`. Equivalent to what Daytona/Vercel/E2B publish | **71 ms** | 78 ms | 67 ms | 78 ms |
| **NODE-WARM-CLI** (`gocracker run -warm -warm-runtime node -cmd 'node-warm <JS>'`) | Snapshot-pool restore + JS eval against pre-loaded V8 (Pieza B) | **70 ms** | 77 ms | 63 ms | 77 ms |
| **POOL-PRIMITIVE** (`pool-bench /bin/true`) | Daemon-mode lease + exec a no-op binary. Pure VMM lifecycle. | **7 ms** | 7 ms | 7 ms | 7 ms |
| **POOL-PRIMITIVE** (`pool-bench node -v`) | Daemon-mode lease + fork+exec node | **37 ms** | 38 ms | 37 ms | 38 ms |

## Where the ms go (trace breakdown)

`gocracker run -warm -warm-runtime node` against a warm snapshot, full
trace from `cmd_run_enter` to first stdout byte:

```
node-warm REPL eval                       regular fork+exec node -v
─────────────────────────                 ─────────────────────────
t=+0.0 ms  cmd_run_enter                  t=+0.0 ms  cmd_run_enter
t=+0.0 ms  flags_parsed                   t=+0.0 ms  flags_parsed
t=+0.0 ms  container_run_begin            t=+0.0 ms  container_run_begin
t=+1.9 ms  container_run_done             t=+1.9 ms  container_run_done
t=+1.9 ms  warm_cmd_begin (node-warm)     t=+1.9 ms  warm_cmd_begin (node)
t=+25.5 ms warm_cmd_done                  t=+33.3 ms warm_cmd_done

guest exec:  ~24 ms                       guest exec:  ~31 ms (V8 init)
in-process:  ~26 ms                       in-process:  ~33 ms
wall-clock TTI dominated by host          wall-clock TTI dominated by
process startup (~35-40 ms from           same host startup, +~9 ms
sudo + Go init + arg parse, NOT           more in-guest for V8 init.
visible to the trace anchor)              ────────────────────────
                                          node-warm saves ~9 ms (~28%)
```

The trace anchor is "first Event() call inside gocracker" — it does
NOT capture pre-`main()` cost (sudo cred check, Go runtime init, flag
parse). That accounts for the ~35-40 ms gap between trace `t=+25.5 ms`
and bench wall-clock 70 ms.

For **daemon-mode callers** (sandboxd lease + exec via SDK or MCP),
the host startup is amortised and only the trace numbers apply:

```
pool-bench /bin/true:    7 ms total  (lease ~3 ms + exec ~4 ms)
pool-bench node -v:     37 ms total  (lease ~3 ms + node fork+exec ~34 ms)
projected node-warm:   ~10 ms total  (lease ~3 ms + UDS dial + REPL eval ~7 ms)
```

The projected ~10 ms for node-warm via daemon-mode is what the
gocracker-mcp `process.eval_node` tool will land at once it's wired
to a real sandboxd warm pool with `base-node-warm`. The bottleneck
shifts from "V8 startup" to "JSON-RPC framing + UDS dial".

## How everything fits together (architecture)

```
┌──────────────────┐   JSON-RPC 2.0    ┌──────────────────┐    HTTP    ┌──────────────────┐
│  Claude Desktop  │ ◄── stdio ──────► │  gocracker-mcp   │ ─────────► │ gocracker-       │
│  any MCP client  │  (this branch)    │  (5 tools)       │            │   sandboxd       │
└──────────────────┘                   └──────────────────┘            └────────┬─────────┘
                                                                                │
                                                                       ┌────────▼──────────┐
                                                                       │  warm pool of     │
                                                                       │  N paused VMs     │
                                                                       │  (3-5 ms restore  │
                                                                       │  via dirty-delta  │
                                                                       │  CoW from snap)   │
                                                                       └────────┬──────────┘
                                                                                │ vsock UDS
                                                                       ┌────────▼──────────┐
                                                                       │ guest VM          │
                                                                       │  + toolbox agent  │
                                                                       │  + node REPL      │
                                                                       │    on UDS         │
                                                                       │   (V8 already     │
                                                                       │    initialised)   │
                                                                       └───────────────────┘
```

When the AI calls `process.eval_node({sandbox_id, source})`:

1. MCP server JSON-decodes the request.
2. Looks up sandbox UDS path via `Client.GetSandbox`.
3. Constructs `ToolboxClient.Exec(["node-warm", source])` —
   this routes to the warm runner, NOT a fresh fork+exec.
4. Toolbox agent inside the guest dials the local
   `/run/gocracker/warm-node.sock`, sends the JS source as a
   length-delimited JSON request to the long-lived REPL.
5. REPL `vm.runInContext`s the source against a persistent sandbox
   object, captures stdout/stderr.
6. Result frames come back to the agent → host → MCP client.

State (globals, requires) persists across calls — useful for stateful
AI loops (`global.x = 1`, then later `console.log(global.x)`).

## Why the warm-pool snapshot has the runner pre-booted

The toolbox agent's `Serve()` startup spawns the embedded
`node-repl-server.js` as a subprocess. The agent then exposes
`GET /runtime/node/ready`. The snapshot capture path
(`captureWarmSnapshot` in `pkg/container/warmcache.go`) waits for
that endpoint to return 200 before snapshotting — guaranteeing the
captured memory image has V8 initialised and the REPL bound on its
UDS.

This is the [Lambda SnapStart](https://aws.amazon.com/blogs/compute/under-the-hood-how-aws-lambda-snapstart-optimizes-function-startup-latency/)
/ [Modal Memory Snapshots](https://modal.com/blog/mem-snapshots) pattern
adapted to gocracker: snapshot AFTER init, BEFORE handler, and
restore lands you in a hot V8 instead of paying the cold-start tax
on every request.

## Comparison vs other public sandbox products

| Product | Tech | Cold create | Warm-pool / restore | In-memory snapshot? | Pre-loaded runtime? |
|---|---|---:|---:|:-:|:-:|
| **gocracker** (this branch) | Go + KVM, virtio-mmio, dirty-page delta | 240 ms | **7 ms primitive / 37 ms node -v / 70 ms wall-CLI / ~10 ms proj. via MCP** | yes | **yes (node-warm)** |
| **boxlite** | Rust + libkrun (KVM/HVF), gvproxy | "<50 ms" claimed (unbenchmarked) | n/a — **no in-memory snapshot/restore**, restore = cold reboot from frozen disk | no | no |
| **E2B** | Firecracker fork + UFFD + OverlayFS | ~440 ms (ComputeSDK) | "near-zero" claimed for warm, no public bench | yes (UFFD) | no |
| **Daytona** | sysbox-runc Docker-in-Docker + WarmPoolService | ~100 ms (ComputeSDK) | "ms-scale" assignment from pool | n/a (filesystem-level) | filesystem only |
| **Vercel Sandbox** | Firecracker on Hive | ~380 ms (ComputeSDK) | not public | not public | no |
| **Cloudflare Code Mode** | V8 isolate (no VM) | <5 ms | n/a (stateless) | n/a | yes (V8) but no FS / proc |
| **Modal Memory Snapshots** | gVisor + CRIU + FUSE preload | varies | claimed 3-10× speedup | yes | yes (`@modal.enter(snap=True)`) |
| **Arrakis** | cloud-hypervisor | not published | snapshot/restore for backtracking | yes | no |
| **node-vmm** (your earlier project) | Node.js + KVM | ~1 s | n/a | not yet | no |

Sources:
- [ComputeSDK Benchmarks](https://www.computesdk.com/benchmarks/)
- [boxlite docs](https://boxlite.ai) and [acmerfight gist (source-verified)](https://gist.github.com/acmerfight/d577b9463e862adcffa64eef5f572188)
- [E2B's UFFD blog](https://e2b.dev/blog/scaling-firecracker-using-overlayfs-to-save-disk-space)
- [Daytona's WarmPoolService](https://deepwiki.com/daytonaio/daytona/4.1-creating-sandboxes)
- [Modal's snapshot blog](https://modal.com/blog/mem-snapshots)
- [Arrakis](https://github.com/abshkbh/arrakis)
- [Cloudflare Code Mode](https://blog.cloudflare.com/code-mode-mcp/)

## Reproduce

The numbers above are produced by:

```bash
# 1. cold-CLI baseline
./tools/bench-node-tti.sh 10

# 2. WARM CLI (snapshot-pool, fork+exec node)
WARM=1 ./tools/bench-node-tti.sh 10

# 3. node-warm REPL eval (the differentiator)
sudo bin/gocracker run -image node:20-alpine \
    -kernel artifacts/kernels/gocracker-guest-minimal-vmlinux \
    -mem 256 -net none -jailer off -wait -warm -warm-runtime node \
    -cmd 'node-warm process.stdout.write(process.version)'

# 4. pool primitives (daemon-mode SDK / MCP path)
sudo bin/pool-bench -burst 1 -warm 2 -p95-budget-ms 100 -exec "/bin/true"
sudo bin/pool-bench -image node:20-alpine -burst 1 -warm 2 \
    -p95-budget-ms 1000 -exec "node -v"

# 5. trace the timeline of any of the above
sudo -E env GOCRACKER_TRACE=1 bin/gocracker run ... 2>&1 | grep "^\[trace\]"
```

All sources and the trace package are at the repo head, branch
`feat/slirp-net-and-atomic-disk-meta` (perf foundation, [PR #22](https://github.com/misaelzapata/gocracker/pull/22))
plus `feat/mcp-server` ([PR #23](https://github.com/misaelzapata/gocracker/pull/23), stacked).
