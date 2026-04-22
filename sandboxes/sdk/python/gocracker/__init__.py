"""gocracker Python SDK.

Minimal client for the sandboxd HTTP control plane + the in-guest
toolbox agent. Designed to match the HTTP shape exactly so the
existing Go server tests double as reference behaviour.

Usage:

    from gocracker import Client

    client = Client("http://127.0.0.1:9091")
    sb = client.create_sandbox(image="alpine:3.20", kernel_path="/abs/path")
    result = sb.toolbox().exec(["echo", "hello"])
    print(result.stdout)
    client.delete(sb.id)
"""
from .client import Client, Sandbox, SandboxError, SandboxNotFound
from .toolbox import ExecResult, ToolboxClient, ToolboxError

__all__ = [
    "Client",
    "Sandbox",
    "SandboxError",
    "SandboxNotFound",
    "ExecResult",
    "ToolboxClient",
    "ToolboxError",
]
