package log

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAccessLogMiddlewareMutations(t *testing.T) {
	handler := AccessLogMiddleware("test")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// GET should be suppressed in default (mutations) mode
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/vms", nil)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	// POST should be logged
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/vms", nil)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestAccessLogMiddlewareError(t *testing.T) {
	handler := AccessLogMiddleware("test")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/vms", nil)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestLogHTTPAccessLevels(t *testing.T) {
	var buf bytes.Buffer
	h := &cliHandler{w: &buf, level: slog.LevelDebug}
	slog.SetDefault(slog.New(h))
	logger := WithComponent("test-http")

	// 200 - Info
	logHTTPAccess(logger, "GET", "/ok", 200, 42, time.Millisecond)
	// 400 - Warn
	logHTTPAccess(logger, "POST", "/err", 400, 0, time.Millisecond)
	// 500 - Error
	logHTTPAccess(logger, "DELETE", "/fail", 500, 0, time.Millisecond)

	output := buf.String()
	if !strings.Contains(output, "INFO") {
		t.Error("missing INFO for 200")
	}
	if !strings.Contains(output, "WARN") {
		t.Error("missing WARN for 400")
	}
	if !strings.Contains(output, "ERROR") {
		t.Error("missing ERROR for 500")
	}

	// Restore
	Init(false)
}

func TestCliHandlerNoComponent(t *testing.T) {
	var buf bytes.Buffer
	h := &cliHandler{w: &buf, level: slog.LevelDebug}
	r := slog.NewRecord(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), slog.LevelInfo, "test msg", 0)
	h.Handle(context.Background(), r)
	output := buf.String()
	if !strings.Contains(output, "[INFO]") {
		t.Errorf("expected [INFO] without component, got %q", output)
	}
}

func TestCliHandlerWithComponentFormat(t *testing.T) {
	var buf bytes.Buffer
	h := &cliHandler{w: &buf, level: slog.LevelDebug}
	h2 := h.WithAttrs([]slog.Attr{slog.String("component", "api")})
	r := slog.NewRecord(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), slog.LevelWarn, "warning", 0)
	h2.Handle(context.Background(), r)
	output := buf.String()
	if !strings.Contains(output, "[api:WARN]") {
		t.Errorf("expected [api:WARN], got %q", output)
	}
}

func TestCliHandlerNonComponentAttrs(t *testing.T) {
	var buf bytes.Buffer
	h := &cliHandler{w: &buf, level: slog.LevelDebug}
	h2 := h.WithAttrs([]slog.Attr{
		slog.String("component", "vmm"),
		slog.String("extra", "value"),
	})
	r := slog.NewRecord(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), slog.LevelInfo, "msg", 0)
	h2.Handle(context.Background(), r)
	output := buf.String()
	if !strings.Contains(output, "extra=value") {
		t.Errorf("expected extra=value in output, got %q", output)
	}
}

func TestCliHandlerTimestamp(t *testing.T) {
	var buf bytes.Buffer
	h := &cliHandler{w: &buf, level: slog.LevelDebug}
	ts := time.Date(2026, 4, 10, 15, 30, 45, 123000000, time.UTC)
	r := slog.NewRecord(ts, slog.LevelInfo, "hello", 0)
	h.Handle(context.Background(), r)
	output := buf.String()
	if !strings.Contains(output, "2026-04-10 15:30:45.123") {
		t.Errorf("expected timestamp in output, got %q", output)
	}
}

func TestShouldLogHTTPAccessDefaultHeadSuppressed(t *testing.T) {
	if shouldLogHTTPAccess(httpAccessLogMutations, http.MethodHead, 200) {
		t.Fatal("HEAD 200 should be suppressed in mutations mode")
	}
}

func TestAccessLogMiddlewareWith400(t *testing.T) {
	handler := AccessLogMiddleware("test")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	handler.ServeHTTP(rec, req)
}
