"""Toolbox agent client over UDS + Firecracker-style CONNECT.

Dials the sandbox's host-side UDS, sends "CONNECT <port>\\n",
validates the "OK\\n" response, then speaks the toolbox HTTP
protocol on the bridged stream. Three operation groups:

  health():                          cheap liveness
  exec(cmd, ...):                    /exec framed binary protocol
  upload(path, data), download(path): /files JSON
"""
from __future__ import annotations

import io
import json
import socket
import struct
from dataclasses import dataclass, field
from typing import Dict, Iterator, List, Optional, Tuple


TOOLBOX_VSOCK_PORT = 10023

# Frame protocol (matches internal/toolbox/agent/frame.go):
#   [1 byte channel][4 byte length BE][payload]
CHANNEL_STDIN = 0
CHANNEL_STDOUT = 1
CHANNEL_STDERR = 2
CHANNEL_EXIT = 3
CHANNEL_SIGNAL = 4


class ToolboxError(Exception):
    """Base class for toolbox-agent client errors."""


class ToolboxTimeout(ToolboxError):
    """Raised when a UDS dial / read exceeds the deadline."""


@dataclass
class ExecResult:
    exit_code: int
    stdout: bytes
    stderr: bytes

    @property
    def stdout_text(self) -> str:
        return self.stdout.decode("utf-8", errors="replace")

    @property
    def stderr_text(self) -> str:
        return self.stderr.decode("utf-8", errors="replace")


@dataclass
class FileEntry:
    name: str
    size: int = 0
    is_dir: bool = False
    mode: int = 0


class ToolboxClient:
    """Client for the in-guest toolbox agent.

    Bound to a single UDS path (the sandbox's host-visible socket).
    Every call dials a fresh UDS → no long-lived connection, no
    keep-alive — matches the agent's Connection: close response
    shape and keeps the client simple.
    """

    def __init__(
        self,
        uds_path: str,
        port: int = TOOLBOX_VSOCK_PORT,
        dial_timeout: float = 5.0,
    ):
        self.uds_path = uds_path
        self.port = port
        self.dial_timeout = dial_timeout

    # ---- Health ----

    def health(self) -> Dict[str, object]:
        """GET /healthz on the agent. Returns the JSON body.
        Raises ToolboxError on non-200."""
        status, _, body = self._request("GET", "/healthz")
        if status != 200:
            raise ToolboxError(f"health: status={status} body={body[:200]!r}")
        return json.loads(body)

    # ---- Exec ----

    def exec(
        self,
        cmd: List[str],
        env: Optional[List[str]] = None,
        workdir: str = "",
        stdin: Optional[bytes] = None,
        timeout: float = 30.0,
    ) -> ExecResult:
        """Run a command in the guest and collect stdout/stderr/exit.
        Blocking — for streaming variant see `exec_stream`."""
        stdout = io.BytesIO()
        stderr = io.BytesIO()
        exit_code = -1
        for channel, payload in self.exec_stream(cmd, env=env, workdir=workdir, stdin=stdin, timeout=timeout):
            if channel == CHANNEL_STDOUT:
                stdout.write(payload)
            elif channel == CHANNEL_STDERR:
                stderr.write(payload)
            elif channel == CHANNEL_EXIT:
                exit_code = _parse_exit(payload)
        return ExecResult(exit_code=exit_code, stdout=stdout.getvalue(), stderr=stderr.getvalue())

    def exec_stream(
        self,
        cmd: List[str],
        env: Optional[List[str]] = None,
        workdir: str = "",
        stdin: Optional[bytes] = None,
        timeout: float = 30.0,
    ) -> Iterator[Tuple[int, bytes]]:
        """Yield (channel, payload) frames as they arrive. Channel
        constants: CHANNEL_STDOUT, CHANNEL_STDERR, CHANNEL_EXIT.

        Called by exec() to aggregate; call directly for line-by-line
        streaming (see examples/python/cookbook/exec_stream.py).
        """
        body: Dict[str, object] = {"cmd": cmd}
        if env:
            body["env"] = env
        if workdir:
            body["workdir"] = workdir

        sock = self._dial_connect()
        sock.settimeout(timeout)
        try:
            # Send HTTP request + body.
            body_bytes = json.dumps(body).encode("utf-8")
            http_req = (
                f"POST /exec HTTP/1.0\r\n"
                f"Host: x\r\n"
                f"Content-Length: {len(body_bytes)}\r\n"
                f"Content-Type: application/json\r\n"
                f"Connection: close\r\n"
                f"\r\n"
            ).encode("ascii")
            sock.sendall(http_req)
            sock.sendall(body_bytes)
            if stdin:
                # Frame the stdin as channel 0 so the agent sees
                # it, then signal EOF via a zero-length stdin frame.
                _write_frame(sock, CHANNEL_STDIN, stdin)
                _write_frame(sock, CHANNEL_STDIN, b"")

            # Read HTTP response line + headers (drain until blank line).
            f = sock.makefile("rb")
            status_line = f.readline()
            if not status_line.startswith(b"HTTP/"):
                raise ToolboxError(f"exec: unexpected response: {status_line!r}")
            while True:
                line = f.readline()
                if line in (b"\r\n", b"\n", b""):
                    break

            # Stream framed responses.
            while True:
                hdr = f.read(5)
                if len(hdr) < 5:
                    return
                channel = hdr[0]
                n = struct.unpack(">I", hdr[1:5])[0]
                if n > 0:
                    payload = f.read(n)
                    if len(payload) < n:
                        return
                else:
                    payload = b""
                yield channel, payload
                if channel == CHANNEL_EXIT:
                    return
        finally:
            try:
                sock.close()
            except Exception:
                pass

    # ---- Files (basic list/upload/download) ----

    def list_files(self, path: str) -> List[FileEntry]:
        status, _, body = self._request("GET", f"/files?path={path}")
        if status != 200:
            raise ToolboxError(f"list_files: status={status} body={body[:200]!r}")
        parsed = json.loads(body)
        return [
            FileEntry(
                name=e.get("name", ""),
                size=e.get("size", 0),
                # Agent returns kind="file"|"dir"; map to is_dir bool.
                is_dir=e.get("kind") == "dir",
                mode=e.get("mode", 0),
            )
            for e in parsed.get("entries", [])
        ]

    def download(self, path: str) -> bytes:
        status, _, body = self._request("GET", f"/files/download?path={path}", raw_body=True)
        if status != 200:
            raise ToolboxError(f"download: status={status} body={body[:200]!r}")
        return body if isinstance(body, bytes) else body.encode("utf-8")

    def upload(self, path: str, data: bytes) -> None:
        status, _, body = self._request(
            "POST",
            f"/files/upload?path={path}",
            body=data,
            content_type="application/octet-stream",
        )
        if status not in (200, 201):
            raise ToolboxError(f"upload: status={status} body={body[:200]!r}")

    def delete_file(self, path: str) -> None:
        status, _, body = self._request("DELETE", f"/files?path={path}")
        if status not in (200, 204):
            raise ToolboxError(f"delete_file: status={status} body={body[:200]!r}")

    def mkdir(self, path: str, parents: bool = False) -> None:
        # Agent expects {"path": ..., "all": bool} — not "parents".
        body = {"path": path, "all": parents}
        status, _, resp = self._request("POST", "/files/mkdir", body=json.dumps(body).encode())
        if status != 200:
            raise ToolboxError(f"mkdir: status={status} body={resp[:200]!r}")

    def rename(self, src: str, dst: str) -> None:
        # Agent expects {"old_path": ..., "new_path": ...}.
        body = {"old_path": src, "new_path": dst}
        status, _, resp = self._request("POST", "/files/rename", body=json.dumps(body).encode())
        if status != 200:
            raise ToolboxError(f"rename: status={status} body={resp[:200]!r}")

    def chmod(self, path: str, mode: int) -> None:
        body = {"path": path, "mode": mode}
        status, _, resp = self._request("POST", "/files/chmod", body=json.dumps(body).encode())
        if status != 200:
            raise ToolboxError(f"chmod: status={status} body={resp[:200]!r}")

    # ---- Git ----

    def git_clone(self, repository: str, directory: str, ref: str = "") -> Dict[str, object]:
        body: Dict[str, object] = {"repository": repository, "directory": directory}
        if ref:
            body["ref"] = ref
        status, _, resp = self._request("POST", "/git/clone", body=json.dumps(body).encode())
        if status != 200:
            raise ToolboxError(f"git_clone: status={status} body={resp[:200]!r}")
        return json.loads(resp)

    def git_status(self, directory: str) -> Dict[str, object]:
        body = {"directory": directory}
        status, _, resp = self._request("POST", "/git/status", body=json.dumps(body).encode())
        if status != 200:
            raise ToolboxError(f"git_status: status={status} body={resp[:200]!r}")
        return json.loads(resp)

    # ---- Secrets ----

    def set_secret(self, name: str, value: str) -> None:
        body = {"name": name, "value": value}
        status, _, resp = self._request("POST", "/secrets", body=json.dumps(body).encode())
        if status not in (200, 201):
            raise ToolboxError(f"set_secret: status={status} body={resp[:200]!r}")

    def list_secrets(self) -> List[str]:
        status, _, resp = self._request("GET", "/secrets")
        if status != 200:
            raise ToolboxError(f"list_secrets: status={status} body={resp[:200]!r}")
        parsed = json.loads(resp)
        # Agent returns {"secrets": [name1, name2, ...]}.
        return parsed.get("secrets", [])

    def delete_secret(self, name: str) -> None:
        status, _, resp = self._request("DELETE", f"/secrets/{name}")
        if status not in (200, 204):
            raise ToolboxError(f"delete_secret: status={status} body={resp[:200]!r}")

    # ---- Internals ----

    def _dial_connect(self) -> socket.socket:
        s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        s.settimeout(self.dial_timeout)
        try:
            s.connect(self.uds_path)
        except socket.timeout:
            raise ToolboxTimeout(f"dial timeout: {self.uds_path}")
        except OSError as e:
            s.close()
            raise ToolboxError(f"dial {self.uds_path}: {e}")
        # CONNECT handshake.
        s.sendall(f"CONNECT {self.port}\n".encode("ascii"))
        f = s.makefile("rb")
        line = f.readline()
        if not line.startswith(b"OK"):
            s.close()
            raise ToolboxError(f"CONNECT rejected: {line.strip()!r}")
        # dial_timeout only bounds the handshake; long-running operations
        # (git clone, large uploads) set their own deadline via the
        # per-call timeout or exec_stream. Leaving a 5s read timeout here
        # truncates any request that waits longer than 5s for its
        # response body.
        s.settimeout(None)
        return s

    def _request(
        self,
        method: str,
        path: str,
        body: Optional[bytes] = None,
        content_type: str = "application/json",
        raw_body: bool = False,
    ) -> Tuple[int, Dict[str, str], object]:
        sock = self._dial_connect()
        try:
            hdrs = f"{method} {path} HTTP/1.0\r\nHost: x\r\nConnection: close\r\n"
            if body:
                hdrs += f"Content-Length: {len(body)}\r\nContent-Type: {content_type}\r\n"
            hdrs += "\r\n"
            sock.sendall(hdrs.encode("ascii"))
            if body:
                sock.sendall(body)
            f = sock.makefile("rb")
            status_line = f.readline()
            if not status_line.startswith(b"HTTP/"):
                raise ToolboxError(f"unexpected response: {status_line!r}")
            parts = status_line.split(b" ", 2)
            status = int(parts[1])
            headers: Dict[str, str] = {}
            while True:
                line = f.readline()
                if line in (b"\r\n", b"\n", b""):
                    break
                if b":" in line:
                    k, v = line.split(b":", 1)
                    headers[k.strip().decode("ascii").lower()] = v.strip().decode("latin-1")
            # Bound the read by Content-Length if present. Without
            # this, f.read() waits for EOF — which never arrives
            # promptly over the UDS bridge because the agent's
            # Connection: close + flush races with the host-side
            # io.Copy. A stale socket timeout masqueraded as a
            # "hung mkdir" bug.
            body_bytes: bytes
            # Responses that definitionally have no body (RFC 7230).
            if status in (204, 205, 304):
                body_bytes = b""
            else:
                cl = headers.get("content-length", "")
                if cl:
                    try:
                        n = int(cl)
                        body_bytes = f.read(n) if n > 0 else b""
                    except ValueError:
                        body_bytes = f.read()
                else:
                    body_bytes = f.read()
            if raw_body:
                return status, headers, body_bytes
            return status, headers, body_bytes.decode("utf-8", errors="replace")
        finally:
            try:
                sock.close()
            except Exception:
                pass


# ---- Helpers ----


def _write_frame(sock: socket.socket, channel: int, payload: bytes) -> None:
    sock.sendall(bytes([channel]) + struct.pack(">I", len(payload)) + payload)


def _parse_exit(payload: bytes) -> int:
    """Exit frames carry a signed 32-bit integer (BE). The agent uses
    -1 for "killed by signal"; 128+|signum| is NOT encoded for us
    because the CLI doesn't need the distinction."""
    if len(payload) < 4:
        return -1
    return struct.unpack(">i", payload[:4])[0]
