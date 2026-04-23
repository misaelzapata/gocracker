# gocracker JS/Node SDK

Zero-dependency (Node stdlib only) client for the gocracker sandboxd
HTTP control plane + the in-guest toolbox agent over UDS.

Requires Node 18+ (uses global `fetch`).

Warm-lease latency (measured 2026-04-22 on x86, pool of 8):

- `createSandbox({template: 'base-python'})` p95 = **1.5 ms**
- full `create → process.exec → delete` p95 = **~35 ms**

## Install

```bash
cd sandboxes/sdk/js
npm link   # or copy src/index.js into your project — no deps
```

## Quick start

Start `gocracker-sandboxd` with `-kernel-path` (or `GOCRACKER_KERNEL`)
so `base-python / base-node / base-bun / base-go` auto-register.

```js
import { Client } from '@gocracker/sdk';

const client = new Client('http://127.0.0.1:9091');
const sb = await client.createSandbox({ template: 'base-python' });
try {
  const r = await sb.process.exec(`python3 -c "print(2+2)"`);
  console.log(r.stdoutText.trim());           // "4"

  await sb.fs.writeFile('/tmp/hi.txt', Buffer.from('hello'));
  const data = await sb.fs.readFile('/tmp/hi.txt');
  console.log(data.toString());                // "hello"

  const entries = await sb.fs.listDir('/tmp');
  console.log(entries.map(e => e.name));       // ["hi.txt"]

  const url = await sb.previewUrl(8080);       // signed preview URL
  console.log(url);
} finally {
  await sb.delete();
}
```

Node 24+ supports `await using` for auto-cleanup (TC39 explicit-
resource-management — `Sandbox` implements `Symbol.asyncDispose`):

```js
await using sb = await client.createSandbox({ template: 'base-python' });
await sb.process.exec('echo hi');
// sb auto-disposed here
```

Low-level / escape hatch — `sb.toolbox()` returns the flat
`ToolboxClient`:

```js
const sb = await client.createSandbox({ image: 'alpine:3.20', kernelPath: '/abs/vmlinux' });
await sb.toolbox().exec(['echo', 'hi']);
```

## Surface

### Client

| Method | What |
|---|---|
| `createSandbox({template, image, kernelPath, ...})` | Template-restore or cold-boot. |
| `leaseSandbox(req)` | Warm lease (<5 ms). |
| `listSandboxes / getSandbox / delete` | Inventory + teardown. |
| `registerPool / listPools / unregisterPool` | Warm pool. |
| `createTemplate / listTemplates / getTemplate / deleteTemplate` | Template lifecycle. |
| `mintPreview(id, port)` | Raw signed URL (prefer `sb.previewUrl`). |

### `Sandbox`

| Property / Method | What |
|---|---|
| `sb.process.exec(cmd, opts?)` | `cmd` may be string or array; non-zero exit → `ProcessExitError`. |
| `sb.process.execStream(cmd) / .start(cmd)` | Async iterator of frames. |
| `sb.fs.writeFile / readFile / listDir / remove / mkdir / chmod / rename` | Canonical file ops. |
| `sb.previewUrl(port)` | Absolute signed URL. |
| `sb.delete()` | Async teardown. |
| `await using sb = ...` | Auto-dispose (Node 24+). |
| `sb.toolbox()` | Flat low-level client. |

### Typed errors

```
SandboxError
├── SandboxNotFound        # 404 on sandbox / pool / template
├── SandboxInvalidRequest  # 400
├── SandboxConflict        # 409
├── ProcessExitError       # exec non-zero; .exitCode + .stdout + .stderr
├── TemplateNotFound       # createSandbox({template: X}) unknown
├── PoolExhausted          # leaseSandbox against an empty pool
├── RuntimeUnreachable     # sandboxd can't reach gocracker runtime
└── SandboxTimeout         # operation deadline exceeded
```

## SDK parity

- Python: `sandboxes/sdk/python/`
- Go: `sandboxes/sdk/go/`

Same surface (`template=`, `.process`, `.fs`, `previewUrl`, typed
errors). JS-specific: `Symbol.asyncDispose` for `await using`,
camelCase field names.
