import sys, time, statistics
sys.path.insert(0, '/home/misael/Desktop/projects/gocracker/sandboxes/sdk/python')
from gocracker import Client
c = Client('http://127.0.0.1:9091', timeout=60)
try: c.unregister_pool('perfbench')
except: pass
c.register_pool(template_id='perfbench', image='alpine:3.20', kernel_path='/home/misael/Desktop/projects/gocracker/artifacts/kernels/gocracker-guest-standard-vmlinux', min_paused=8, max_paused=8)
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
