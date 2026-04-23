#!/usr/bin/env python3
"""Cookbook 2/5: exec with line-by-line stdout streaming.

Runs a shell loop inside the guest that prints one line per second,
streams the output to stdout as frames arrive (not at-end-of-command
like exec()).

Usage:
  python exec_stream.py [KERNEL_PATH]
"""
from __future__ import annotations

import sys
from _common import resolve_kernel, sandboxd_url

from gocracker import Client  # noqa: E402
from gocracker.toolbox import CHANNEL_EXIT, CHANNEL_STDERR, CHANNEL_STDOUT  # noqa: E402


def main() -> int:
    kernel = resolve_kernel()
    client = Client(sandboxd_url())

    sb = client.create_sandbox(image="alpine:3.20", kernel_path=kernel)
    print(f"sandbox id={sb.id}")

    # Small sh loop: 5 lines, one per second, with a final "done" line.
    # Using /bin/sh so the loop runs inside the guest shell.
    script = (
        "for i in 1 2 3 4 5; do "
        "  echo line $i; "
        "  sleep 1; "
        "done; "
        "echo done"
    )
    try:
        exit_code = -1
        for channel, payload in sb.toolbox().exec_stream(["/bin/sh", "-c", script], timeout=30.0):
            text = payload.decode("utf-8", errors="replace").rstrip("\n")
            if channel == CHANNEL_STDOUT:
                print(f"stdout> {text}", flush=True)
            elif channel == CHANNEL_STDERR:
                print(f"stderr> {text}", flush=True)
            elif channel == CHANNEL_EXIT:
                exit_code = int.from_bytes(payload[:4], byteorder="big", signed=True)
        print(f"exit={exit_code}")
    finally:
        sb.delete()

    return 0


if __name__ == "__main__":
    sys.exit(main())
