"""Time-to-Interactive bench.

TTI = wall-clock from `create()` returning the sandbox handle to the
first stdout byte of `runCommand("node", "-v")`. Ten timed runs after
one warmup, against the auto-registered `base-node` template (warm
pool of 8).
"""
import sys, time, statistics
sys.path.insert(0, '/home/misael/Desktop/projects/gocracker/sandboxes/sdk/python')
from gocracker import Client

c = Client('http://127.0.0.1:9091', timeout=60)

# Register a warm pool from base-node so lease_sandbox lands a warm
# restore.
try: c.unregister_pool('tti-bench')
except Exception: pass
c.register_pool(template_id='tti-bench', from_template='base-node', min_paused=8, max_paused=8)
deadline = time.time() + 120
while time.time() < deadline:
    p = [x for x in c.list_pools() if x.template_id == 'tti-bench']
    if p and p[0].counts.get('paused', 0) >= 6: break
    time.sleep(0.3)
print(f"pool ready: {p[0].counts}")

# 1 warmup + 10 timed
def one():
    sb = c.lease_sandbox('tti-bench')           # create() returns
    t0 = time.perf_counter()                    # ← TTI starts HERE
    r = sb.process.exec(['node', '-v'])         # first runCommand
    elapsed_ms = (time.perf_counter() - t0) * 1000
    c.delete(sb.id)
    return elapsed_ms, r.stdout_text.strip()

# Warmup
_w, out = one()
print(f"warmup: TTI={_w:.0f}ms output={out!r}")

# Timed runs
ttis = []
for i in range(10):
    ms, out = one()
    print(f"TTI {i+1}: {ms:.0f}ms output={out!r}")
    ttis.append(ms)

ttis.sort()
print()
print(f"median = {statistics.median(ttis):.0f} ms, mean = {statistics.mean(ttis):.0f} ms, "
      f"p95 = {ttis[-1]:.0f} ms, min = {ttis[0]:.0f} ms, max = {ttis[-1]:.0f} ms")
c.unregister_pool('tti-bench')
