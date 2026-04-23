"""gocracker Python SDK.

Minimal client for the sandboxd HTTP control plane + the in-guest
toolbox agent. Designed to match the HTTP shape exactly so the
existing Go server tests double as reference behaviour.

Usage (Daytona-style):

    from gocracker import Client

    client = Client("http://127.0.0.1:9091")
    with client.create_sandbox(template="base-python") as sb:
        r = sb.process.exec('python3 -c "print(2+2)"')
        sb.fs.write_file('/tmp/x', b'hi')
        url = sb.preview_url(8080)
"""
from .client import (
    Client,
    Sandbox,
    SandboxError,
    SandboxNotFound,
    SandboxInvalidRequest,
    SandboxConflict,
    ProcessExitError,
    TemplateNotFound,
    PoolExhausted,
    RuntimeUnreachable,
    SandboxTimeout,
)
from .toolbox import ExecResult, ToolboxClient, ToolboxError

__all__ = [
    "Client",
    "Sandbox",
    "SandboxError",
    "SandboxNotFound",
    "SandboxInvalidRequest",
    "SandboxConflict",
    "ProcessExitError",
    "TemplateNotFound",
    "PoolExhausted",
    "RuntimeUnreachable",
    "SandboxTimeout",
    "ExecResult",
    "ToolboxClient",
    "ToolboxError",
]
