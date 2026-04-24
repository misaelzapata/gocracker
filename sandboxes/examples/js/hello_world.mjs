#!/usr/bin/env node
// Cookbook 1/N (JS): create + exec `echo hello`.
//
// Kernel resolves from argv[2] -> $GOCRACKER_KERNEL -> repo default.
// Sandboxd URL from $GOCRACKER_SANDBOXD -> http://127.0.0.1:9091.
//
// Usage:
//   sudo node hello_world.mjs [KERNEL_PATH]

import { Client } from '../../sdk/js/src/index.js';
import process from 'node:process';
import { resolve, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import { existsSync } from 'node:fs';

function resolveKernel() {
  if (process.argv[2]) return process.argv[2];
  if (process.env.GOCRACKER_KERNEL) return process.env.GOCRACKER_KERNEL;
  const repo = resolve(dirname(fileURLToPath(import.meta.url)), '..', '..', '..');
  const def = resolve(repo, 'artifacts', 'kernels', 'gocracker-guest-standard-vmlinux');
  if (existsSync(def)) return def;
  console.error('error: pass kernel path as arg 1 or set $GOCRACKER_KERNEL');
  process.exit(2);
}

const kernel = resolveKernel();
const client = new Client(process.env.GOCRACKER_SANDBOXD ?? 'http://127.0.0.1:9091');

if (!(await client.healthz())) {
  console.error('sandboxd not reachable at 127.0.0.1:9091');
  process.exit(1);
}

console.log(`creating sandbox (alpine:3.20, kernel=${kernel})...`);
const sb = await client.createSandbox({ image: 'alpine:3.20', kernelPath: kernel });
console.log(`  id=${sb.id} guest_ip=${sb.guestIp} uds=${sb.udsPath}`);

try {
  const result = await sb.toolbox().exec(['echo', 'hello from gocracker (JS)']);
  console.log(`exit=${result.exitCode}`);
  console.log(`stdout: ${result.stdoutText.trim()}`);
  if (result.stderr.length) console.log(`stderr: ${result.stderrText.trim()}`);
} finally {
  await sb.delete();
  console.log(`deleted id=${sb.id}`);
}
