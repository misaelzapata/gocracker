package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/textproto"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	toolboxspec "github.com/gocracker/gocracker/internal/toolbox/spec"
)

// guestSecrets holds secrets pushed from the host. In Fase 2 the map is
// populated by /secrets POST but nothing reads it — the egress proxy
// (which would inject Authorization headers based on this map) lands in
// Fase 4 along with sandboxd's secret push wiring. Keeping the storage
// endpoint avoids a wire-protocol change later.
var (
	guestSecretsMu sync.RWMutex
	guestSecrets   = map[string]SandboxSecret{}
)

// Version is re-exported from toolboxspec so the /healthz response and
// the on-disk VERSION file always agree. Bumping toolboxspec.Version
// invalidates every cached warm snapshot and gates restore parity.
var Version = toolboxspec.Version

type Health struct {
	OK      bool   `json:"ok"`
	Version string `json:"version"`
}

// Handler builds the agent's HTTP mux. Exposed so tests can drive
// handlers via httptest.Server without binding a real vsock listener.
func Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, Health{OK: true, Version: Version})
	})
	mux.HandleFunc("GET /info/version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"version": Version})
	})
	mux.HandleFunc("GET /files", handleListFiles)
	mux.HandleFunc("GET /files/download", handleDownloadFile)
	mux.HandleFunc("POST /files/upload", handleUploadFile)
	mux.HandleFunc("DELETE /files", handleDeleteFile)
	mux.HandleFunc("POST /files/mkdir", handleMkdir)
	mux.HandleFunc("POST /files/rename", handleRename)
	mux.HandleFunc("POST /files/chmod", handleChmod)
	mux.HandleFunc("POST /git/clone", handleGitClone)
	mux.HandleFunc("POST /git/status", handleGitStatus)
	mux.HandleFunc("POST /secrets", handleSetSecret)
	mux.HandleFunc("DELETE /secrets/{name}", handleDeleteSecret)
	mux.HandleFunc("GET /secrets", handleListSecrets)
	mux.Handle("/proxy/http/", http.HandlerFunc(handleHTTPProxy))
	// /exec is the framed-binary data plane (PLAN_SANDBOXD §4). Unlike
	// the other JSON endpoints above, this hijacks the conn after the
	// handshake JSON and switches to the binary frame protocol — see
	// internal/toolbox/agent/exec.go and frame.go.
	mux.HandleFunc("POST /exec", handleExec)
	// /internal/* is host-only — sandboxd-side trusted callers (PLAN
	// §3 SetNetwork after warm restore). Not exposed via any
	// user-facing API surface; callers reach it via the same UDS
	// CONNECT 10023 path as the other endpoints.
	mux.HandleFunc("POST /internal/setnetwork", handleSetNetwork)
	return mux
}

func Serve(ctx context.Context, port uint32) error {
	listener, err := ListenVsock(port)
	if err != nil {
		return err
	}
	defer listener.Close()

	server := &http.Server{Handler: Handler()}
	go func() {
		<-ctx.Done()
		_ = server.Shutdown(context.Background())
	}()
	return server.Serve(listener)
}

func handleListFiles(w http.ResponseWriter, r *http.Request) {
	dir := cleanGuestPath(r.URL.Query().Get("path"))
	if !requirePath(w, dir) {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	out := FileListResponse{Path: dir}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			continue
		}
		kind := "file"
		if entry.IsDir() {
			kind = "dir"
		}
		out.Entries = append(out.Entries, FileEntry{
			Name: entry.Name(),
			Path: filepath.Join(dir, entry.Name()),
			Kind: kind,
			Size: info.Size(),
		})
	}
	sort.Slice(out.Entries, func(i, j int) bool { return out.Entries[i].Name < out.Entries[j].Name })
	writeJSON(w, http.StatusOK, out)
}

func handleDownloadFile(w http.ResponseWriter, r *http.Request) {
	filePath := cleanGuestPath(r.URL.Query().Get("path"))
	if !requirePath(w, filePath) {
		return
	}
	data, err := os.ReadFile(filePath)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func handleUploadFile(w http.ResponseWriter, r *http.Request) {
	filePath := cleanGuestPath(r.URL.Query().Get("path"))
	if !requirePath(w, filePath) {
		return
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	// 0644 — regular file-transfer default. Callers that need an
	// executable bit should follow up with POST /files/chmod (which
	// takes an explicit mode) instead of every upload gaining +x.
	if err := os.WriteFile(filePath, data, 0o644); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": filePath, "size": len(data)})
}

func handleDeleteFile(w http.ResponseWriter, r *http.Request) {
	filePath := cleanGuestPath(r.URL.Query().Get("path"))
	if !requirePath(w, filePath) {
		return
	}
	if err := os.RemoveAll(filePath); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func handleMkdir(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
		All  bool   `json:"all"` // if true, create parent dirs (MkdirAll)
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	dir := cleanGuestPath(req.Path)
	if !requirePath(w, dir) {
		return
	}
	var err error
	if req.All {
		err = os.MkdirAll(dir, 0o755)
	} else {
		err = os.Mkdir(dir, 0o755)
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": dir})
}

func handleRename(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OldPath string `json:"old_path"`
		NewPath string `json:"new_path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	src := cleanGuestPath(req.OldPath)
	dst := cleanGuestPath(req.NewPath)
	if !requirePath(w, src) || !requirePath(w, dst) {
		return
	}
	if err := os.Rename(src, dst); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "old_path": src, "new_path": dst})
}

func handleChmod(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
		Mode uint32 `json:"mode"` // octal, e.g. 0o755 = 493
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	p := cleanGuestPath(req.Path)
	if !requirePath(w, p) {
		return
	}
	if err := os.Chmod(p, os.FileMode(req.Mode)); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": p, "mode": req.Mode})
}

func handleGitClone(w http.ResponseWriter, r *http.Request) {
	var req GitCloneRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	dir := cleanGuestPath(req.Directory)
	if !requirePath(w, dir) {
		return
	}
	cmd := exec.CommandContext(r.Context(), "git", "clone", req.Repository, dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out))))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"output": string(out)})
}

func handleGitStatus(w http.ResponseWriter, r *http.Request) {
	var req GitStatusRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	dir := cleanGuestPath(req.Directory)
	if !requirePath(w, dir) {
		return
	}
	cmd := exec.CommandContext(r.Context(), "git", "-C", dir, "status", "--short", "--branch")
	out, err := cmd.CombinedOutput()
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out))))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"output": string(out)})
}

func handleHTTPProxy(w http.ResponseWriter, r *http.Request) {
	trimmed := strings.TrimPrefix(r.URL.Path, "/proxy/http/")
	parts := strings.SplitN(trimmed, "/", 2)
	if len(parts) == 0 || strings.TrimSpace(parts[0]) == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("proxy port is required"))
		return
	}
	port, err := strconv.Atoi(parts[0])
	if err != nil || port <= 0 || port > 65535 {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid proxy port"))
		return
	}
	targetURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	if len(parts) == 2 && parts[1] != "" {
		targetURL += "/" + parts[1]
	} else {
		targetURL += "/"
	}
	if raw := r.URL.RawQuery; raw != "" {
		targetURL += "?" + raw
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, r.Body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	req.Header = cloneHeaders(r.Header)
	req.Host = fmt.Sprintf("127.0.0.1:%d", port)

	client := &http.Client{
		Transport: &http.Transport{Proxy: nil},
	}
	resp, err := client.Do(req)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	defer resp.Body.Close()
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// cleanGuestPath returns a filepath.Clean'd copy of value, or "" if
// value is empty/whitespace. Callers that require a path MUST check
// for "" and reject the request — defaulting to "." used to be the
// behavior but made `DELETE /files?path=` (omitting path entirely)
// silently run `os.RemoveAll(".")` against whatever cwd the agent
// happened to be in, which for PID-1-spawned agents is `/`.
func cleanGuestPath(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}
	return filepath.Clean(trimmed)
}

// requirePath writes a 400 and returns false if the caller didn't
// supply a path. Centralises the check so every handler that needs
// a non-empty path fails the same way.
func requirePath(w http.ResponseWriter, p string) bool {
	if p == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("path is required"))
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func cloneHeaders(in http.Header) http.Header {
	if len(in) == 0 {
		return http.Header{}
	}
	out := make(http.Header, len(in))
	for key, values := range in {
		out[textproto.CanonicalMIMEHeaderKey(key)] = append([]string(nil), values...)
	}
	return out
}

func copyHeaders(dst, src http.Header) {
	for key, values := range src {
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

// handleSetSecret stores (or replaces) a secret pushed by the host.
func handleSetSecret(w http.ResponseWriter, r *http.Request) {
	var s SandboxSecret
	if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if s.Name == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("name required"))
		return
	}
	guestSecretsMu.Lock()
	guestSecrets[s.Name] = s
	guestSecretsMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]string{"name": s.Name})
}

// handleDeleteSecret removes a secret by name.
func handleDeleteSecret(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	guestSecretsMu.Lock()
	delete(guestSecrets, name)
	guestSecretsMu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// handleListSecrets returns secret names (not values) for introspection.
func handleListSecrets(w http.ResponseWriter, r *http.Request) {
	guestSecretsMu.RLock()
	names := make([]string, 0, len(guestSecrets))
	for n := range guestSecrets {
		names = append(names, n)
	}
	guestSecretsMu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]any{"secrets": names})
}
