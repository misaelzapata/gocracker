// lease → process.exec("echo") → delete micro-bench (JS SDK).
// Kernel path resolves from argv[2] → $GOCRACKER_KERNEL → repo default.
// Sandboxd URL from $GOCRACKER_SANDBOXD → http://127.0.0.1:9091.
import { resolve, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import { existsSync } from 'node:fs';

const _repo = resolve(dirname(fileURLToPath(import.meta.url)), '..', '..', '..', '..');
function resolveKernel() {
  if (process.argv[2]) return process.argv[2];
  if (process.env.GOCRACKER_KERNEL) return process.env.GOCRACKER_KERNEL;
  const def = resolve(_repo, 'artifacts', 'kernels', 'gocracker-guest-standard-vmlinux');
  if (existsSync(def)) return def;
  console.error('error: pass kernel path as arg 1 or set $GOCRACKER_KERNEL');
  process.exit(2);
}
const KERNEL = resolveKernel();
const SANDBOXD = process.env.GOCRACKER_SANDBOXD ?? 'http://127.0.0.1:9091';

const { Client } = await import(resolve(_repo, 'sandboxes', 'sdk', 'js', 'src', 'index.js'));
const c = new Client(SANDBOXD, { timeoutMs: 120000 });
try { await c.unregisterPool('perfbench-js'); } catch {}
await c.registerPool({ templateId: 'perfbench-js', image: 'alpine:3.20', kernelPath: KERNEL, minPaused: 8, maxPaused: 8 });
const deadline = Date.now() + 90000;
while (Date.now() < deadline) {
  const pools = await c.listPools();
  const p = pools.find(x => x.templateId === 'perfbench-js');
  if (p && (p.counts.paused ?? 0) >= 6) break;
  await new Promise(r => setTimeout(r, 300));
}
const lease = [], exec = [], del = [];
for (let i = 0; i < 8; i++) {
  let t = performance.now();
  const sb = await c.leaseSandbox({ templateId: 'perfbench-js' });
  lease.push(performance.now() - t);
  t = performance.now();
  await sb.process.exec(['echo', 'hi']);
  exec.push(performance.now() - t);
  t = performance.now();
  await sb.delete();
  del.push(performance.now() - t);
}
const pr = (name, xs) => { xs.sort((a,b)=>a-b); console.log(`  ${name.padEnd(10)} min=${xs[0].toFixed(2)}  p50=${xs[Math.floor(xs.length/2)].toFixed(2)}  p95=${xs[xs.length-1].toFixed(2)}  max=${xs[xs.length-1].toFixed(2)}`); };
console.log('js-sdk:'); pr('lease', lease); pr('exec_echo', exec); pr('delete', del);
try { await c.unregisterPool('perfbench-js'); } catch {}
