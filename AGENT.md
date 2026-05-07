# gocracker — agent guide

gocracker is a KVM micro-VM sandbox server with an MCP interface. When the
`gocracker` MCP server is connected, you can run code in isolated VMs with
sub-30 ms warm-pool restore — no Docker, no container runtimes, no shared
kernel.

## MCP tools

| Tool | Purpose |
|------|---------|
| `sandbox.lease` | Lease one warm sandbox from a template pool |
| `sandbox.fan_out` | Lease N sandboxes in parallel (wall time = one lease) |
| `sandbox.delete` | Release sandbox back to the pool |
| `sandbox.recycle` | Swap for a fresh sandbox from the same pool in one round trip |
| `process.exec` | Run any command inside a sandbox |
| `process.eval_node` | Eval JS against a pre-loaded V8 (skips ~25 ms node startup) |

## Typical workflow

```
sandbox.lease({ template_id: "base-node" })
  → { id: "gc-abc123", uds_path: "/var/lib/…/abc123.sock", guest_ip: "…" }

process.exec({ sandbox_id: "gc-abc123", cmd: ["node", "-e", "console.log(1+1)"] })
  → { stdout: "2\n", stderr: "", exit_code: 0, wall_ms: 8 }

sandbox.delete({ id: "gc-abc123" })
```

Use `sandbox.recycle` instead of delete + lease when you want a clean
filesystem but want to stay in the same template pool (one HTTP round trip
instead of two).

## Available templates

Templates are registered by sandboxd at startup. Common ones:

| Template ID | Runtime |
|-------------|---------|
| `base-node` | Node.js 20 · Alpine |
| `base-node-warm` | Node.js 20 · Alpine · V8 pre-loaded (use with `process.eval_node`) |
| `base-python` | Python 3.12 · Alpine |
| `base-go` | Go 1.22 · Alpine |
| `base-bun` | Bun · Alpine |

Check which pools are active: `GET http://127.0.0.1:9091/pools`.

## Latencies (warm pool, localhost)

| Operation | Typical |
|-----------|---------|
| `sandbox.lease` | ~30 ms p95 |
| `process.exec` (short command) | ~5–15 ms after lease |
| `process.eval_node` | ~24 ms in-guest (V8 pre-initialised) |
| `sandbox.fan_out(n=4)` | ~30 ms wall (same as one lease, 4× parallel) |
| `sandbox.recycle` | ~30 ms (release + fresh lease in one call) |

## Stateful JS across calls

`process.eval_node` persists globals in the same sandbox between calls:

```
eval_node({ sandbox_id, source: "global.db = require('better-sqlite3')('/tmp/data.db')" })
eval_node({ sandbox_id, source: "global.db.exec('CREATE TABLE …')" })
eval_node({ sandbox_id, source: "console.log(global.db.prepare('SELECT …').all())" })
```

Call `sandbox.recycle` to get a clean-state sandbox when you need a fresh V8.

## parallel execution with fan_out

```
sandbox.fan_out({ template_id: "base-node", n: 4 })
  → { sandboxes: [{id, uds_path}, …×4], wall_ms: 28, errors: [] }
```

Run a different hypothesis in each sandbox concurrently, take the first
successful result, delete the rest.

## Error handling

Tool-level errors return `isError: true` with the message in
`content[0].text`. They are not JSON-RPC errors. Check `result.isError`
before trying to parse the result JSON.

## Running without sudo

**kvm group + no flags needed:**

```bash
sudo usermod -aG kvm $USER   # one-time; log out and back in
# then all gocracker commands work as your normal user:
gocracker run --image alpine:latest --kernel "$KERNEL" --cmd "echo hi" --wait
```

gocracker auto-detects when it is not running as root and switches the
jailer to `off` mode (KVM isolation is still in effect). For network
access use `--net slirp` (no CAP_NET_ADMIN needed) or `--net none`
(default for `gocracker run`).

sandboxd also runs rootless:

```bash
gocracker-sandboxd serve --kernel-path "$KERNEL" --network-mode slirp
```

## Diagnosing "connection refused"

sandboxd is not running. Check:

```bash
systemctl status gocracker-sandboxd    # if installed as a service
# or start manually:
sudo gocracker-sandboxd serve --kernel-path /path/to/kernel
```

First lease after startup may take 2–5 s while the base template snapshot
is captured. Subsequent leases are fast.
