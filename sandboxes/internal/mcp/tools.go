package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	gosdk "github.com/gocracker/gocracker/sandboxes/sdk/go"
)

// registerDefaultTools wires the MVP tool set onto the server. Each
// tool is a thin translator between MCP-shape JSON and the gocracker
// SDK; the actual VMM work happens server-side.
//
// Adding a new tool: append a register*(s) call here, define its
// handler in this file, and bump the inputSchema rawJSON. There's no
// reflection-driven schema generator yet — schemas are hand-written
// to keep the dependency surface tiny.
func registerDefaultTools(s *Server) {
	registerSandboxLease(s)
	registerSandboxDelete(s)
	registerSandboxRecycle(s)
	registerProcessExec(s)
	registerProcessEvalNode(s)
}

// ---- sandbox.lease ----

type sandboxLeaseArgs struct {
	TemplateID string `json:"template_id"`
	TimeoutMs  int    `json:"timeout_ms,omitempty"`
}

type sandboxLeaseResult struct {
	ID       string `json:"id"`
	UDSPath  string `json:"uds_path,omitempty"`
	GuestIP  string `json:"guest_ip,omitempty"`
	State    string `json:"state"`
	LeasedAt string `json:"leased_at"`
}

func registerSandboxLease(s *Server) {
	s.RegisterTool(Tool{
		Name: "sandbox.lease",
		Description: `Lease a warm-pool sandbox from gocracker-sandboxd. Returns the sandbox ID + UDS path. ` +
			`Backed by gocracker's sub-30 ms warm-pool restore primitive (dirty-page-delta CoW). ` +
			`Use process.exec or process.eval_node against the returned id to run code.`,
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "template_id": {"type": "string", "description": "Pool template ID, e.g. base-node, base-node-warm, base-python"},
    "timeout_ms":  {"type": "integer", "description": "Max wait for a free pool slot (default 5000)"}
  },
  "required": ["template_id"]
}`),
	}, func(ctx context.Context, raw json.RawMessage) (CallToolResult, error) {
		var args sandboxLeaseArgs
		if err := json.Unmarshal(raw, &args); err != nil {
			return errResult("decode args: " + err.Error()), nil
		}
		if args.TemplateID == "" {
			return errResult("template_id required"), nil
		}
		req := gosdk.LeaseSandboxRequest{TemplateID: args.TemplateID}
		if args.TimeoutMs > 0 {
			req.Timeout = time.Duration(args.TimeoutMs) * time.Millisecond
		}
		sb, err := s.Sandboxd.LeaseSandbox(ctx, req)
		if err != nil {
			return errResult("lease: " + err.Error()), nil
		}
		out := sandboxLeaseResult{
			ID:       sb.ID,
			UDSPath:  sb.UDSPath,
			GuestIP:  sb.GuestIP,
			State:    sb.State,
			LeasedAt: sb.CreatedAt.UTC().Format(time.RFC3339Nano),
		}
		return jsonResult(out), nil
	})
}

// ---- sandbox.delete ----

type sandboxDeleteArgs struct {
	ID string `json:"id"`
}

func registerSandboxDelete(s *Server) {
	s.RegisterTool(Tool{
		Name:        "sandbox.delete",
		Description: `Delete (or release back to pool) the given sandbox. Idempotent; safe to call on already-released sandboxes.`,
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "id": {"type": "string"}
  },
  "required": ["id"]
}`),
	}, func(ctx context.Context, raw json.RawMessage) (CallToolResult, error) {
		var args sandboxDeleteArgs
		if err := json.Unmarshal(raw, &args); err != nil {
			return errResult("decode args: " + err.Error()), nil
		}
		if args.ID == "" {
			return errResult("id required"), nil
		}
		if err := s.Sandboxd.Delete(ctx, args.ID); err != nil {
			return errResult("delete: " + err.Error()), nil
		}
		return jsonResult(map[string]any{"ok": true, "id": args.ID}), nil
	})
}

// ---- sandbox.recycle ----

func registerSandboxRecycle(s *Server) {
	s.RegisterTool(Tool{
		Name: "sandbox.recycle",
		Description: `Release a leased sandbox back to its pool and immediately lease a fresh one from the same pool. ` +
			`Equivalent to delete+lease in one round-trip. Useful between AI tool calls when the agent wants a clean filesystem.`,
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "id": {"type": "string"}
  },
  "required": ["id"]
}`),
	}, func(ctx context.Context, raw json.RawMessage) (CallToolResult, error) {
		var args sandboxDeleteArgs
		if err := json.Unmarshal(raw, &args); err != nil {
			return errResult("decode args: " + err.Error()), nil
		}
		if args.ID == "" {
			return errResult("id required"), nil
		}
		sb, err := s.Sandboxd.Recycle(ctx, args.ID)
		if err != nil {
			return errResult("recycle: " + err.Error()), nil
		}
		return jsonResult(sandboxLeaseResult{
			ID:       sb.ID,
			UDSPath:  sb.UDSPath,
			GuestIP:  sb.GuestIP,
			State:    sb.State,
			LeasedAt: sb.CreatedAt.UTC().Format(time.RFC3339Nano),
		}), nil
	})
}

// ---- process.exec ----

type processExecArgs struct {
	SandboxID string   `json:"sandbox_id"`
	Cmd       []string `json:"cmd"`
	// Env is KEY=VALUE strings (matching ExecOptions wire format).
	// We accept both list-of-strings and a string→string map and
	// flatten the map to KEY=VALUE for caller convenience.
	Env       []string          `json:"env,omitempty"`
	EnvMap    map[string]string `json:"env_map,omitempty"`
	WorkDir   string            `json:"workdir,omitempty"`
	TimeoutMs int               `json:"timeout_ms,omitempty"`
	Stdin     string            `json:"stdin,omitempty"`
}

type processExecResult struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int32  `json:"exit_code"`
	WallMs   int64  `json:"wall_ms"`
}

func registerProcessExec(s *Server) {
	s.RegisterTool(Tool{
		Name: "process.exec",
		Description: `Run a command inside the sandbox via the toolbox /exec endpoint. Blocks until the process exits ` +
			`(or timeout), then returns aggregated stdout/stderr/exit_code. For long-running jobs use process.exec_stream.`,
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "sandbox_id": {"type": "string"},
    "cmd":        {"type": "array", "items": {"type": "string"}},
    "env":        {"type": "object", "additionalProperties": {"type": "string"}},
    "workdir":    {"type": "string"},
    "timeout_ms": {"type": "integer"},
    "stdin":      {"type": "string"}
  },
  "required": ["sandbox_id", "cmd"]
}`),
	}, func(ctx context.Context, raw json.RawMessage) (CallToolResult, error) {
		var args processExecArgs
		if err := json.Unmarshal(raw, &args); err != nil {
			return errResult("decode args: " + err.Error()), nil
		}
		if args.SandboxID == "" {
			return errResult("sandbox_id required"), nil
		}
		if len(args.Cmd) == 0 {
			return errResult("cmd required (non-empty array)"), nil
		}
		tb, err := s.toolboxFor(ctx, args.SandboxID)
		if err != nil {
			return errResult(err.Error()), nil
		}
		// Flatten env_map into KEY=VALUE for the SDK. Both args.Env and
		// args.EnvMap may be set; we concatenate, with EnvMap winning
		// for any duplicate keys (last write wins per env(7) semantics).
		env := append([]string{}, args.Env...)
		for k, v := range args.EnvMap {
			env = append(env, k+"="+v)
		}
		opts := gosdk.ExecOptions{
			Env:     env,
			WorkDir: args.WorkDir,
			Stdin:   []byte(args.Stdin),
		}
		if args.TimeoutMs > 0 {
			opts.Timeout = time.Duration(args.TimeoutMs) * time.Millisecond
		}
		t0 := time.Now()
		res, err := tb.Exec(ctx, args.Cmd, opts)
		if err != nil {
			return errResult("exec: " + err.Error()), nil
		}
		return jsonResult(processExecResult{
			Stdout:   string(res.Stdout),
			Stderr:   string(res.Stderr),
			ExitCode: res.ExitCode,
			WallMs:   time.Since(t0).Milliseconds(),
		}), nil
	})
}

// ---- process.eval_node ----

type processEvalNodeArgs struct {
	SandboxID string `json:"sandbox_id"`
	Source    string `json:"source"`
	TimeoutMs int    `json:"timeout_ms,omitempty"`
}

func registerProcessEvalNode(s *Server) {
	s.RegisterTool(Tool{
		Name: "process.eval_node",
		Description: `Evaluate JavaScript inside the sandbox using a pre-loaded V8 instance ` +
			`(node-warm runtime). Skips ~25–50 ms of node startup vs running a fresh node process. ` +
			`Requires the sandbox to be from a template with Runtime="node" (e.g. base-node-warm). ` +
			`State (globals) persists across calls in the same sandbox — useful for stateful AI loops.`,
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "sandbox_id": {"type": "string"},
    "source":     {"type": "string", "description": "JS source. Use process.stdout.write(value) to return data."},
    "timeout_ms": {"type": "integer", "description": "Per-eval timeout (default 30000)"}
  },
  "required": ["sandbox_id", "source"]
}`),
	}, func(ctx context.Context, raw json.RawMessage) (CallToolResult, error) {
		var args processEvalNodeArgs
		if err := json.Unmarshal(raw, &args); err != nil {
			return errResult("decode args: " + err.Error()), nil
		}
		if args.SandboxID == "" {
			return errResult("sandbox_id required"), nil
		}
		if args.Source == "" {
			return errResult("source required"), nil
		}
		tb, err := s.toolboxFor(ctx, args.SandboxID)
		if err != nil {
			return errResult(err.Error()), nil
		}
		opts := gosdk.ExecOptions{}
		if args.TimeoutMs > 0 {
			opts.Timeout = time.Duration(args.TimeoutMs) * time.Millisecond
		}
		t0 := time.Now()
		// node-warm protocol: cmd[0]="node-warm" cmd[1]=source. Agent
		// routes to the in-guest REPL on /run/gocracker/warm-node.sock
		// instead of fork+exec'ing fresh node. See
		// internal/toolbox/agent/exec.go runWarmEvalNode.
		res, err := tb.Exec(ctx, []string{"node-warm", args.Source}, opts)
		if err != nil {
			return errResult("eval_node: " + err.Error()), nil
		}
		return jsonResult(processExecResult{
			Stdout:   string(res.Stdout),
			Stderr:   string(res.Stderr),
			ExitCode: res.ExitCode,
			WallMs:   time.Since(t0).Milliseconds(),
		}), nil
	})
}

// ---- helpers ----

// toolboxFor builds a ToolboxClient bound to a sandbox's UDS. We have
// to look up the sandbox first to get its UDS path, since the SDK
// doesn't memoise.
func (s *Server) toolboxFor(ctx context.Context, sandboxID string) (*gosdk.ToolboxClient, error) {
	sb, err := s.Sandboxd.GetSandbox(ctx, sandboxID)
	if err != nil {
		return nil, fmt.Errorf("get sandbox %s: %w", sandboxID, err)
	}
	if sb.UDSPath == "" {
		return nil, fmt.Errorf("sandbox %s has no UDS path (state=%s)", sandboxID, sb.State)
	}
	return sb.Toolbox(), nil
}

// errResult wraps a string as an MCP error-shaped CallToolResult. We
// surface tool errors as `isError: true` content (per MCP spec)
// rather than JSON-RPC errors, so the LLM sees the message in its
// own context and can adjust.
func errResult(msg string) CallToolResult {
	return CallToolResult{
		Content: []ContentItem{TextContent(msg)},
		IsError: true,
	}
}

// jsonResult marshals v as JSON and wraps it in a single text content
// block. The LLM is expected to parse it; using a structured content
// type would require the spec's `resource` shape with a base64+mime
// payload, which is overkill for our small JSON results.
func jsonResult(v any) CallToolResult {
	data, err := json.Marshal(v)
	if err != nil {
		return errResult("marshal result: " + err.Error())
	}
	return CallToolResult{
		Content: []ContentItem{TextContent(string(data))},
	}
}
