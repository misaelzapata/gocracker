package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gosdk "github.com/gocracker/gocracker/sandboxes/sdk/go"
)

// fakeSandboxd returns a minimal HTTP server that fakes the sandboxd
// surface the MCP layer needs. Only the routes we exercise are
// implemented; unknown paths return 404 so a missing route fails the
// test loudly.
func fakeSandboxd(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/sandboxes/lease", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sandbox": map[string]any{
				"id":         "sb-test-1",
				"state":      "running",
				"image":      "test-image",
				"uds_path":   "/tmp/fake.sock",
				"guest_ip":   "10.0.0.5",
				"created_at": time.Now().UTC(),
			},
		})
	})
	mux.HandleFunc("/sandboxes/sb-test-1", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"sandbox": map[string]any{
					"id":         "sb-test-1",
					"state":      "running",
					"image":      "test-image",
					"uds_path":   "/tmp/fake.sock",
					"created_at": time.Now().UTC(),
				},
			})
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "method", http.StatusMethodNotAllowed)
		}
	})
	return httptest.NewServer(mux)
}

// newServerWithFake returns a Server whose Sandboxd points at a
// freshly-spawned httptest. Test cleanup tears down the server.
func newServerWithFake(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	srv := fakeSandboxd(t)
	t.Cleanup(srv.Close)
	cli := gosdk.NewClient(srv.URL)
	return NewServer(cli, ServerInfo{Name: "test", Version: "test"}), srv
}

func TestInitializeReturnsServerInfo(t *testing.T) {
	s, _ := newServerWithFake(t)
	resp := s.Handle(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","clientInfo":{"name":"test","version":"1"}}}`))
	if resp == nil {
		t.Fatal("nil response")
	}
	if resp.Error != nil {
		t.Fatalf("error: %+v", resp.Error)
	}
	var got InitializeResult
	if err := json.Unmarshal(resp.Result, &got); err != nil {
		t.Fatal(err)
	}
	if got.ProtocolVersion != ProtocolVersion {
		t.Errorf("protocol version = %q, want %q", got.ProtocolVersion, ProtocolVersion)
	}
	if got.ServerInfo.Name != "test" {
		t.Errorf("server info name = %q, want test", got.ServerInfo.Name)
	}
	if got.Capabilities.Tools == nil {
		t.Error("tools capability missing")
	}
}

func TestInitializedNotificationReturnsNil(t *testing.T) {
	s, _ := newServerWithFake(t)
	// A notification — no `id` field.
	resp := s.Handle(context.Background(), []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	if resp != nil {
		t.Fatalf("expected nil response for notification, got %+v", resp)
	}
}

func TestToolsListReturnsAllTools(t *testing.T) {
	s, _ := newServerWithFake(t)
	resp := s.Handle(context.Background(), []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`))
	if resp.Error != nil {
		t.Fatal(resp.Error)
	}
	var got ListToolsResult
	if err := json.Unmarshal(resp.Result, &got); err != nil {
		t.Fatal(err)
	}
	want := []string{"process.eval_node", "process.exec", "sandbox.delete", "sandbox.lease", "sandbox.recycle"}
	if len(got.Tools) != len(want) {
		t.Fatalf("got %d tools, want %d", len(got.Tools), len(want))
	}
	for i, n := range want {
		if got.Tools[i].Name != n {
			t.Errorf("tools[%d] = %q, want %q", i, got.Tools[i].Name, n)
		}
		if got.Tools[i].InputSchema == nil {
			t.Errorf("tools[%d] (%s) has nil inputSchema", i, n)
		}
	}
}

func TestToolsCallSandboxLease(t *testing.T) {
	s, _ := newServerWithFake(t)
	resp := s.Handle(context.Background(), []byte(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"sandbox.lease","arguments":{"template_id":"base-node"}}}`))
	if resp.Error != nil {
		t.Fatal(resp.Error)
	}
	var got CallToolResult
	if err := json.Unmarshal(resp.Result, &got); err != nil {
		t.Fatal(err)
	}
	if got.IsError {
		t.Fatalf("tool reported error: %+v", got.Content)
	}
	if len(got.Content) != 1 || got.Content[0].Type != "text" {
		t.Fatalf("unexpected content: %+v", got.Content)
	}
	var parsed sandboxLeaseResult
	if err := json.Unmarshal([]byte(got.Content[0].Text), &parsed); err != nil {
		t.Fatalf("inner JSON: %v", err)
	}
	if parsed.ID != "sb-test-1" {
		t.Errorf("sandbox id = %q, want sb-test-1", parsed.ID)
	}
}

func TestToolsCallMissingArgs(t *testing.T) {
	s, _ := newServerWithFake(t)
	resp := s.Handle(context.Background(), []byte(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"sandbox.lease","arguments":{}}}`))
	if resp.Error != nil {
		t.Fatal(resp.Error)
	}
	var got CallToolResult
	if err := json.Unmarshal(resp.Result, &got); err != nil {
		t.Fatal(err)
	}
	if !got.IsError {
		t.Fatalf("expected isError=true for missing template_id, got %+v", got)
	}
	if !strings.Contains(got.Content[0].Text, "template_id") {
		t.Errorf("error text missing field name: %q", got.Content[0].Text)
	}
}

func TestToolsCallUnknown(t *testing.T) {
	s, _ := newServerWithFake(t)
	resp := s.Handle(context.Background(), []byte(`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"nonexistent","arguments":{}}}`))
	if resp.Error == nil {
		t.Fatal("expected error for unknown tool")
	}
	if resp.Error.Code != ErrMethodNotFound {
		t.Errorf("error code = %d, want %d", resp.Error.Code, ErrMethodNotFound)
	}
}

func TestUnknownMethod(t *testing.T) {
	s, _ := newServerWithFake(t)
	resp := s.Handle(context.Background(), []byte(`{"jsonrpc":"2.0","id":6,"method":"nonsense"}`))
	if resp.Error == nil || resp.Error.Code != ErrMethodNotFound {
		t.Fatalf("got %+v, want method-not-found error", resp.Error)
	}
}

func TestInvalidJSONRPCVersion(t *testing.T) {
	s, _ := newServerWithFake(t)
	resp := s.Handle(context.Background(), []byte(`{"jsonrpc":"1.0","id":7,"method":"ping"}`))
	if resp.Error == nil || resp.Error.Code != ErrInvalidRequest {
		t.Fatalf("got %+v, want invalid-request error", resp.Error)
	}
}

func TestServeStdioRoundTrip(t *testing.T) {
	s, _ := newServerWithFake(t)
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}` + "\n")
	var out bytes.Buffer
	if err := s.ServeStdio(context.Background(), in, &out); err != nil {
		t.Fatal(err)
	}
	// We expect two response objects in `out`.
	dec := json.NewDecoder(&out)
	var r1, r2 Response
	if err := dec.Decode(&r1); err != nil {
		t.Fatalf("decode r1: %v", err)
	}
	if err := dec.Decode(&r2); err != nil {
		t.Fatalf("decode r2: %v", err)
	}
	if r1.Error != nil || r2.Error != nil {
		t.Fatalf("errors: r1=%+v r2=%+v", r1.Error, r2.Error)
	}
}

func TestParseError(t *testing.T) {
	s, _ := newServerWithFake(t)
	resp := s.Handle(context.Background(), []byte(`not json`))
	if resp.Error == nil || resp.Error.Code != ErrParseError {
		t.Fatalf("got %+v, want parse error", resp.Error)
	}
}

// TestServeStdioRecoversFromBadJSON regression-tests that a malformed
// JSON line emits a parse-error response and the loop keeps reading
// subsequent lines instead of fatally exiting. Earlier versions used
// a json.Decoder (stateful) which corrupted the stream after the
// first decode error — the scanner-based loop fixes this.
func TestServeStdioRecoversFromBadJSON(t *testing.T) {
	s, _ := newServerWithFake(t)
	// Three lines: a bad-JSON line, a ping, an EOF-equivalent.
	in := strings.NewReader("garbage line\n" +
		`{"jsonrpc":"2.0","id":99,"method":"ping"}` + "\n")
	var out bytes.Buffer
	if err := s.ServeStdio(context.Background(), in, &out); err != nil {
		t.Fatalf("ServeStdio returned error: %v", err)
	}
	dec := json.NewDecoder(&out)
	var r1, r2 Response
	if err := dec.Decode(&r1); err != nil {
		t.Fatalf("decode r1 (parse error): %v", err)
	}
	if r1.Error == nil || r1.Error.Code != ErrParseError {
		t.Errorf("r1: got %+v, want parse error", r1.Error)
	}
	if err := dec.Decode(&r2); err != nil {
		t.Fatalf("decode r2 (ping after bad json): %v", err)
	}
	if r2.Error != nil {
		t.Errorf("r2 (ping): got error %+v, want success", r2.Error)
	}
}
