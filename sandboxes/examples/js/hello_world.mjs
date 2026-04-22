#!/usr/bin/env node
// Cookbook 1/N (JS): create + exec `echo hello`.
//
// Usage:
//   sudo node hello_world.mjs [KERNEL_PATH]

import { Client } from '../../sdk/js/src/index.js';
import process from 'node:process';

const kernel = process.argv[2]
  || '/home/misael/Desktop/projects/gocracker/artifacts/kernels/gocracker-guest-standard-vmlinux';

const client = new Client('http://127.0.0.1:9091');

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
