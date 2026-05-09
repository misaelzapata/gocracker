# gocracker — run code in isolated KVM micro-VMs

Run selected code or entire files in real KVM micro-VMs directly from VS Code. gocracker uses Firecracker-style lightweight VMs restored from memory snapshots in ~30 ms — no Docker, no containers, no sudo after the one-time `kvm` group setup. Each run gets a fresh, fully isolated guest; the VM is deleted when the run completes.

## Requirements

- Linux x86-64
- `/dev/kvm` accessible (`ls -l /dev/kvm` should show your user or the `kvm` group)
- `kvm` group membership (see below)

## Installation

Install from the VS Code Marketplace, then run the one-time system setup:

```bash
sudo usermod -aG kvm $USER && newgrp kvm
```

The extension downloads a guest kernel on first use and starts `gocracker-sandboxd` automatically when you run code.

## Usage

| Action | Keybinding | Command |
|---|---|---|
| Run selected code in sandbox | `Ctrl+Shift+G` | `gocracker: Run Selection in Sandbox` |
| Run entire file in sandbox | `Ctrl+Shift+Alt+G` | `gocracker: Run File in Sandbox` |
| Start daemon manually | — | `gocracker: Start Daemon` |
| Stop daemon | — | `gocracker: Stop Daemon` |
| Write MCP tool configs | — | `gocracker: Setup MCP` |
| Refresh sandbox list | — | `gocracker: Refresh` |

Output appears in the **gocracker** panel. The sidebar **gocracker Sandboxes** view lists live VMs.

## Supported Languages

| Extension | Template | Runtime |
|---|---|---|
| `.py` | `base-python` | Python 3 |
| `.js`, `.mjs` | `base-node` | Node.js |
| `.ts` | `base-bun` | Bun (native TypeScript support) |
| `.go` | `base-go` | Go |

## Settings

| Setting | Default | Description |
|---|---|---|
| `gocracker.sandboxdUrl` | `http://127.0.0.1:9091` | sandboxd API base URL |
| `gocracker.kernelPath` | *(auto)* | Path to guest kernel vmlinux; empty = auto-discover |
| `gocracker.networkMode` | `slirp` | `slirp` (rootless), `auto`, or `none` |
| `gocracker.defaultMemMb` | `256` | RAM in MiB per sandbox |
| `gocracker.autoStartDaemon` | `true` | Start sandboxd automatically on first run |
| `gocracker.keepSandboxOnError` | `false` | Keep VM alive after a failed run for inspection |

## Architecture

The extension talks to `gocracker-sandboxd`, a local HTTP daemon that manages Firecracker micro-VMs. When you run code, sandboxd restores a pre-snapshotted VM from memory, the extension uploads your source file, executes it via a lightweight in-guest agent, streams output back, then tears the VM down. Total overhead from cold daemon state is under a second; with a warm snapshot it is typically 30–50 ms.

## Running Without sudo

Network mode `slirp` uses a userspace network stack inside the guest, requiring no root privileges. Combined with the automatic jailer-off configuration, the only privileged step is the one-time `kvm` group membership — all subsequent VM operations run as your regular user.

## License

Apache 2.0 — see [LICENSE](../../LICENSE).
