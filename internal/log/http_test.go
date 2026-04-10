package log

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestParseHTTPAccessLogMode(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		if got := parseHTTPAccessLogMode(""); got != httpAccessLogMutations {
			t.Fatalf("parseHTTPAccessLogMode() = %v, want %v", got, httpAccessLogMutations)
		}
	})

	t.Run("all", func(t *testing.T) {
		if got := parseHTTPAccessLogMode("all"); got != httpAccessLogAll {
			t.Fatalf("parseHTTPAccessLogMode(all) = %v, want %v", got, httpAccessLogAll)
		}
	})

	t.Run("off", func(t *testing.T) {
		if got := parseHTTPAccessLogMode("off"); got != httpAccessLogOff {
			t.Fatalf("parseHTTPAccessLogMode(off) = %v, want %v", got, httpAccessLogOff)
		}
	})
}

func TestShouldLogHTTPAccess(t *testing.T) {
	tests := []struct {
		name   string
		mode   httpAccessLogMode
		method string
		status int
		want   bool
	}{
		{name: "default suppresses successful get", mode: httpAccessLogMutations, method: http.MethodGet, status: http.StatusOK, want: false},
		{name: "default keeps successful put", mode: httpAccessLogMutations, method: http.MethodPut, status: http.StatusNoContent, want: true},
		{name: "default keeps failed get", mode: httpAccessLogMutations, method: http.MethodGet, status: http.StatusBadRequest, want: true},
		{name: "all keeps successful get", mode: httpAccessLogAll, method: http.MethodGet, status: http.StatusOK, want: true},
		{name: "off suppresses write too", mode: httpAccessLogOff, method: http.MethodPost, status: http.StatusCreated, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldLogHTTPAccess(tc.mode, tc.method, tc.status); got != tc.want {
				t.Fatalf("shouldLogHTTPAccess(%v, %q, %d) = %v, want %v", tc.mode, tc.method, tc.status, got, tc.want)
			}
		})
	}
}

func TestParseHTTPAccessLogModeAliases(t *testing.T) {
	allAliases := []string{"all", "on", "true", "1", "debug", "full", " ALL ", " True "}
	for _, s := range allAliases {
		if got := parseHTTPAccessLogMode(s); got != httpAccessLogAll {
			t.Errorf("parseHTTPAccessLogMode(%q) = %v, want httpAccessLogAll", s, got)
		}
	}

	offAliases := []string{"off", "false", "0", "none", " OFF ", " None "}
	for _, s := range offAliases {
		if got := parseHTTPAccessLogMode(s); got != httpAccessLogOff {
			t.Errorf("parseHTTPAccessLogMode(%q) = %v, want httpAccessLogOff", s, got)
		}
	}

	defaultCases := []string{"", "mutations", "whatever", "  "}
	for _, s := range defaultCases {
		if got := parseHTTPAccessLogMode(s); got != httpAccessLogMutations {
			t.Errorf("parseHTTPAccessLogMode(%q) = %v, want httpAccessLogMutations", s, got)
		}
	}
}

func TestShouldLogHTTPAccessExtended(t *testing.T) {
	tests := []struct {
		name   string
		mode   httpAccessLogMode
		method string
		status int
		want   bool
	}{
		{"mutations HEAD success suppressed", httpAccessLogMutations, http.MethodHead, http.StatusOK, false},
		{"mutations DELETE success logged", httpAccessLogMutations, http.MethodDelete, http.StatusOK, true},
		{"mutations PATCH success logged", httpAccessLogMutations, http.MethodPatch, http.StatusOK, true},
		{"mutations GET 500 logged", httpAccessLogMutations, http.MethodGet, http.StatusInternalServerError, true},
		{"mutations GET 404 logged", httpAccessLogMutations, http.MethodGet, http.StatusNotFound, true},
		{"mutations POST 201 logged", httpAccessLogMutations, http.MethodPost, http.StatusCreated, true},
		{"all POST 500 logged", httpAccessLogAll, http.MethodPost, http.StatusInternalServerError, true},
		{"all HEAD 200 logged", httpAccessLogAll, http.MethodHead, http.StatusOK, true},
		{"off GET 500 suppressed", httpAccessLogOff, http.MethodGet, http.StatusInternalServerError, false},
		{"off DELETE 200 suppressed", httpAccessLogOff, http.MethodDelete, http.StatusOK, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldLogHTTPAccess(tc.mode, tc.method, tc.status); got != tc.want {
				t.Fatalf("shouldLogHTTPAccess(%v, %q, %d) = %v, want %v", tc.mode, tc.method, tc.status, got, tc.want)
			}
		})
	}
}

func TestCliHandlerEnabled(t *testing.T) {
	h := &cliHandler{w: &bytes.Buffer{}, level: slog.LevelInfo}

	if !h.Enabled(context.Background(), slog.LevelInfo) {
		t.Error("INFO should be enabled when level=INFO")
	}
	if !h.Enabled(context.Background(), slog.LevelWarn) {
		t.Error("WARN should be enabled when level=INFO")
	}
	if !h.Enabled(context.Background(), slog.LevelError) {
		t.Error("ERROR should be enabled when level=INFO")
	}
	if h.Enabled(context.Background(), slog.LevelDebug) {
		t.Error("DEBUG should not be enabled when level=INFO")
	}
}

func TestCliHandlerHandle(t *testing.T) {
	var buf bytes.Buffer
	h := &cliHandler{w: &buf, level: slog.LevelDebug}

	r := slog.NewRecord(time.Date(2026, 4, 9, 12, 0, 0, 0, time.UTC), slog.LevelInfo, "hello world", 0)
	if err := h.Handle(context.Background(), r); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "hello world") {
		t.Errorf("output missing message: %q", output)
	}
	if !strings.Contains(output, "INFO") {
		t.Errorf("output missing level tag: %q", output)
	}
}

func TestCliHandlerLevels(t *testing.T) {
	tests := []struct {
		level   slog.Level
		wantTag string
	}{
		{slog.LevelDebug, "DEBUG"},
		{slog.LevelInfo, "INFO"},
		{slog.LevelWarn, "WARN"},
		{slog.LevelError, "ERROR"},
	}
	for _, tc := range tests {
		t.Run(tc.wantTag, func(t *testing.T) {
			var buf bytes.Buffer
			h := &cliHandler{w: &buf, level: slog.LevelDebug}
			r := slog.NewRecord(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), tc.level, "test", 0)
			if err := h.Handle(context.Background(), r); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if !strings.Contains(buf.String(), tc.wantTag) {
				t.Errorf("output missing %q: %q", tc.wantTag, buf.String())
			}
		})
	}
}

func TestCliHandlerWithComponent(t *testing.T) {
	var buf bytes.Buffer
	h := &cliHandler{w: &buf, level: slog.LevelDebug}
	h2 := h.WithAttrs([]slog.Attr{slog.String("component", "vmm")})

	r := slog.NewRecord(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), slog.LevelInfo, "booting", 0)
	if err := h2.Handle(context.Background(), r); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	output := buf.String()
	if !strings.Contains(output, "vmm") {
		t.Errorf("output missing component: %q", output)
	}
	if !strings.Contains(output, "booting") {
		t.Errorf("output missing message: %q", output)
	}
}

func TestCliHandlerWithRecordAttrs(t *testing.T) {
	var buf bytes.Buffer
	h := &cliHandler{w: &buf, level: slog.LevelDebug}

	r := slog.NewRecord(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), slog.LevelInfo, "event", 0)
	r.AddAttrs(slog.String("id", "gc-1234"), slog.Int("port", 8080))
	if err := h.Handle(context.Background(), r); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	output := buf.String()
	if !strings.Contains(output, "id=gc-1234") {
		t.Errorf("output missing id attr: %q", output)
	}
	if !strings.Contains(output, "port=8080") {
		t.Errorf("output missing port attr: %q", output)
	}
}

func TestCliHandlerWithGroup(t *testing.T) {
	var buf bytes.Buffer
	h := &cliHandler{w: &buf, level: slog.LevelDebug}
	h2 := h.WithGroup("mygroup")

	// WithGroup returns a handler; verify it implements slog.Handler
	var _ slog.Handler = h2

	r := slog.NewRecord(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), slog.LevelInfo, "grouped", 0)
	if err := h2.Handle(context.Background(), r); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !strings.Contains(buf.String(), "grouped") {
		t.Errorf("output missing message: %q", buf.String())
	}
}

func TestCliHandlerWithAttrsPreservesExisting(t *testing.T) {
	var buf bytes.Buffer
	h := &cliHandler{w: &buf, level: slog.LevelDebug}

	h2 := h.WithAttrs([]slog.Attr{slog.String("a", "1")})
	h3 := h2.WithAttrs([]slog.Attr{slog.String("b", "2")})

	r := slog.NewRecord(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), slog.LevelInfo, "msg", 0)
	if err := h3.Handle(context.Background(), r); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	output := buf.String()
	if !strings.Contains(output, "a=1") {
		t.Errorf("missing first attr: %q", output)
	}
	if !strings.Contains(output, "b=2") {
		t.Errorf("missing second attr: %q", output)
	}
}

func TestCliHandlerFiltersBelowLevel(t *testing.T) {
	var buf bytes.Buffer
	h := &cliHandler{w: &buf, level: slog.LevelWarn}

	r := slog.NewRecord(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), slog.LevelInfo, "should not appear", 0)
	if h.Enabled(context.Background(), r.Level) {
		t.Error("INFO should not be enabled at WARN level")
	}
}

func TestWithComponent(t *testing.T) {
	logger := WithComponent("test-comp")
	if logger == nil {
		t.Fatal("WithComponent returned nil")
	}
}

func TestInitJSON(t *testing.T) {
	// Just verify Init doesn't panic in JSON mode
	Init(true)
	if VMM == nil || API == nil || Container == nil {
		t.Error("component loggers should not be nil after Init")
	}
	// Restore CLI mode
	Init(false)
}

func TestInitCLI(t *testing.T) {
	Init(false)
	if VMM == nil || API == nil || Container == nil || Compose == nil || Loader == nil || KVM == nil || Virtio == nil {
		t.Error("all component loggers should be non-nil after Init")
	}
}

func TestLogHTTPAccess(t *testing.T) {
	// logHTTPAccess should not panic for different status ranges
	logger := WithComponent("test")
	logHTTPAccess(logger, "GET", "/vms", 200, 42, 5*time.Millisecond)
	logHTTPAccess(logger, "POST", "/vms", 400, 0, 1*time.Millisecond)
	logHTTPAccess(logger, "DELETE", "/vms/1", 500, 0, 2*time.Millisecond)
}
