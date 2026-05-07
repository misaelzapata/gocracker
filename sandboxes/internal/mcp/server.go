package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	gosdk "github.com/gocracker/gocracker/sandboxes/sdk/go"
)

// Server is the MCP server core. It owns the tool registry, the
// dispatcher, and a reference to the sandboxd SDK client.
//
// The server is request-scoped per JSON-RPC frame: every request
// flows through Handle(ctx, raw) and produces one Response (or nil
// for notifications). Transports (stdio, HTTP) are responsible for
// framing and concurrency.
//
// Server is safe for concurrent use — tools and the SDK client are
// goroutine-safe by construction.
type Server struct {
	// Sandboxd is the gocracker control-plane client. Required.
	// All sandbox lifecycle ops route through this.
	Sandboxd *gosdk.Client

	// Info identifies the server back to the MCP client during
	// `initialize`. Defaults to "gocracker-mcp"/version-from-build
	// when unset.
	Info ServerInfo

	// initialised tracks whether the client has completed the MCP
	// `initialize` handshake. Most methods refuse to run before this
	// per the spec; we soft-enforce it (log + serve) to keep the
	// server usable from raw `curl` for debugging.
	initialised bool
	mu          sync.Mutex

	// tools is the registered tool table. Built once at NewServer
	// time so dispatch is a map lookup, not a linear scan.
	tools map[string]toolHandler
}

// toolHandler is a registered tool's runtime callback. Args is the raw
// JSON the client sent; the handler decodes it into a typed struct.
type toolHandler struct {
	tool    Tool
	handler func(ctx context.Context, args json.RawMessage) (CallToolResult, error)
}

// NewServer constructs a Server with the default tool set wired to
// the given sandboxd client. Pass info to override the default
// server identity (otherwise we report "gocracker-mcp/dev").
func NewServer(sandboxd *gosdk.Client, info ServerInfo) *Server {
	if info.Name == "" {
		info.Name = "gocracker-mcp"
	}
	if info.Version == "" {
		info.Version = "dev"
	}
	s := &Server{
		Sandboxd: sandboxd,
		Info:     info,
		tools:    map[string]toolHandler{},
	}
	registerDefaultTools(s)
	return s
}

// RegisterTool adds a tool to the server. Panics on duplicate names —
// this is a development-time mistake and we want it loud.
func (s *Server) RegisterTool(t Tool, h func(ctx context.Context, args json.RawMessage) (CallToolResult, error)) {
	if _, dup := s.tools[t.Name]; dup {
		panic("mcp: duplicate tool: " + t.Name)
	}
	s.tools[t.Name] = toolHandler{tool: t, handler: h}
}

// Handle dispatches one JSON-RPC frame. Returns nil for notifications
// (no id), otherwise a Response. Errors are reported as JSON-RPC
// error objects, never raw Go errors — the caller doesn't need to
// translate.
func (s *Server) Handle(ctx context.Context, raw []byte) *Response {
	var req Request
	if err := json.Unmarshal(raw, &req); err != nil {
		return errorResponse(nil, ErrParseError, "parse error: "+err.Error())
	}
	if req.JSONRPC != JSONRPCVersion {
		return errorResponse(req.ID, ErrInvalidRequest, "jsonrpc must be \"2.0\"")
	}
	switch req.Method {
	case "initialize":
		return s.handleInitialize(req)
	case "initialized", "notifications/initialized":
		// JSON-RPC notification, no response.
		s.mu.Lock()
		s.initialised = true
		s.mu.Unlock()
		return nil
	case "tools/list":
		return s.handleToolsList(req)
	case "tools/call":
		return s.handleToolsCall(ctx, req)
	case "ping":
		return successResponse(req.ID, struct{}{})
	default:
		return errorResponse(req.ID, ErrMethodNotFound, "method not found: "+req.Method)
	}
}

// ServeStdio reads JSON-RPC frames from r (one JSON object per line)
// and writes responses to w. Returns when r reaches EOF or ctx is
// cancelled. Frames are processed serially — tools that need to run
// concurrently can dispatch their own goroutines internally.
//
// This is the loop Claude Desktop drives: it spawns gocracker-mcp as
// a subprocess, pipes JSON-RPC frames in stdin, reads responses from
// stdout. Stderr is the server's log surface.
func (s *Server) ServeStdio(ctx context.Context, r io.Reader, w io.Writer) error {
	dec := json.NewDecoder(r)
	enc := json.NewEncoder(w)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("decode: %w", err)
		}
		resp := s.Handle(ctx, raw)
		if resp == nil {
			continue
		}
		if err := enc.Encode(resp); err != nil {
			return fmt.Errorf("encode: %w", err)
		}
	}
}

func (s *Server) handleInitialize(req Request) *Response {
	var params InitializeParams
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return errorResponse(req.ID, ErrInvalidParams, err.Error())
		}
	}
	// We don't fail incompat versions hard — the client can decide.
	// Logging the actual version is enough operational info.
	fmt.Fprintf(stderrLog(), "[mcp] initialize from %s/%s (proto=%s)\n",
		params.ClientInfo.Name, params.ClientInfo.Version, params.ProtocolVersion)
	return successResponse(req.ID, InitializeResult{
		ProtocolVersion: ProtocolVersion,
		Capabilities: ServerCapabilities{
			Tools: &ToolsCapability{},
		},
		ServerInfo: s.Info,
	})
}

func (s *Server) handleToolsList(req Request) *Response {
	out := make([]Tool, 0, len(s.tools))
	for _, t := range s.tools {
		out = append(out, t.tool)
	}
	// Sort for stable output (tests rely on this).
	sortToolsByName(out)
	return successResponse(req.ID, ListToolsResult{Tools: out})
}

func (s *Server) handleToolsCall(ctx context.Context, req Request) *Response {
	var params CallToolParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return errorResponse(req.ID, ErrInvalidParams, err.Error())
	}
	h, ok := s.tools[params.Name]
	if !ok {
		return errorResponse(req.ID, ErrMethodNotFound, "tool not found: "+params.Name)
	}
	res, err := h.handler(ctx, params.Arguments)
	if err != nil {
		// Tools may surface errors either as a JSON-RPC error or as a
		// successful result with isError=true. Convention: if the
		// tool returned a CallToolResult, it owns the error message
		// formatting; if it returned a raw error, we wrap it.
		return errorResponse(req.ID, ErrInternalError, err.Error())
	}
	return successResponse(req.ID, res)
}

func errorResponse(id json.RawMessage, code int, msg string) *Response {
	return &Response{
		JSONRPC: JSONRPCVersion,
		ID:      id,
		Error: &Error{
			Code:    code,
			Message: msg,
		},
	}
}

func successResponse(id json.RawMessage, result any) *Response {
	data, _ := json.Marshal(result)
	return &Response{
		JSONRPC: JSONRPCVersion,
		ID:      id,
		Result:  data,
	}
}
