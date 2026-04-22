# gocracker JS/Node SDK

Zero-dependency (Node stdlib only) client for the gocracker sandboxd
HTTP control plane + the in-guest toolbox agent over UDS.

Requires Node 18+ (uses global `fetch`).

## Install

```bash
cd sandboxes/sdk/js
npm link     # or: npm install -e .
```

or copy `src/index.js` into your project.

## Quick start

```js
import { Client } from '@gocracker/sdk';

const client = new Client('http://127.0.0.1:9091');
const sb = await client.createSandbox({
  image: 'alpine:3.20',
  kernelPath: '/abs/path/to/vmlinux',
});

const tb = sb.toolbox();
const { stdoutText, exitCode } = await tb.exec(['echo', 'hello']);
console.log({ stdoutText, exitCode });  // { stdoutText: 'hello\n', exitCode: 0 }

await sb.delete();
```

## Surface

Mirror of the Python SDK — same endpoint shape, camelCase field names.

**Control plane** (`client.`):
- `createSandbox` / `listSandboxes` / `getSandbox` / `delete`
- `registerPool` / `listPools` / `unregisterPool` / `leaseSandbox`
- `createTemplate` / `listTemplates` / `getTemplate` / `deleteTemplate`
- `mintPreview`
- `healthz`

**Toolbox** (`sb.toolbox()`):
- `health()` · `exec(cmd, opts)` · `execStream(cmd, opts)` (async iterator)
- `listFiles(path)` · `download(path)` · `upload(path, data)` · `deleteFile(path)`
- `mkdir(path, parents?)` · `rename(src, dst)` · `chmod(path, mode)`
- `gitClone(repository, directory, ref?)` · `gitStatus(directory)`
- `setSecret(name, value)` · `listSecrets()` · `deleteSecret(name)`

Typed errors: `SandboxNotFound`, `SandboxInvalidRequest`, `SandboxConflict`, `ToolboxError`.
