# gocracker Python SDK

Zero-dependency (stdlib-only) Python client for the gocracker
sandboxd HTTP control plane + the in-guest toolbox agent over UDS.

Warm-lease latency (measured 2026-04-22 on x86, pool of 8):

- `lease_sandbox` p95 = **1.5 ms**
- full `create → exec('echo') → delete` p95 = **~35 ms**

## Install

```bash
cd sandboxes/sdk/python
pip install -e .
```

(or copy the `gocracker/` directory into your project — no deps).

## Quick start (Daytona-style)

Start `gocracker-sandboxd` with `-kernel-path` (or `GOCRACKER_KERNEL` in
env) so the `base-python / base-node / base-bun / base-go` templates
auto-register at boot. Then:

```python
from gocracker import Client

client = Client("http://127.0.0.1:9091")

# Lease-style (with auto-delete on context exit):
with client.create_sandbox(template="base-python") as sb:
    r = sb.process.exec('python3 -c "print(2+2)"')   # → ProcessExitError on non-zero
    print(r.stdout_text.strip())                      # "4"

    sb.fs.write_file("/tmp/hi.txt", b"hello")
    print(sb.fs.read_file("/tmp/hi.txt"))             # b"hello"
    print([e.name for e in sb.fs.list_dir("/tmp")])   # ["hi.txt"]

    url = sb.preview_url(8080)                        # signed preview URL
    print(url)
# sandbox auto-deleted here
```

Low-level / escape hatches still work:

```python
# Raw create with image + kernel (no template)
sb = client.create_sandbox(image="alpine:3.20", kernel_path="/abs/vmlinux")
sb.toolbox().exec(["echo", "hi"])   # the v3 flat surface is still present
sb.delete()
```

## Surface

### Client

| Method | What |
|---|---|
| `create_sandbox(template=, image=, kernel_path=, ...)` | Cold-boot or template-restore. `template=` takes precedence when set. |
| `lease_sandbox(template_id)` | Warm lease from a registered pool (<5 ms). |
| `list_sandboxes / get_sandbox / delete(id)` | Inventory + teardown. |
| `register_pool / list_pools / unregister_pool` | Warm pool lifecycle. |
| `create_template / list_templates / get_template / delete_template` | Template (content-addressed snapshot). |
| `mint_preview(id, port)` | Signed preview URL. |
| `healthz()` | Liveness of sandboxd. |

### `Sandbox` (returned by create / lease)

| Method | What |
|---|---|
| `sb.process.exec(cmd)` | Synchronous exec. `cmd` may be `str` (wrapped in `sh -c`) or `list[str]`. Non-zero exit → `ProcessExitError`. |
| `sb.process.exec_stream(cmd)` | Iterator of `(channel, bytes)` frames — channel 1 = stdout, 2 = stderr, 0 = exit. |
| `sb.process.start(cmd)` | Alias for `exec_stream` — kept for Daytona familiarity. |
| `sb.fs.write_file(path, bytes)` | Upload. |
| `sb.fs.read_file(path) -> bytes` | Download. |
| `sb.fs.list_dir(path) -> list[FileEntry]` | Directory listing. |
| `sb.fs.remove(path)` / `mkdir` / `chmod(path, mode)` / `rename(src, dst)` | Canonical file ops. |
| `sb.preview_url(port) -> str` | Absolute URL with signed token. |
| `sb.delete()` | Explicit teardown (async on the server side). |
| `with sb: ...` | Auto-delete on context exit. |
| `sb.toolbox()` | Access the flat low-level toolbox client (escape hatch). |

### Typed errors

```
SandboxError
├── SandboxNotFound        # 404 on sandbox / pool / template route
├── SandboxInvalidRequest  # 400
├── SandboxConflict        # 409 (template id taken, etc.)
├── ProcessExitError       # sb.process.exec(cmd) → exit != 0; carries .exit_code + .stdout + .stderr
├── TemplateNotFound       # create_sandbox(template=X) where X is unknown
├── PoolExhausted          # lease_sandbox against an empty pool (rare)
├── RuntimeUnreachable     # sandboxd can't reach gocracker runtime
└── SandboxTimeout         # operation exceeded its deadline
```

## Cookbook

`sandboxes/examples/python/cookbook/` — 10 end-to-end scripts. All
pass against the current `main` (measured 2026-04-22):

| Script | Feature |
|---|---|
| `hello_world.py` | create + exec |
| `exec_stream.py` | streaming stdout/stderr |
| `files.py` | upload + download + list |
| `files_full.py` | mkdir / chmod / rename / delete |
| `preview.py` | mint + HTTP proxy through the agent |
| `secrets.py` | per-sandbox secret set/list/delete (never hits disk) |
| `pool_burst.py` | N concurrent leases, p95 histogram |
| `template_pool.py` | template → pool → lease (≤3 ms p95) |
| `concurrent_cold.py` | N parallel cold-boots |
| `git_clone.py` | toolbox's `git clone` + `git status` |

## SDK parity

Same surface in `sandboxes/sdk/go/` and `sandboxes/sdk/js/` — any
example in the cookbook has a one-to-one translation in Go and JS.
See the respective READMEs.
