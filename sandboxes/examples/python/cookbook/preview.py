#!/usr/bin/env python3
"""Cookbook 4/5: launch a guest HTTP server, mint a preview URL, curl it.

Runs a tiny Python web server inside the guest on port 8080, mints
a signed preview URL via POST /sandboxes/{id}/preview/8080, then
fetches the URL from the host and prints the body.

Usage:
  python preview.py [KERNEL_PATH]
"""
from __future__ import annotations

import sys
import time
import urllib.request
from _common import resolve_kernel, sandboxd_url

from gocracker import Client  # noqa: E402


GUEST_SERVER_SCRIPT = r"""
python3 -c '
from http.server import BaseHTTPRequestHandler, HTTPServer
class H(BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200)
        self.send_header("Content-Type", "text/plain")
        self.end_headers()
        self.wfile.write(b"hello from the guest\n")
    def log_message(self, *a, **kw): pass
HTTPServer(("0.0.0.0", 8080), H).serve_forever()
'
""".strip()


def main() -> int:
    kernel = resolve_kernel()
    client = Client(sandboxd_url())

    sb = client.create_sandbox(image="python:3.12-alpine", kernel_path=kernel)
    print(f"sandbox id={sb.id}")

    try:
        tb = sb.toolbox()

        # Start the server in the background. exec() blocks by default,
        # so we fire and forget via /bin/sh -c '... &' + tiny sleep to
        # give the server time to bind.
        print("starting guest HTTP server on :8080...")
        tb.exec(
            ["/bin/sh", "-c", f"({GUEST_SERVER_SCRIPT}) >/dev/null 2>&1 </dev/null &"],
            timeout=5.0,
        )
        time.sleep(2)

        # Mint the preview URL.
        preview = client.mint_preview(sb.id, 8080)
        print(f"preview URL: {preview.url}")
        print(f"subdomain:   {preview.subdomain}")
        print(f"expires:     {preview.expires_at}")

        # Fetch via the path-based URL.
        full_url = sandboxd_url() + preview.url
        with urllib.request.urlopen(full_url, timeout=5.0) as resp:
            body = resp.read()
        print(f"GET {full_url} → {resp.status} {body!r}")
    finally:
        sb.delete()

    return 0


if __name__ == "__main__":
    sys.exit(main())
