#!/usr/bin/env python3
"""Cookbook: pause / resume a running sandbox.

Demonstrates that the runtime survives a snapshot round-trip:
start a long-running counter, pause the VM mid-run, resume it, and
verify the counter kept its state. (In the current sandboxd, pause/
resume is a lease-pool primitive — regular cold-booted sandboxes
don't expose it directly. This example instead uses the delete /
recreate cycle to show "fresh state" vs "persisted state".)

Usage:
  sudo python3 pause_resume.py [KERNEL_PATH]
"""
from __future__ import annotations

import sys
from _common import resolve_kernel, sandboxd_url

from gocracker import Client  # noqa: E402


def main() -> int:
    kernel = resolve_kernel()
    client = Client(sandboxd_url(), timeout=60)
    sb = client.create_sandbox(image="alpine:3.20", kernel_path=kernel, network_mode="auto")
    print(f"sandbox id={sb.id}")
    try:
        # Write a counter file in /tmp, read it back, increment, read.
        sb.process.exec("echo 0 > /tmp/counter")
        for i in range(3):
            sb.process.exec("n=$(cat /tmp/counter); echo $((n+1)) > /tmp/counter")
            r = sb.process.exec("cat /tmp/counter")
            print(f"  iter {i+1}: counter = {r.stdout_text.strip()}")
        # Demonstrate that state is preserved within one sandbox lifecycle.
        r = sb.process.exec("cat /tmp/counter")
        print(f"  final state in-sandbox: {r.stdout_text.strip()}")
    finally:
        sb.delete()
    # A fresh sandbox has NO memory of the previous counter — state is
    # scoped to the sandbox lifecycle. For cross-lease persistence use a
    # post-ready snapshot (templates with Readiness probe) instead.
    sb2 = client.create_sandbox(image="alpine:3.20", kernel_path=kernel, network_mode="auto")
    try:
        r = sb2.process.exec("test -f /tmp/counter && cat /tmp/counter || echo '(no counter)'")
        print(f"  fresh sandbox sees: {r.stdout_text.strip()}")
    finally:
        sb2.delete()
    return 0


if __name__ == "__main__":
    sys.exit(main())
