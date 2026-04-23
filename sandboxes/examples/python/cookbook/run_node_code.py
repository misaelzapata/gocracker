#!/usr/bin/env python3
"""Cookbook: run Node.js code inside a sandbox via base-node.

Usage:
  sudo python3 run_node_code.py [KERNEL_PATH]
"""
from __future__ import annotations

import sys
from _common import resolve_kernel, sandboxd_url

from gocracker import Client  # noqa: E402


SNIPPETS = [
    ("hello",            "node -e 'console.log(\"hi from node\")'"),
    ("async / promise",  "node -e 'Promise.resolve(42).then(v=>console.log(\"resolved=\",v))'"),
    ("JSON round-trip",  "node -e 'const o={a:1,b:[2,3]}; console.log(JSON.stringify(o))'"),
    ("buffer api",       "node -e 'console.log(Buffer.from(\"hello\").toString(\"base64\"))'"),
]


def main() -> int:
    kernel = resolve_kernel()
    client = Client(sandboxd_url(), timeout=60)
    try:
        sb = client.create_sandbox(template="base-node", network_mode="auto")
        print("(using base-node warm template)")
    except Exception:
        sb = client.create_sandbox(image="node:22-alpine", kernel_path=kernel, network_mode="auto")
    print(f"sandbox id={sb.id}")
    try:
        for label, cmd in SNIPPETS:
            r = sb.process.exec(cmd, timeout=15)
            print(f"  [{label}] {r.stdout_text.rstrip()}")
    finally:
        sb.delete()
    return 0


if __name__ == "__main__":
    sys.exit(main())
