#!/usr/bin/env python3
"""Cookbook: run N sandboxes in parallel, each doing independent work.

Spawns `count` sandboxes concurrently using a ThreadPool, has each run
a different short command, and aggregates results. Verifies that the
runtime handles concurrent create/exec/delete without collision or
cross-talk (different guest_ip, different id, clean shutdown).

Usage:
  sudo python3 multiple_sandboxes.py [KERNEL_PATH] [COUNT=5]
"""
from __future__ import annotations

import sys
import time
from concurrent.futures import ThreadPoolExecutor
from _common import resolve_kernel, sandboxd_url

from gocracker import Client  # noqa: E402


def worker(client: Client, kernel: str, index: int) -> dict:
    t0 = time.perf_counter()
    sb = client.create_sandbox(image="alpine:3.20", kernel_path=kernel, network_mode="auto")
    try:
        r = sb.process.exec(f"echo 'sandbox #{index} from inside {sb.id}'", timeout=10)
        return {
            "index": index,
            "id": sb.id,
            "ip": sb.guest_ip,
            "stdout": r.stdout_text.strip(),
            "total_ms": round((time.perf_counter() - t0) * 1000),
        }
    finally:
        sb.delete()


def main() -> int:
    kernel = resolve_kernel()
    count = int(sys.argv[2]) if len(sys.argv) > 2 else 5
    client = Client(sandboxd_url(), timeout=300)
    print(f"spawning {count} sandboxes in parallel...")
    with ThreadPoolExecutor(max_workers=count) as ex:
        results = list(ex.map(lambda i: worker(client, kernel, i), range(count)))
    ips = {r["ip"] for r in results}
    ids = {r["id"] for r in results}
    print(f"  unique IPs:  {len(ips)} / {count}")
    print(f"  unique IDs:  {len(ids)} / {count}")
    for r in sorted(results, key=lambda r: r["index"]):
        print(f"  #{r['index']} id={r['id']} ip={r['ip']} ({r['total_ms']}ms): {r['stdout']}")
    ok = len(ips) == count and len(ids) == count and all(f"#{r['index']}" in r["stdout"] for r in results)
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
