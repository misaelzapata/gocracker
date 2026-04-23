"""lease → process.exec("echo") → delete micro-bench (Python SDK).

Kernel path resolves from argv[1] → $GOCRACKER_KERNEL → repo default.
Sandboxd URL from $GOCRACKER_SANDBOXD → http://127.0.0.1:9091.
"""
import os, sys, time, statistics
from pathlib import Path

_REPO = Path(__file__).resolve().parents[4]
sys.path.insert(0, str(_REPO / "sandboxes" / "sdk" / "python"))

def resolve_kernel() -> str:
    if len(sys.argv) > 1 and sys.argv[1]:
        return sys.argv[1]
    if os.environ.get("GOCRACKER_KERNEL"):
        return os.environ["GOCRACKER_KERNEL"]
    default = _REPO / "artifacts" / "kernels" / "gocracker-guest-standard-vmlinux"
    if default.exists():
        return str(default)
    print(f"error: pass kernel path as arg 1 or set $GOCRACKER_KERNEL", file=sys.stderr)
    sys.exit(2)

KERNEL = resolve_kernel()
SANDBOXD = os.environ.get("GOCRACKER_SANDBOXD", "http://127.0.0.1:9091")

from gocracker import Client
c = Client(SANDBOXD, timeout=60)
try: c.unregister_pool('perfbench')
except: pass
c.register_pool(template_id='perfbench', image='alpine:3.20', kernel_path=KERNEL, min_paused=8, max_paused=8)
deadline = time.time() + 90
while time.time() < deadline:
    p = [x for x in c.list_pools() if x.template_id == 'perfbench']
    if p and p[0].counts.get('paused', 0) >= 6: break
    time.sleep(0.3)
lease, exec_, delete_ = [], [], []
for _ in range(8):
    t0 = time.perf_counter(); sb = c.lease_sandbox('perfbench'); lease.append((time.perf_counter()-t0)*1000)
    t0 = time.perf_counter(); r = sb.process.exec(['echo','hi'], timeout=5); exec_.append((time.perf_counter()-t0)*1000)
    t0 = time.perf_counter(); c.delete(sb.id); delete_.append((time.perf_counter()-t0)*1000)
def pr(name, xs): xs.sort(); print(f'  {name:10} min={xs[0]:5.2f}  p50={xs[len(xs)//2]:5.2f}  p95={xs[-1]:5.2f}  max={xs[-1]:5.2f}')
print('python-sdk:'); pr('lease', lease); pr('exec_echo', exec_); pr('delete', delete_)
c.unregister_pool('perfbench')
