package log

import (
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5/middleware"
)

const httpAccessLogEnv = "GOCRACKER_HTTP_ACCESS_LOG"

type httpAccessLogMode int

const (
	httpAccessLogMutations httpAccessLogMode = iota
	httpAccessLogAll
	httpAccessLogOff
)

// AccessLogMiddleware logs HTTP requests with a quieter default policy.
// By default, successful GET/HEAD requests are suppressed while writes and
// all failures remain visible. Set GOCRACKER_HTTP_ACCESS_LOG=all to restore
// full access logs or GOCRACKER_HTTP_ACCESS_LOG=off to disable them entirely.
func AccessLogMiddleware(component string) func(http.Handler) http.Handler {
	logger := WithComponent(component)
	mode := parseHTTPAccessLogMode(os.Getenv(httpAccessLogEnv))
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			start := time.Now()
			next.ServeHTTP(ww, r)
			status := ww.Status()
			if !shouldLogHTTPAccess(mode, r.Method, status) {
				return
			}
			logHTTPAccess(logger, r.Method, r.URL.RequestURI(), status, ww.BytesWritten(), time.Since(start))
		})
	}
}

func parseHTTPAccessLogMode(raw string) httpAccessLogMode {
	switch strings.TrimSpace(strings.ToLower(raw)) {
	case "all", "on", "true", "1", "debug", "full":
		return httpAccessLogAll
	case "off", "false", "0", "none":
		return httpAccessLogOff
	default:
		return httpAccessLogMutations
	}
}

func shouldLogHTTPAccess(mode httpAccessLogMode, method string, status int) bool {
	switch mode {
	case httpAccessLogAll:
		return true
	case httpAccessLogOff:
		return false
	default:
		if status >= http.StatusBadRequest {
			return true
		}
		switch method {
		case http.MethodGet, http.MethodHead:
			return false
		default:
			return true
		}
	}
}

func logHTTPAccess(logger *slog.Logger, method, uri string, status, bytes int, latency time.Duration) {
	msg := "http"
	switch {
	case status >= http.StatusInternalServerError:
		logger.Error(msg, "method", method, "uri", uri, "status", status, "bytes", bytes, "latency", latency.Round(time.Microsecond))
	case status >= http.StatusBadRequest:
		logger.Warn(msg, "method", method, "uri", uri, "status", status, "bytes", bytes, "latency", latency.Round(time.Microsecond))
	default:
		logger.Info(msg, "method", method, "uri", uri, "status", status, "bytes", bytes, "latency", latency.Round(time.Microsecond))
	}
}
