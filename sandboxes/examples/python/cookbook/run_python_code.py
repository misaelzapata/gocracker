#!/usr/bin/env python3
"""Cookbook: run arbitrary Python code inside a sandbox.

Uses the auto-registered `base-python` template (python:3.12-alpine
with the toolbox agent baked in). After `lease`, `sb.process.exec`
forwards stdin into the guest's python3 -c so callers can ship a
script in a single request.

Usage:
  sudo python3 run_python_code.py [KERNEL_PATH]
"""
from __future__ import annotations

import sys
import textwrap
from _common import resolve_kernel, sandboxd_url

from gocracker import Client  # noqa: E402


SNIPPETS = [
    ("2+2",                      "python3 -c 'print(2+2)'"),
    ("list comprehension",       "python3 -c 'print([i*i for i in range(5)])'"),
    (
        "small computation",
        textwrap.dedent(r"""
        python3 - <<'PY'
        import math
        print("pi     =", math.pi)
        print("e      =", math.e)
        print("phi    =", (1 + math.sqrt(5)) / 2)
        print("ln(2)  =", math.log(2))
        PY
        """).strip(),
    ),
    (
        "json roundtrip",
        textwrap.dedent(r"""
        python3 - <<'PY'
        import json
        data = {"sandbox": "gocracker", "version": 3, "ok": True}
        print(json.dumps(data, indent=2))
        PY
        """).strip(),
    ),
]


def main() -> int:
    kernel = resolve_kernel()
    client = Client(sandboxd_url(), timeout=60)

    # Fall back to a cold `python:3.12-alpine` if base-python isn't
    # registered (sandboxd needs `-kernel-path` for auto-register).
    try:
        sb = client.create_sandbox(template="base-python", network_mode="auto")
        print("(using base-python warm template)")
    except Exception:
        sb = client.create_sandbox(image="python:3.12-alpine", kernel_path=kernel, network_mode="auto")
        print("(cold-booting python:3.12-alpine)")

    print(f"sandbox id={sb.id} guest_ip={sb.guest_ip}")
    try:
        for label, cmd in SNIPPETS:
            r = sb.process.exec(cmd, timeout=20)
            print(f"  [{label}]")
            for line in r.stdout_text.rstrip().splitlines():
                print(f"      {line}")
    finally:
        sb.delete()
    return 0


if __name__ == "__main__":
    sys.exit(main())
