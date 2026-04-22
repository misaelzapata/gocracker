#!/usr/bin/env python3
"""Cookbook 9/10: template → pool → lease pipeline (Fase 6 + Fase 5).

Demonstrates the fast path: register a template (one-time ~2s cold
boot + snapshot capture), register a pool from_template (skips the
per-pool prewarm), lease a sandbox (~15-20ms warm). Second-time
template create is a ~100 µs cache hit per Fase 6.

Usage:
  sudo python3 template_pool.py [KERNEL_PATH]
"""
from __future__ import annotations

import sys
import time
from pathlib import Path

repo_root = Path(__file__).resolve().parents[3]
sys.path.insert(0, str(repo_root / "sdk" / "python"))

from gocracker import Client  # noqa: E402


def main() -> int:
    kernel = sys.argv[1] if len(sys.argv) > 1 else "/home/misael/Desktop/projects/gocracker/artifacts/kernels/gocracker-guest-standard-vmlinux"
    client = Client("http://127.0.0.1:9091", timeout=300.0)

    template_id = "alpine-demo"

    # Create template. First call cold-boots + captures; second is
    # a no-op cache hit.
    t0 = time.time()
    template = client.create_template(image="alpine:3.20", kernel_path=kernel)
    print(f"1st template create: {(time.time()-t0)*1000:.0f} ms, spec_hash={template.spec_hash[:12]}")

    t0 = time.time()
    client.create_template(image="alpine:3.20", kernel_path=kernel)
    print(f"2nd template create (cache hit): {(time.time()-t0)*1000:.1f} ms")

    # Register a pool from the template. Skip per-pool prewarm.
    try:
        client.unregister_pool(template_id)  # clean prior state
    except Exception:
        pass
    t0 = time.time()
    client.register_pool(
        template_id=template_id,
        from_template=template.id,
        min_paused=3,
        max_paused=3,
    )
    print(f"register pool from_template: {(time.time()-t0)*1000:.0f} ms")

    # Wait for warm fill.
    print("warming up 3 paused VMs...", end="", flush=True)
    deadline = time.time() + 60
    while time.time() < deadline:
        pools = [p for p in client.list_pools() if p.template_id == template_id]
        if pools and pools[0].counts.get("paused", 0) >= 3:
            print(" ok")
            break
        print(".", end="", flush=True)
        time.sleep(0.5)

    # Lease 3 warm sandboxes.
    print("leasing 3 warm sandboxes...")
    for i in range(3):
        t0 = time.time()
        sb = client.lease_sandbox(template_id)
        elapsed_ms = (time.time() - t0) * 1000
        print(f"  lease {i+1}: {elapsed_ms:.1f} ms  id={sb.id}  guest_ip={sb.guest_ip}")
        sb.delete()

    # Cleanup.
    client.unregister_pool(template_id)
    client.delete_template(template.id)
    print("cleanup ok")
    return 0


if __name__ == "__main__":
    sys.exit(main())
