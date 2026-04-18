// Package log provides structured logging for gocracker using slog.
// CLI mode uses Firecracker-style colored output; daemon mode uses JSON.
package log

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"
)

// ANSI color codes
const (
	colorReset  = "\033[0m"
	colorDim    = "\033[2m"
	colorRed    = "\033[31m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	colorCyan   = "\033[36m"
)

// Component loggers — use these throughout the codebase.
var (
	VMM       *slog.Logger
	API       *slog.Logger
	Container *slog.Logger
	Compose   *slog.Logger
	Loader    *slog.Logger
	KVM       *slog.Logger
	Virtio    *slog.Logger
)

func init() {
	Init(false)
}

// Init configures the global slog default and all component loggers.
// If json is true, output is JSON (for API/daemon mode);
// otherwise Firecracker-style colored text.
//
// Default level is INFO. Set GOCRACKER_LOG=debug to enable debug output.
func Init(json bool) {
	level := slog.LevelInfo
	if os.Getenv("GOCRACKER_LOG") == "debug" {
		level = slog.LevelDebug
	}
	var handler slog.Handler
	if json {
		handler = slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	} else {
		handler = &cliHandler{w: os.Stderr, level: level}
	}
	slog.SetDefault(slog.New(handler))

	VMM = WithComponent("vmm")
	API = WithComponent("api")
	Container = WithComponent("container")
	Compose = WithComponent("compose")
	Loader = WithComponent("loader")
	KVM = WithComponent("kvm")
	Virtio = WithComponent("virtio")
}

// WithComponent returns a logger with a "component" attribute.
func WithComponent(name string) *slog.Logger {
	return slog.Default().With("component", name)
}

// cliHandler produces Firecracker-style colored output:
//   2026-04-01T16:08:17.898 [container:INFO] booting id=gc-8647
type cliHandler struct {
	w     io.Writer
	mu    sync.Mutex
	level slog.Level
	attrs []slog.Attr
	group string
}

func (h *cliHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level
}

func (h *cliHandler) Handle(_ context.Context, r slog.Record) error {
	ts := r.Time.Format(time.DateTime) + "." + fmt.Sprintf("%03d", r.Time.Nanosecond()/1e6)

	// Level tag with color
	var levelColor, levelTag string
	switch {
	case r.Level >= slog.LevelError:
		levelColor, levelTag = colorRed, "ERROR"
	case r.Level >= slog.LevelWarn:
		levelColor, levelTag = colorYellow, "WARN"
	case r.Level >= slog.LevelInfo:
		levelColor, levelTag = colorGreen, "INFO"
	default:
		levelColor, levelTag = colorDim, "DEBUG"
	}

	// Extract component from pre-set attrs
	component := ""
	for _, a := range h.attrs {
		if a.Key == "component" {
			component = a.Value.String()
			break
		}
	}

	// Build key=value pairs for extra attrs
	extra := ""
	// Pre-set attrs (skip component — it's in the bracket)
	for _, a := range h.attrs {
		if a.Key == "component" {
			continue
		}
		extra += " " + a.Key + "=" + a.Value.String()
	}
	// Record attrs
	r.Attrs(func(a slog.Attr) bool {
		extra += " " + a.Key + "=" + a.Value.String()
		return true
	})

	var line string
	if component != "" {
		line = fmt.Sprintf("%s%s%s %s[%s:%s]%s %s%s\n",
			colorDim, ts, colorReset,
			levelColor, component, levelTag, colorReset,
			r.Message, extra)
	} else {
		line = fmt.Sprintf("%s%s%s %s[%s]%s %s%s\n",
			colorDim, ts, colorReset,
			levelColor, levelTag, colorReset,
			r.Message, extra)
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := h.w.Write([]byte(line))
	return err
}

func (h *cliHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &cliHandler{
		w:     h.w,
		level: h.level,
		attrs: append(append([]slog.Attr{}, h.attrs...), attrs...),
		group: h.group,
	}
}

func (h *cliHandler) WithGroup(name string) slog.Handler {
	return &cliHandler{
		w:     h.w,
		level: h.level,
		attrs: h.attrs,
		group: name,
	}
}
