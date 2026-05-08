# gocracker VS Code Extension — Design

Status: **shipped** (all 3 phases, `feat/mcp-server` branch)

## What it does

Lets users run code in isolated KVM micro-VMs directly from VS Code — no
terminal, no sudo, no Docker. Select code, press a keybind, see output in
a panel. The sandbox is a real Linux VM restored from a warm snapshot in
~30 ms; it disappears automatically when you close the panel.

## Why now

gocracker is now fully rootless for users in the `kvm` group. sandboxd with
`--network-mode slirp` needs no elevated privileges. The MCP server (`gocracker-mcp`)
already works as a subprocess. The extension becomes a UI shell over what is
already working end-to-end.

## Architecture

```
┌─────────────────────────────────────────────────┐
│  VS Code Extension (TypeScript)                 │
│                                                 │
│  ┌───────────────┐  ┌────────────────────────┐  │
│  │ SandboxPanel  │  │ GocrackrClient (HTTP)  │  │
│  │ (WebviewPanel)│  │  → sandboxd :9091      │  │
│  └───────────────┘  └────────────────────────┘  │
│          │                    │                 │
│          └─── run selection ──┘                 │
│                                                 │
│  ┌──────────────────────────────────────────┐   │
│  │ DaemonManager                            │   │
│  │  • starts gocracker-sandboxd if not up   │   │
│  │  • monitors health (:9091/healthz)       │   │
│  │  • kills it on extension deactivate      │   │
│  └──────────────────────────────────────────┘   │
└─────────────────────────────────────────────────┘
```

The extension talks to sandboxd's HTTP API directly — no MCP layer needed
for the UI path. The MCP server is still used for AI tool calls (Claude Code,
Copilot, etc.) which happens via `gocracker-mcp setup` at install time.

## User flows

### Run selection in sandbox (MVP)

1. User selects code in editor.
2. Runs command `gocracker: Run Selection` (keybind: `Ctrl+Shift+G`).
3. Extension detects language from file extension, picks template
   (`base-node`, `base-python`, `base-go`, `base-bun`).
4. Leases a warm sandbox via `POST /sandboxes/lease`.
5. Execs the code via toolbox `/exec` endpoint.
6. Shows stdout/stderr in a `GocrackrOutput` panel (reusable across runs).
7. Deletes the sandbox in the background.

### Run file in sandbox

Same as above but wraps the whole file. For Node.js: `node <tmpfile>`.
For Python: `python3 <tmpfile>`. File is uploaded to the sandbox before exec
via `PUT /sandboxes/{id}/files`.

### Sandbox explorer (sidebar)

A TreeView showing live sandboxes from `GET /sandboxes`. Each entry shows:
- ID (short), template, state, age
- Right-click: exec shell, delete, recycle

### Auto-start daemon

On first use (or via `gocracker: Start Daemon` command), the extension:
1. Checks if sandboxd is running (`GET http://127.0.0.1:9091/healthz`).
2. If not, spawns `gocracker-sandboxd serve --network-mode slirp
   --kernel-path <discovered> --uds-group <user>` as a child process.
3. Waits for healthz to return 200 (polls 100 ms, timeout 10 s).
4. Shows a status bar item: `$(vm) gocracker ready`.

Kernel path discovery order:
1. Extension setting `gocracker.kernelPath`
2. `GOCRACKER_KERNEL` env var
3. `~/.local/share/gocracker/kernels/` (downloaded on install)
4. Repo-relative `artifacts/kernels/` (for developers)

## Configuration

```json
{
  "gocracker.kernelPath": "",
  "gocracker.sandboxdUrl": "http://127.0.0.1:9091",
  "gocracker.networkMode": "slirp",
  "gocracker.defaultMemMb": 256,
  "gocracker.autoStartDaemon": true,
  "gocracker.keepSandboxOnError": false
}
```

## Phases

### Phase 1 — MVP ✅

- [x] Extension scaffold (TypeScript, no webpack)
- [x] `DaemonManager`: start/stop/health-check sandboxd; auto-starts on first run
- [x] `GocrackrClient`: `leaseSandbox`, `exec`, `uploadFile`, `deleteSandbox`, `listSandboxes`
- [x] `Run Selection` command (`Ctrl+Shift+G`) with language detection + template mapping
- [x] `GocrackrOutputPanel`: WebviewPanel showing stdout/stderr with ANSI→HTML support
- [x] Status bar item (ready / stopped)
- [x] Settings: `kernelPath`, `sandboxdUrl`, `networkMode`, `defaultMemMb`, `autoStartDaemon`, `keepSandboxOnError`
- [x] `gocracker: Start Daemon` and `gocracker: Stop Daemon` commands
- [x] README with install instructions (kvm group + kernel setup)
- [x] sandboxd: `POST /sandboxes/{id}/exec` and `PUT /sandboxes/{id}/files` HTTP endpoints

### Phase 2 — File run + explorer ✅

- [x] `Run File` command (`Ctrl+Shift+Alt+G`) — uploads full file, execs, cleans up
- [x] Sandbox explorer TreeView (`GET /sandboxes` polled every 3 s)
- [x] Exec shell command — REPL-style PTY terminal via `vscode.Pseudoterminal`
- [x] Auto-kernel download if none configured (pulls pre-built from GitHub releases, gunzips in-process)
- [x] Right-click context menu: Open Shell, Delete (with confirmation), Recycle

### Phase 3 — AI integration ✅

- [x] `gocracker: Setup MCP` command: calls `gocracker-mcp setup` in a terminal
- [x] `@gocracker` Copilot chat participant — parses fenced code blocks, runs in sandbox, streams result
- [x] `sandbox.fan_out` UI (`Ctrl+Shift+Alt+F`) — N parallel sandboxes, detects identical vs differing outputs

## File layout

```
sandboxes/vscode/
  src/
    extension.ts           # activate / deactivate + all command registration
    daemon.ts              # DaemonManager + downloadKernel()
    client.ts              # GocrackrClient (HTTP fetch, no SDK)
    panel.ts               # GocrackrOutputPanel (WebviewPanel, ANSI→HTML)
    explorer.ts            # SandboxExplorer (TreeView, 3s poll)
    terminal.ts            # openSandboxShell() — REPL PTY terminal
    fanout.ts              # runFanOut() — N parallel sandbox runs
    chat.ts                # @gocracker Copilot chat participant
    language.ts            # file extension → template ID
    config.ts              # settings wrapper
  package.json
  tsconfig.json
  .vscodeignore
  .gitignore
```

## Language → template mapping

| Extension | Language | Template |
|-----------|----------|---------|
| `.js`, `.mjs`, `.ts` | JavaScript/TypeScript | `base-node` |
| `.py` | Python | `base-python` |
| `.go` | Go | `base-go` |
| `.ts` (bun project) | TypeScript | `base-bun` |

Detection: check `package.json` for `"type": "module"` or bun lockfile to
distinguish Node from Bun.

## Install story (end user)

```
1. Install extension from VS Code Marketplace
2. Extension prompts: "Add yourself to the kvm group to run sandboxes"
   → click "Set up" → opens terminal with the usermod command
3. Log out and back in
4. Press Ctrl+Shift+G on any code selection → sandbox spins up in ~30 ms
```

No sudo prompt ever. The kernel is downloaded automatically on first use
if not already configured.

## Open questions

- Should the extension ship or download the kernel? Kernel is ~5 MB
  compressed. Bundling it avoids a download step but makes the `.vsix`
  large. Likely: download on first use, cache in `globalStorageUri`.
- Separate repo vs. monorepo? Monorepo under `sandboxes/vscode/` keeps
  versions in sync; separate repo is cleaner for Marketplace CI. Start
  monorepo, split if needed.
- Windows/macOS: KVM is Linux-only. Extension could show a "not supported
  on this platform" message rather than not loading at all.
