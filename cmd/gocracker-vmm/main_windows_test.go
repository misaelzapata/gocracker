//go:build windows

package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestResolveListenerAddr covers the (-api-addr, -socket) flag matrix.
// The mappings here are load-bearing for the shim's interop with the
// Linux build's address conventions; any change must be conscious.
func TestResolveListenerAddr(t *testing.T) {
	cases := []struct {
		name      string
		apiAddr   string
		socket    string
		wantNet   string
		wantAddr  string
		wantError bool
	}{
		{"default-socket", "", `C:\tmp\vmm.sock`, "unix", `C:\tmp\vmm.sock`, false},
		{"unix-prefix-on-socket", "", "unix:///tmp/vmm.sock", "unix", "/tmp/vmm.sock", false},
		{"api-addr-unix", "unix:///tmp/api.sock", `C:\ignored`, "unix", "/tmp/api.sock", false},
		{"api-addr-tcp", "tcp://127.0.0.1:8080", `C:\ignored`, "tcp", "127.0.0.1:8080", false},
		{"api-addr-bad-scheme", "http://nope", `C:\ignored`, "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotNet, gotAddr, err := resolveListenerAddr(tc.apiAddr, tc.socket)
			if tc.wantError {
				if err == nil {
					t.Fatalf("want error, got nil (net=%q addr=%q)", gotNet, gotAddr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if gotNet != tc.wantNet || gotAddr != tc.wantAddr {
				t.Fatalf("got (%q,%q), want (%q,%q)", gotNet, gotAddr, tc.wantNet, tc.wantAddr)
			}
		})
	}
}

// TestWorkerStatusInitialState pins the initial GET / response. If we
// ever change "Uninitialized" we want to break callers explicitly —
// Firecracker-compatible clients pattern-match this string.
func TestWorkerStatusInitialState(t *testing.T) {
	w := newWorker("vm-test", nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got["id"] != "vm-test" {
		t.Fatalf("id = %v, want vm-test", got["id"])
	}
	if got["state"] != "Uninitialized" {
		t.Fatalf("state = %v, want Uninitialized", got["state"])
	}
}

// TestWorkerMachineConfigValidation covers the request validators
// without spinning up an actual VM (which would require WHP).
func TestWorkerMachineConfigValidation(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantCode int
	}{
		{"valid", `{"vcpu_count":1,"mem_size_mib":128}`, http.StatusNoContent},
		{"zero-vcpu", `{"vcpu_count":0,"mem_size_mib":128}`, http.StatusBadRequest},
		{"multi-vcpu", `{"vcpu_count":2,"mem_size_mib":128}`, http.StatusBadRequest},
		{"too-little-mem", `{"vcpu_count":1,"mem_size_mib":32}`, http.StatusBadRequest},
		{"bad-json", `{not json`, http.StatusBadRequest},
		{"unknown-field", `{"vcpu_count":1,"mem_size_mib":128,"frob":true}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wk := newWorker("", nil)
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPut, "/machine-config", strings.NewReader(tc.body))
			wk.ServeHTTP(rec, req)
			if rec.Code != tc.wantCode {
				t.Fatalf("code = %d, want %d (body=%s)", rec.Code, tc.wantCode, rec.Body.String())
			}
		})
	}
}

// TestWorkerUnknownRoute confirms unknown paths/methods 404 rather
// than triggering one of the JSON handlers.
func TestWorkerUnknownRoute(t *testing.T) {
	wk := newWorker("", nil)
	cases := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/machine-config"}, // wrong verb
		{http.MethodPut, "/snapshot/save"},   // unsupported path
		{http.MethodGet, "/random"},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(tc.method, tc.path, nil)
		wk.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s %s: code = %d, want 404", tc.method, tc.path, rec.Code)
		}
	}
}

// TestWorkerActionRejectsUnknownAction confirms an unrecognised
// action_type fails with a 400 fault_message rather than panicking
// or silently no-oping.
func TestWorkerActionRejectsUnknownAction(t *testing.T) {
	wk := newWorker("", nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/actions", bytes.NewReader([]byte(`{"action_type":"FlushMetrics"}`)))
	wk.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("FlushMetrics")) {
		t.Fatalf("body should mention the rejected action: %s", rec.Body.String())
	}
}

// TestWorkerStartVMRequiresBootSource catches the easy mis-step of
// firing InstanceStart before /boot-source — the same error the
// Linux build returns.
func TestWorkerStartVMRequiresBootSource(t *testing.T) {
	wk := newWorker("", nil)
	if err := wk.startVM(); err == nil {
		t.Fatalf("startVM() with no boot source = nil, want error")
	}
}
