import { Client } from '/home/misael/Desktop/projects/gocracker/sandboxes/sdk/js/src/index.js';
const c = new Client('http://127.0.0.1:9091', { timeoutMs: 120000 });
try { await c.unregisterPool('perfbench-js'); } catch {}
await c.registerPool({ templateId: 'perfbench-js', image: 'alpine:3.20', kernelPath: '/home/misael/Desktop/projects/gocracker/artifacts/kernels/gocracker-guest-standard-vmlinux', minPaused: 8, maxPaused: 8 });
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
