#!/usr/bin/env python3
"""Cookbook: shell commands against an alpine sandbox.

Runs a battery of common shell verbs (ls, cat, grep, pipe, redirect,
subshell) through `sb.process.exec` so the `.process` surface gets a
real workout. Pattern mirrors v2's `06_shell_commands.py` but builds
on the warm `base-python` template so each command runs in ~20 ms
after lease.

Usage:
  sudo python3 shell_commands.py [KERNEL_PATH]
"""
from __future__ import annotations

import sys
from _common import resolve_kernel, sandboxd_url

from gocracker import Client, ProcessExitError  # noqa: E402


SHELL_DEMOS = [
    ("uname -a",                         "simple command"),
    ("echo hello | tr a-z A-Z",          "pipe"),
    ("mkdir -p /tmp/demo && touch /tmp/demo/{a,b,c}.txt && ls /tmp/demo/", "mkdir + brace expansion + ls"),
    ("cat /etc/alpine-release",          "read a file"),
    ("printf 'line1\\nline2\\nline3' > /tmp/demo.log && wc -l /tmp/demo.log", "redirect + count"),
    ("grep -c line /tmp/demo.log",       "count matches"),
    ("(cd /etc && ls -1 | head -5)",     "subshell"),
    ("for i in 1 2 3; do echo iter=$i; done", "shell loop"),
    ("date -u +%FT%TZ",                  "command substitution-friendly date"),
    ("test -f /etc/passwd && echo exists", "test / conditional"),
]


def main() -> int:
    kernel = resolve_kernel()
    client = Client(sandboxd_url(), timeout=60)

    # Pool path if base-python exists; otherwise just cold-boot alpine.
    try:
        sb = client.create_sandbox(template="base-python", network_mode="auto")
    except Exception:
        sb = client.create_sandbox(image="alpine:3.20", kernel_path=kernel, network_mode="auto")

    print(f"sandbox id={sb.id}")
    try:
        for cmd, desc in SHELL_DEMOS:
            try:
                r = sb.process.exec(cmd, timeout=10)
                out = r.stdout_text.rstrip()
                print(f"  [{desc}] $ {cmd}")
                for line in out.splitlines():
                    print(f"      {line}")
            except ProcessExitError as e:
                print(f"  [{desc}] $ {cmd}  FAILED exit={e.exit_code} stderr={e.stderr.strip()[:80]}")
    finally:
        sb.delete()
    return 0


if __name__ == "__main__":
    sys.exit(main())
