# gocracker Python SDK

Zero-dependency (stdlib-only) Python client for the gocracker
sandboxd HTTP control plane + the in-guest toolbox agent over UDS.

## Install

```bash
cd sandboxes/sdk/python
pip install -e .
```

(or copy the `gocracker/` directory into your project)

## Quick start

```python
from gocracker import Client

client = Client("http://127.0.0.1:9091")

# Cold-boot a sandbox
sb = client.create_sandbox(
    image="alpine:3.20",
    kernel_path="/abs/path/to/vmlinux",
)

# Talk to the toolbox agent inside the guest
result = sb.toolbox().exec(["echo", "hello"])
print(result.stdout_text)   # "hello\n"
print(result.exit_code)      # 0

sb.delete()
```

## Surface

| Method | Route | What |
|---|---|---|
| `create_sandbox` | POST /sandboxes | Cold-boot VM |
| `list_sandboxes` | GET /sandboxes | All sandboxes |
| `get_sandbox(id)` | GET /sandboxes/{id} | One by id |
| `delete(id)` | DELETE /sandboxes/{id} | Teardown |
| `register_pool` | POST /pools | Warm pool |
| `list_pools` / `unregister_pool` | GET/DELETE /pools | Pool lifecycle |
| `lease_sandbox` | POST /sandboxes/lease | Warm lease (~17 ms p95) |
| `create_template` | POST /templates | Template with SpecHash cache |
| `list_templates` / `get_template` / `delete_template` | ... /templates | Template lifecycle |
| `mint_preview` | POST /sandboxes/{id}/preview/{port} | Signed URL |
| `healthz` | GET /healthz | Liveness |

Toolbox (`sb.toolbox()`):

| Method | What |
|---|---|
| `health()` | `/healthz` on the guest agent |
| `exec(cmd, ...)` | Block-collect stdout/stderr/exit |
| `exec_stream(cmd, ...)` | Yield frames as they arrive (line-streaming, SSE) |
| `list_files(path)` | Guest filesystem listing |
| `download(path)` / `upload(path, data)` | File I/O |

## Cookbook

`examples/python/cookbook/` — 5 canonical examples per
`PLAN_SANDBOXD.md` §8:

1. `hello_world.py` — create + exec `echo hello`
2. `exec_stream.py` — exec with line-by-line stdout streaming
3. `files.py` — upload + read + list
4. `preview.py` — start guest server, mint URL, curl
5. `pool_burst.py` — 50 concurrent leases, measure p95
