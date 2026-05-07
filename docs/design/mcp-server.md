# gocracker MCP Server — Design

Status: **MVP shipped on `feat/mcp-server`** (this branch). 5 tools, stdio
transport, JSON-RPC 2.0 per [modelcontextprotocol.io spec rev 2025-11-25](https://modelcontextprotocol.io/specification/2025-11-25/basic/transports).

## TL;DR

`gocracker-mcp` is a thin JSON-RPC server that lets AI clients
(Claude Desktop, Claude Code, custom MCP-aware agents) execute code
in gocracker sandboxes by calling well-typed tools. Every tool is a
~30–80 LoC translator over `sandboxes/sdk/go`; the server owns no
VMM state.

The differentiator vs E2B / Daytona / Cloudflare Code Mode / Arrakis
is that it surfaces gocracker's unique primitives — most importantly
the **`process.eval_node` warm-runtime tool** that lands at ~24 ms in-
guest (vs ~36 ms for fork+exec'ed `node`) because V8 is already
initialised inside the snapshot.

## Architecture

```
┌──────────────────┐    JSON-RPC 2.0     ┌──────────────────┐    HTTP    ┌──────────────────┐    vsock UDS    ┌─────────────┐
│  Claude Desktop  │ ◄──── stdio ──────► │  gocracker-mcp   │ ─────────► │ gocracker-sandboxd │ ──────────────► │ guest VM    │
│  Claude Code     │                     │  (this binary)   │            │  (control plane)   │                 │ + toolbox   │
│  custom client   │                     │                  │            │                    │                 │  agent      │
└──────────────────┘                     └──────────────────┘            └──────────────────┘                 └─────────────┘
                                                  │
                                                  │  (HTTP transport for
                                                  │   multi-tenant — TBD)
                                                  ▼
                                          ┌──────────────────┐
                                          │  remote AI cluster │
                                          └──────────────────┘
```

Two transports planned, one shipped:

- **stdio (shipped)** — the default for desktop integrations. The MCP
  client spawns `gocracker-mcp` as a subprocess, pipes JSON-RPC frames
  in stdin, reads responses on stdout. Diagnostic logs go to stderr
  (NEVER stdout — that's the JSON-RPC channel).
- **streamable HTTP (deferred)** — for remote multi-tenant. Same
  `Server.Handle` core; just a different framing layer. Bearer-token
  auth via `--auth-token` (already wired through, not enforced yet).

## MVP tools (this commit)

All 5 tools accept arguments per their JSON Schema and return
results as `text` content blocks containing JSON.

| Tool | Argument shape | Result | Backed by |
|---|---|---|---|
| `sandbox.lease` | `{template_id, timeout_ms?}` | `{id, uds_path, guest_ip, state, leased_at}` | `Client.LeaseSandbox` |
| `sandbox.delete` | `{id}` | `{ok, id}` | `Client.Delete` |
| `sandbox.recycle` | `{id}` | new sandbox shape | `Client.Recycle` |
| `process.exec` | `{sandbox_id, cmd[], env?, env_map?, workdir?, timeout_ms?, stdin?}` | `{stdout, stderr, exit_code, wall_ms}` | `ToolboxClient.Exec` |
| `process.eval_node` | `{sandbox_id, source, timeout_ms?}` | `{stdout, stderr, exit_code, wall_ms}` | `ToolboxClient.Exec` with `cmd[0]="node-warm"` |

`process.eval_node` is the marquee differentiator. It only works against
sandboxes leased from a `Runtime="node"` template (e.g. `base-node-warm`,
auto-registered by sandboxd). The toolbox agent's exec dispatcher routes
`cmd[0] == "node-warm"` to the in-guest pre-loaded REPL on
`/run/gocracker/warm-node.sock` instead of fork+exec'ing fresh node —
see [internal/toolbox/agent/exec.go](../../internal/toolbox/agent/exec.go)
`runWarmEvalNode`.

## Wire format

JSON-RPC 2.0, line-delimited (one JSON object per stdin line).

```jsonc
// Client → server
{"jsonrpc":"2.0","id":1,"method":"initialize","params":{
  "protocolVersion":"2025-11-25",
  "clientInfo":{"name":"claude-desktop","version":"0.7.1"}
}}

// Server → client
{"jsonrpc":"2.0","id":1,"result":{
  "protocolVersion":"2025-11-25",
  "capabilities":{"tools":{}},
  "serverInfo":{"name":"gocracker-mcp","version":"dev"}
}}

// Client → server (notification, no id, no response)
{"jsonrpc":"2.0","method":"notifications/initialized"}

// Client lists tools
{"jsonrpc":"2.0","id":2,"method":"tools/list"}
// → {"jsonrpc":"2.0","id":2,"result":{"tools":[...5 entries...]}}

// Client invokes a tool
{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{
  "name":"sandbox.lease",
  "arguments":{"template_id":"base-node-warm"}
}}
// → {"jsonrpc":"2.0","id":3,"result":{
//      "content":[{"type":"text","text":"{\"id\":\"sb-...\", ...}"}],
//      "isError":false
//    }}
```

Errors come back two ways depending on severity:

- **Protocol-level errors** (parse, unknown method, bad params) → JSON-RPC
  `error` object with codes from the standard space (-32xxx).
- **Tool-level errors** (sandbox not found, exec failed) → successful
  JSON-RPC `result` with `isError: true` and the message in the text
  content block. The LLM sees these inside its own context and can
  adjust its next call.

## Code layout

- [`sandboxes/cmd/gocracker-mcp/main.go`](../../sandboxes/cmd/gocracker-mcp/main.go)
  — entry point. Parses flags, builds SDK client, calls `Server.ServeStdio`.
- [`sandboxes/internal/mcp/protocol.go`](../../sandboxes/internal/mcp/protocol.go)
  — wire types (Request, Response, Error, Tool, etc.) and constants.
- [`sandboxes/internal/mcp/server.go`](../../sandboxes/internal/mcp/server.go)
  — Server core, dispatch, stdio loop.
- [`sandboxes/internal/mcp/tools.go`](../../sandboxes/internal/mcp/tools.go)
  — registers all default tools and their handlers. Adding a new tool
  is one new `register*` function.
- [`sandboxes/internal/mcp/util.go`](../../sandboxes/internal/mcp/util.go)
  — small helpers (deterministic tool sort, stderr sink indirection).
- [`sandboxes/internal/mcp/server_test.go`](../../sandboxes/internal/mcp/server_test.go)
  — 10 tests covering initialize, tools/list, tools/call happy +
  error paths, JSON-RPC version validation, parse errors, the stdio
  loop. All run against an `httptest`-backed fake sandboxd; no real
  KVM needed.

## Next phases (out of scope for this MVP)

These are the ranked novel ideas that didn't make the MVP cut. Each
gets its own PR.

1. **`sandbox.fan_out`** — N=4..64 microsecond CoW fork in a single
   RPC. Backed by gocracker's dirty-page-delta restore primitive
   (`pkg/vmm/migration.go`). No competitor exposes batch-fork.
   Estimated ~200 LoC.
2. **`speculate.race`** — fork sandbox into N children, run a
   different candidate command in each, return the first to satisfy
   `success_predicate`, kill the rest. Maps directly to LangGraph's
   parallel/speculative mode. ~150 LoC.
3. **`checkpoint.tree.diff`** — given two checkpoints, return what
   changed (files, env, RSS pages). Surfaces the dirty-log capture
   gocracker already does internally, in a verb no other MCP server
   has. Needs a new sandboxd HTTP route. ~250 LoC.
4. **`process.exec_stream`** — streaming stdout/stderr to the LLM as
   the process runs. Wraps `ToolboxClient.ExecStream`. SSE-over-MCP
   is just landed in the spec; ~80 LoC.
5. **`files.put` / `files.get`** — upload/download a single file to
   the sandbox. Wraps `ToolboxClient.Upload`/`Download`. ~50 LoC.
6. **`preview.mint`** — mint an HMAC-signed preview URL the user can
   click to open a guest-side service. Wraps `Client.MintPreview`.
   ~25 LoC.
7. **HTTP transport** — streamable HTTP with bearer-token auth, for
   remote multi-tenant. Same `Server.Handle` core. ~150 LoC.

## Quick start

```bash
# Terminal 1: sandboxd
sudo gocracker-sandboxd serve --addr 127.0.0.1:9091 \
    --kernel-path artifacts/kernels/gocracker-guest-standard-vmlinux

# Terminal 2: spawn an MCP server pointing at it
gocracker-mcp --sandboxd http://127.0.0.1:9091

# Terminal 3: smoke-test
(echo '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","clientInfo":{"name":"curl","version":"1"}}}'
 echo '{"jsonrpc":"2.0","method":"notifications/initialized"}'
 echo '{"jsonrpc":"2.0","id":2,"method":"tools/list"}') \
| gocracker-mcp --sandboxd http://127.0.0.1:9091 \
| jq .
```

## Claude Desktop integration

Add to `~/Library/Application Support/Claude/claude_desktop_config.json`
(macOS) or the equivalent on Linux/Windows:

```json
{
  "mcpServers": {
    "gocracker": {
      "command": "/usr/local/bin/gocracker-mcp",
      "args": ["--sandboxd", "http://127.0.0.1:9091"]
    }
  }
}
```

Restart Claude Desktop. Ask: *"Lease a base-node-warm sandbox and run
`console.log(process.version)` in it."* — Claude calls
`sandbox.lease({template_id: "base-node-warm"})` then
`process.eval_node({sandbox_id, source})` and replies with the output.

## Why this matters

A 24 ms in-guest eval makes per-tool-call sandboxing competitive with
plain function calls in a chat agent loop. The state-of-the-art
"execute code" MCP servers either run Docker (100 ms+ cold start),
V8 isolates (no filesystem, no process state), or cold microVMs
(~125–500 ms even warm). gocracker's warm-pool + node-warm gets the
~10 ms ms-floor while keeping a real Linux guest with files,
processes, snapshots — the AI can `pip install`, write to `/tmp`,
fork a server, and have all of it inside a real isolation boundary.
