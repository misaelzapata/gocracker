// Package mcp implements a Model Context Protocol server that exposes
// gocracker's sandbox primitives to AI clients (Claude Desktop, Claude
// Code, custom MCP-aware agents, etc.) over JSON-RPC 2.0.
//
// The server is intentionally a thin translation layer: every MCP tool
// call lands on one or two methods in sandboxes/sdk/go (Client +
// ToolboxClient). It owns no VMM state — sandboxd is the source of
// truth — so the server can be killed and respawned without leaking
// VMs, and multi-tenancy maps cleanly to "one MCP process per LLM
// client session, talking to a shared sandboxd."
//
// Wire protocol (JSON-RPC 2.0, modelcontextprotocol.io spec rev
// 2025-11-25):
//
//   {"jsonrpc":"2.0","id":1,"method":"initialize","params":{...}}
//   {"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-11-25",...}}
//   {"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}
//   {"jsonrpc":"2.0","id":2,"result":{"tools":[...]}}
//   {"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"process.exec","arguments":{...}}}
//   {"jsonrpc":"2.0","id":3,"result":{"content":[{"type":"text","text":"..."}]}}
//
// Transport: stdio for desktop (default) and streamable HTTP for
// remote/multi-tenant. Both call the same Server.Handle method;
// transports just frame requests differently.
package mcp

import "encoding/json"

// ProtocolVersion is the MCP wire-format version this server speaks.
// Bumping it is a breaking change — clients negotiate during
// `initialize` and refuse to talk if their range doesn't include it.
const ProtocolVersion = "2025-11-25"

// JSONRPCVersion is the JSON-RPC 2.0 envelope version. MCP locks to 2.0;
// don't touch this.
const JSONRPCVersion = "2.0"

// Request is a JSON-RPC 2.0 request envelope. Notifications (requests
// with no `id` field) are valid per the JSON-RPC spec but MCP servers
// rarely receive them — we accept and ignore them.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response is a JSON-RPC 2.0 response envelope. Exactly one of
// `result` or `error` is set per the spec.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Error is a JSON-RPC 2.0 error object. Codes follow the standard
// JSON-RPC code space (-32700..-32600 reserved for transport, -32000..
// -32099 reserved for application errors).
type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Standard JSON-RPC 2.0 error codes plus MCP application codes used by
// this server. Keep these stable — clients log them.
const (
	ErrParseError     = -32700
	ErrInvalidRequest = -32600
	ErrMethodNotFound = -32601
	ErrInvalidParams  = -32602
	ErrInternalError  = -32603

	// Application codes for gocracker MCP. Any new code goes BELOW
	// -32100 to stay clear of the JSON-RPC reserved range.
	ErrSandboxNotFound = -32100
	ErrSandboxFailed   = -32101
	ErrToolboxFailed   = -32102
	ErrTimeout         = -32103
)

// InitializeParams is what an MCP client sends in the first request.
// We mostly care about the client info for logging and the protocol
// version for compat checking.
type InitializeParams struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities,omitempty"`
	ClientInfo      ClientInfo     `json:"clientInfo,omitempty"`
}

// ClientInfo identifies the AI client (Claude Desktop, Claude Code,
// custom). We log it on initialize for ops debugging.
type ClientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// InitializeResult is what we return — capabilities we expose plus
// our own server identity.
type InitializeResult struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    ServerCapabilities `json:"capabilities"`
	ServerInfo      ServerInfo         `json:"serverInfo"`
}

// ServerCapabilities advertises which sub-protocols we implement. MVP
// supports tools only; resources, prompts, and sampling are room for
// future expansion (see docs/design/mcp-server.md).
type ServerCapabilities struct {
	Tools     *ToolsCapability     `json:"tools,omitempty"`
	Resources *ResourcesCapability `json:"resources,omitempty"`
	Prompts   *PromptsCapability   `json:"prompts,omitempty"`
}

// ToolsCapability is empty in MVP — `listChanged` notifications would
// go here when we support dynamic tool registration.
type ToolsCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

// ResourcesCapability — placeholder for future per-sandbox `sandbox://`
// URIs (stdout streams, /proc, /fs).
type ResourcesCapability struct {
	Subscribe   bool `json:"subscribe,omitempty"`
	ListChanged bool `json:"listChanged,omitempty"`
}

// PromptsCapability — placeholder for prompt templates exposed to the
// client (e.g. "run the latest test" prompt).
type PromptsCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

// ServerInfo identifies us back to the client. Mirrors ClientInfo.
type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Tool is the MCP-side description of one callable function. Each
// gocracker primitive (exec, lease, fork, etc.) is one Tool.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// ListToolsResult is the response shape for `tools/list`.
type ListToolsResult struct {
	Tools []Tool `json:"tools"`
}

// CallToolParams is the params shape for `tools/call`. The client
// names the tool and provides arguments matching its inputSchema.
type CallToolParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// CallToolResult is what we return from `tools/call`. Per spec the
// content is a list of typed parts; we keep it simple with text-only
// for now (stdout/stderr/JSON results all serialise as one text
// block) and `isError: true` on failures so the LLM can see them.
type CallToolResult struct {
	Content []ContentItem `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

// ContentItem is one part of a CallToolResult. The MCP spec defines
// "text", "image", and "resource" types; MVP uses text only.
type ContentItem struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// TextContent is a convenience wrapper for the common case.
func TextContent(s string) ContentItem {
	return ContentItem{Type: "text", Text: s}
}
