package main

import (
	"bytes"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewMuxServesGreeting(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	newMux().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if body := strings.TrimSpace(rec.Body.String()); body != "Hello from gocracker!" {
		t.Fatalf("body = %q", body)
	}
}

func TestRunWritesBannerOnImmediateError(t *testing.T) {
	var stdout bytes.Buffer
	err := run("bad addr", &stdout)
	if err == nil {
		t.Fatal("run() error = nil, want error")
	}
	if !strings.Contains(stdout.String(), "Listening on bad addr") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if _, ok := err.(*net.OpError); !ok && !strings.Contains(err.Error(), "missing port") {
		t.Fatalf("err = %v", err)
	}
}
