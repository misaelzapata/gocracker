"""Control-plane client for sandboxd.

Typed wrappers around the HTTP routes exposed by
sandboxes/internal/sandboxd. Errors are typed (SandboxNotFound,
PoolNotFound, TemplateNotFound, etc.) so callers can match on
`except SandboxNotFound` instead of parsing status codes.

The client uses the stdlib-only `urllib.request` — no `requests`
dependency so the SDK can be dropped into a user's env without
extra install steps. HTTP keep-alive + pooling are handled by a
module-level opener; callers that need more concurrency can
construct multiple Client instances.
"""
from __future__ import annotations

import json
import time
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass, field
from typing import Any, Dict, List, Optional


class SandboxError(Exception):
    """Base class for sandboxd client errors. Subclasses keep the
    typed-error shape callers rely on (SandboxNotFound vs generic
    SandboxError) without losing the underlying HTTP status."""

    def __init__(self, message: str, status: int = 0, body: str = ""):
        super().__init__(message)
        self.status = status
        self.body = body


class SandboxNotFound(SandboxError):
    """Raised for 404 on sandbox / pool / template endpoints."""


class SandboxInvalidRequest(SandboxError):
    """Raised for 400 — malformed request."""


class SandboxConflict(SandboxError):
    """Raised for 409 — pool template_id already registered, etc."""


@dataclass
class Sandbox:
    """Server-side sandbox record. Mirrors sandboxd.Sandbox JSON shape.
    Fields are ordered to match the JSON tags exactly so the dataclass
    round-trips via `Sandbox(**resp)`."""

    id: str
    state: str
    image: str
    uds_path: str = ""
    guest_ip: str = ""
    runtime_id: str = ""
    created_at: str = ""
    error: str = ""

    # Non-persisted: the client instance this sandbox belongs to, so
    # `sb.toolbox()` and `sb.delete()` work without the caller
    # threading the client through.
    _client: Optional["Client"] = field(default=None, repr=False, compare=False)

    def delete(self) -> None:
        if self._client is None:
            raise RuntimeError("Sandbox has no client; call client.delete(id) instead")
        self._client.delete(self.id)

    def recycle(self) -> "Sandbox":
        """Recycle this leased sandbox: teardown the current VM and
        return a fresh one from the same pool. The old handle (self)
        is dead after this call — use the returned Sandbox.

        Only works for lease-origin sandboxes (created via a pool).
        Cold-booted sandboxes raise SandboxConflict. Raises
        PoolExhausted if the pool has no paused entries within 5 s.
        """
        if self._client is None:
            raise RuntimeError("Sandbox has no client; call client.recycle(id) instead")
        return self._client.recycle(self.id)

    def toolbox(self) -> "ToolboxClient":
        """Return a toolbox-agent client bound to this sandbox's UDS.
        Import is lazy so the toolbox module doesn't load for callers
        who only use the control plane."""
        if not self.uds_path:
            raise SandboxError("sandbox has no uds_path — not ready?")
        from .toolbox import ToolboxClient

        return ToolboxClient(self.uds_path)

    # ---- Convenience namespaces (v2 parity on top of v3 runtime) ----
    #
    # sb.process.exec("python -c 'print(2)'")
    # sb.fs.read_file("/tmp/x")
    # sb.preview_url(8080)
    # with client.create_sandbox(template="base-python") as sb: ...
    #
    # The underlying transport (UDS + CONNECT + toolbox agent) is
    # unchanged; these are thin wrappers that keep the shape v2 callers
    # expect.

    @property
    def process(self) -> "_ProcessNamespace":
        return _ProcessNamespace(self.toolbox())

    @property
    def fs(self) -> "_FSNamespace":
        return _FSNamespace(self.toolbox())

    def preview_url(self, port: int) -> str:
        """Mint a signed preview URL for a guest-side port. Returns the
        absolute path-form URL (`/previews/<token>/`) usable directly
        against the sandboxd HTTP endpoint.

        Callers who need the subdomain form, expiry, or raw token can
        use `client.mint_preview(sb.id, port)` which returns the full
        Preview record.
        """
        if self._client is None:
            raise RuntimeError("Sandbox has no client; call client.mint_preview(id, port)")
        preview = self._client.mint_preview(self.id, port)
        return f"{self._client.base_url}{preview.url}"

    def __enter__(self) -> "Sandbox":
        return self

    def __exit__(self, exc_type, exc, tb) -> None:
        try:
            self.delete()
        except SandboxError:
            # Swallow cleanup errors — the user's exc (if any) is more
            # interesting than a double-delete of an already-gone
            # sandbox. Mirrors v2 behaviour.
            pass


class _ProcessNamespace:
    """`sb.process.*` surface. Delegates to the toolbox client."""

    def __init__(self, tb: "ToolboxClient"):
        self._tb = tb

    def exec(
        self,
        cmd,
        env=None,
        workdir: str = "",
        stdin=None,
        timeout: float = 30.0,
    ):
        """Synchronous exec. Raises ProcessExitError on non-zero exit."""
        if isinstance(cmd, str):
            cmd = ["/bin/sh", "-c", cmd]
        result = self._tb.exec(cmd, env=env, workdir=workdir, stdin=stdin, timeout=timeout)
        if result.exit_code != 0:
            raise ProcessExitError(result.exit_code, result.stdout_text, result.stderr_text)
        return result

    def exec_stream(self, cmd, env=None, workdir: str = "", stdin=None, timeout: float = 300.0):
        """Yield (channel, bytes) frames as the agent produces them.
        `channel` is 1 (stdout), 2 (stderr), or 0 (exit frame)."""
        if isinstance(cmd, str):
            cmd = ["/bin/sh", "-c", cmd]
        return self._tb.exec_stream(cmd, env=env, workdir=workdir, stdin=stdin, timeout=timeout)

    def start(self, cmd, env=None, workdir: str = "", stdin=None, timeout: float = 300.0):
        """Launch a command in the background. Returns an iterator of
        exec_stream frames — callers can drive it to completion or run
        it in a thread. Intentionally minimal; v2's Session handle with
        pause/resume will land when the agent exposes process control."""
        return self.exec_stream(cmd, env=env, workdir=workdir, stdin=stdin, timeout=timeout)


class _FSNamespace:
    """`sb.fs.*` surface. Delegates to the toolbox client's file API."""

    def __init__(self, tb: "ToolboxClient"):
        self._tb = tb

    def write_file(self, path: str, data: bytes) -> None:
        self._tb.upload(path, data)

    def read_file(self, path: str) -> bytes:
        return self._tb.download(path)

    def list_dir(self, path: str):
        return self._tb.list_files(path)

    def remove(self, path: str) -> None:
        self._tb.delete_file(path)

    def mkdir(self, path: str, parents: bool = True) -> None:
        self._tb.mkdir(path, parents=parents)

    def chmod(self, path: str, mode: int) -> None:
        self._tb.chmod(path, mode)

    def rename(self, src: str, dst: str) -> None:
        self._tb.rename(src, dst)


class ProcessExitError(SandboxError):
    """Raised by sb.process.exec(cmd) when cmd exits non-zero. Carries
    the exit code + captured stdout/stderr so the caller can log or
    recover."""

    def __init__(self, exit_code: int, stdout: str, stderr: str):
        super().__init__(f"process exited with code {exit_code}")
        self.exit_code = exit_code
        self.stdout = stdout
        self.stderr = stderr


class TemplateNotFound(SandboxError):
    """client.create_sandbox(template=...) when the template name is
    unknown. Usually means sandboxd wasn't started with -kernel-path,
    so the base templates weren't auto-registered."""


class PoolExhausted(SandboxError):
    """lease_sandbox when the pool has no paused entries and can't cold-
    fall-back in time. Typically transient under burst load."""


class RuntimeUnreachable(SandboxError):
    """Sandboxd can't reach the gocracker runtime (KVM missing, jailer
    misconfigured, etc.). Distinct from SandboxNotFound so callers can
    retry transient reachability vs. logic errors."""


class SandboxTimeout(SandboxError):
    """An operation (exec, lease, probe) exceeded its deadline."""


@dataclass
class Pool:
    template_id: str
    image: str = ""
    kernel_path: str = ""
    mem_mb: int = 0
    cpus: int = 0
    jailer_mode: str = ""
    min_paused: int = 0
    max_paused: int = 0
    counts: Dict[str, int] = field(default_factory=dict)


@dataclass
class Template:
    id: str
    spec_hash: str
    state: str
    snapshot_dir: str = ""
    last_error: str = ""
    created_at: str = ""
    updated_at: str = ""
    spec: Dict[str, Any] = field(default_factory=dict)


@dataclass
class Preview:
    token: str
    url: str
    subdomain: str
    expires_at: str


class Client:
    """Sandboxd HTTP client.

    The base URL should include scheme + host + port, e.g.
    `http://127.0.0.1:9091`. No trailing slash.
    """

    def __init__(self, base_url: str, timeout: float = 30.0):
        self.base_url = base_url.rstrip("/")
        self.timeout = timeout
        # Module-level opener with keep-alive. A single user would
        # normally have a single Client; if they need more throughput
        # they spin up another instance.
        self._opener = urllib.request.build_opener(urllib.request.HTTPHandler())

    # ---- Sandbox lifecycle ----

    def create_sandbox(
        self,
        image: str = "",
        kernel_path: str = "",
        mem_mb: int = 0,
        cpus: int = 0,
        entrypoint: Optional[List[str]] = None,
        cmd: Optional[List[str]] = None,
        env: Optional[List[str]] = None,
        workdir: str = "",
        network_mode: str = "",
        jailer_mode: str = "",
        dockerfile: str = "",
        context: str = "",
        template: str = "",
    ) -> Sandbox:
        # Template resolution: `create_sandbox(template="base-python")`
        # looks up the registered template, copies its image/kernel/mem
        # into the request, then falls through to the normal create path.
        # The underlying template's snapshot is already in the warm cache
        # so container.Run hits the restore fast path instead of a cold boot.
        if template:
            try:
                t = self.get_template(template)
            except SandboxError as e:
                raise TemplateNotFound(
                    f"template {template!r} is unknown. If you expect base templates "
                    f"(base-python/node/bun/go) to be preregistered, make sure sandboxd "
                    f"was started with -kernel-path or $GOCRACKER_KERNEL set."
                ) from e
            spec = t.spec or {}
            image = image or spec.get("image", "")
            kernel_path = kernel_path or spec.get("kernel_path", "")
            mem_mb = mem_mb or int(spec.get("mem_mb", 0) or 0)
            cpus = cpus or int(spec.get("cpus", 0) or 0)
            # NOTE: we intentionally do NOT forward the template's
            # snapshot_dir. Passing it to container.Run triggers a
            # direct restore + reIPGuest, which hits a 15 s vsock dial
            # timeout against the old exec agent on port 10022 even
            # when the snapshot was captured cleanly. container.Run's
            # own warmcache.Lookup (keyed on image+kernel+mem+cpus+...)
            # will still find the right dir and go through the fast
            # restore path without reIPGuest on its IP-preasignado
            # branch. Net effect: same restore speed, no 15 s stall.
        req: Dict[str, Any] = {"kernel_path": kernel_path}
        if image:
            req["image"] = image
        if dockerfile:
            req["dockerfile"] = dockerfile
        if context:
            req["context"] = context
        if mem_mb:
            req["mem_mb"] = mem_mb
        if cpus:
            req["cpus"] = cpus
        if entrypoint:
            req["entrypoint"] = entrypoint
        if cmd:
            req["cmd"] = cmd
        if env:
            req["env"] = env
        if workdir:
            req["workdir"] = workdir
        if network_mode:
            req["network_mode"] = network_mode
        if jailer_mode:
            req["jailer_mode"] = jailer_mode
        resp = self._post("/sandboxes", req)
        return self._parse_sandbox(resp.get("sandbox", {}))

    def list_sandboxes(self) -> List[Sandbox]:
        resp = self._get("/sandboxes")
        return [self._parse_sandbox(x) for x in resp.get("sandboxes", [])]

    def get_sandbox(self, id: str) -> Sandbox:
        return self._parse_sandbox(self._get(f"/sandboxes/{id}"))

    def delete(self, id: str) -> None:
        self._request("DELETE", f"/sandboxes/{id}", body=None, expect_status={204})

    def recycle(self, id: str) -> Sandbox:
        """Recycle a leased sandbox: tear it down and return a fresh
        one from the same pool in a single round trip. Returns a new
        Sandbox (different id); the old `id` is gone after this call."""
        resp = self._post(f"/sandboxes/{id}/recycle", body=None)
        return self._parse_sandbox(resp.get("sandbox", {}))

    # ---- Pool ----

    def register_pool(
        self,
        template_id: str,
        image: str = "",
        kernel_path: str = "",
        mem_mb: int = 0,
        cpus: int = 0,
        jailer_mode: str = "",
        min_paused: int = 0,
        max_paused: int = 0,
        from_template: str = "",
    ) -> Pool:
        req: Dict[str, Any] = {"template_id": template_id}
        for k, v in (
            ("from_template", from_template),
            ("image", image),
            ("kernel_path", kernel_path),
            ("mem_mb", mem_mb),
            ("cpus", cpus),
            ("jailer_mode", jailer_mode),
            ("min_paused", min_paused),
            ("max_paused", max_paused),
        ):
            if v:
                req[k] = v
        resp = self._post("/pools", req)
        return self._parse_pool(resp)

    def list_pools(self) -> List[Pool]:
        resp = self._get("/pools")
        return [self._parse_pool(x) for x in resp.get("pools", [])]

    def unregister_pool(self, template_id: str) -> None:
        self._request("DELETE", f"/pools/{template_id}", body=None, expect_status={204})

    def lease_sandbox(self, template_id: str, timeout_ns: int = 0) -> Sandbox:
        """Pull a warm sandbox from the named pool. Blocks server-side
        until available or timeout elapses. `timeout_ns` is passed as
        Go `time.Duration` via JSON-number nanoseconds (the server's
        decoder handles both nanoseconds and sub-unit suffixes)."""
        req: Dict[str, Any] = {"template_id": template_id}
        if timeout_ns:
            req["timeout"] = timeout_ns
        resp = self._post("/sandboxes/lease", req)
        return self._parse_sandbox(resp.get("sandbox", {}))

    # ---- Templates ----

    def create_template(
        self,
        image: str = "",
        kernel_path: str = "",
        dockerfile: str = "",
        context: str = "",
        mem_mb: int = 0,
        cpus: int = 0,
        id: str = "",
        cmd: Optional[List[str]] = None,
        env: Optional[List[str]] = None,
        workdir: str = "",
        readiness: Optional[Dict[str, Any]] = None,
    ) -> Template:
        req: Dict[str, Any] = {}
        for k, v in (
            ("id", id),
            ("image", image),
            ("dockerfile", dockerfile),
            ("context", context),
            ("kernel_path", kernel_path),
            ("mem_mb", mem_mb),
            ("cpus", cpus),
            ("workdir", workdir),
        ):
            if v:
                req[k] = v
        if cmd:
            req["cmd"] = cmd
        if env:
            req["env"] = env
        if readiness is not None:
            # readiness = {"http_port": int, "http_path": str, "timeout": ns, "interval": ns}
            req["readiness"] = readiness
        resp = self._post("/templates", req)
        return self._parse_template(resp.get("template", {}))

    def list_templates(self) -> List[Template]:
        resp = self._get("/templates")
        return [self._parse_template(x) for x in resp.get("templates", [])]

    def get_template(self, id: str) -> Template:
        return self._parse_template(self._get(f"/templates/{id}"))

    def delete_template(self, id: str) -> None:
        self._request("DELETE", f"/templates/{id}", body=None, expect_status={204})

    # ---- Previews ----

    def mint_preview(self, sandbox_id: str, port: int) -> Preview:
        resp = self._post(f"/sandboxes/{sandbox_id}/preview/{port}", body=None)
        return Preview(
            token=resp["token"],
            url=resp["url"],
            subdomain=resp.get("subdomain", ""),
            expires_at=resp.get("expires_at", ""),
        )

    # ---- Healthz ----

    def healthz(self) -> bool:
        try:
            resp = self._get("/healthz")
            return bool(resp.get("ok"))
        except SandboxError:
            return False

    # ---- Internals ----

    def _post(self, path: str, body: Any) -> Dict[str, Any]:
        return self._request("POST", path, body=body, expect_status={200, 201})

    def _get(self, path: str) -> Dict[str, Any]:
        return self._request("GET", path, body=None, expect_status={200})

    def _request(
        self,
        method: str,
        path: str,
        body: Any,
        expect_status: set,
    ) -> Dict[str, Any]:
        url = self.base_url + path
        data: Optional[bytes] = None
        headers = {"Accept": "application/json"}
        if body is not None:
            data = json.dumps(body).encode("utf-8")
            headers["Content-Type"] = "application/json"
        req = urllib.request.Request(url, data=data, method=method, headers=headers)
        try:
            resp = self._opener.open(req, timeout=self.timeout)
        except urllib.error.HTTPError as e:
            err_body = e.read().decode("utf-8", errors="replace")
            raise self._wrap_http_error(e.code, err_body) from None
        except urllib.error.URLError as e:
            raise SandboxError(f"{method} {path}: {e.reason}")

        with resp:
            status = resp.getcode()
            raw = resp.read()
        if status not in expect_status:
            raise SandboxError(
                f"{method} {path}: unexpected status {status}", status=status, body=raw.decode("utf-8", "replace")
            )
        if status == 204 or not raw:
            return {}
        return json.loads(raw.decode("utf-8"))

    @staticmethod
    def _wrap_http_error(status: int, body: str) -> SandboxError:
        # The sandboxd error shape is {"error": "..."} — unwrap for
        # friendlier messages.
        msg = body
        try:
            parsed = json.loads(body)
            if isinstance(parsed, dict) and parsed.get("error"):
                msg = parsed["error"]
        except Exception:
            pass
        if status == 404:
            return SandboxNotFound(msg, status=status, body=body)
        if status == 400:
            return SandboxInvalidRequest(msg, status=status, body=body)
        if status == 409:
            return SandboxConflict(msg, status=status, body=body)
        return SandboxError(msg, status=status, body=body)

    def _parse_sandbox(self, d: Dict[str, Any]) -> Sandbox:
        return Sandbox(
            id=d.get("id", ""),
            state=d.get("state", ""),
            image=d.get("image", ""),
            uds_path=d.get("uds_path", ""),
            guest_ip=d.get("guest_ip", ""),
            runtime_id=d.get("runtime_id", ""),
            created_at=d.get("created_at", ""),
            error=d.get("error", ""),
            _client=self,
        )

    def _parse_pool(self, d: Dict[str, Any]) -> Pool:
        return Pool(
            template_id=d.get("template_id", ""),
            image=d.get("image", ""),
            kernel_path=d.get("kernel_path", ""),
            mem_mb=d.get("mem_mb", 0),
            cpus=d.get("cpus", 0),
            jailer_mode=d.get("jailer_mode", ""),
            min_paused=d.get("min_paused", 0),
            max_paused=d.get("max_paused", 0),
            counts=d.get("counts", {}) or {},
        )

    def _parse_template(self, d: Dict[str, Any]) -> Template:
        return Template(
            id=d.get("id", ""),
            spec_hash=d.get("spec_hash", ""),
            state=d.get("state", ""),
            snapshot_dir=d.get("snapshot_dir", ""),
            last_error=d.get("last_error", ""),
            created_at=d.get("created_at", ""),
            updated_at=d.get("updated_at", ""),
            spec=d.get("spec", {}) or {},
        )


def _lazy_time():
    """Monkey-patch-friendly clock for tests."""
    return time.time()
