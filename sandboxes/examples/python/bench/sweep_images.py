"""Sweep the 6 images we exercised during this session.
For each: cold-boot a sandbox, run a representative command, verify, delete.
Reports duration of the full create+exec+delete cycle."""
import sys, time
sys.path.insert(0, '/home/misael/Desktop/projects/gocracker/sandboxes/sdk/python')
from gocracker import Client, ProcessExitError

c = Client('http://127.0.0.1:9091', timeout=300)
KERNEL = '/home/misael/Desktop/projects/gocracker/artifacts/kernels/gocracker-guest-standard-vmlinux'

# (label, image, cmd, expected substring)
IMAGES = [
    ('alpine 3.20',          'alpine:3.20',         ['cat', '/etc/alpine-release'], '3.20'),
    ('python 3.12-alpine',   'python:3.12-alpine',  ['python3', '--version'],       'Python 3.12'),
    ('node 22-alpine',       'node:22-alpine',      ['node', '-v'],                 'v22'),
    ('bun 1',                'oven/bun:1',          ['/usr/local/bin/bun', '--version'],           '1.'),
    ('golang 1.23-alpine',   'golang:1.23-alpine',  ['/usr/local/go/bin/go', 'version'],              'go1.23'),
    ('alpine + git',         'alpine:3.20',         ['sh', '-c', 'apk add --no-cache git >/dev/null && git --version'], 'git version 2.'),
]

print(f'{"image":24} {"cold_create":>12} {"exec_ms":>8} {"delete_ms":>10} status')
print('-' * 72)
for label, image, cmd, expected in IMAGES:
    try:
        t0 = time.perf_counter()
        sb = c.create_sandbox(image=image, kernel_path=KERNEL, network_mode='auto')
        t_create = (time.perf_counter() - t0) * 1000
        t1 = time.perf_counter()
        try:
            r = sb.process.exec(cmd, timeout=120)
            t_exec = (time.perf_counter() - t1) * 1000
            ok = expected in (r.stdout_text + r.stderr_text)
            status = f'OK ({r.stdout_text.strip()[:30]!r})' if ok else f'FAIL stdout={r.stdout_text.strip()[:50]!r}'
        except ProcessExitError as e:
            t_exec = (time.perf_counter() - t1) * 1000
            status = f'EXIT {e.exit_code} stderr={e.stderr.strip()[:50]!r}'
        t2 = time.perf_counter()
        c.delete(sb.id)
        t_delete = (time.perf_counter() - t2) * 1000
        print(f'{label:24} {t_create:>10.0f}ms {t_exec:>6.0f}ms {t_delete:>8.0f}ms {status}')
    except Exception as e:
        print(f'{label:24} FAILED: {type(e).__name__}: {e}')
