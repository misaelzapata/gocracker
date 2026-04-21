package sandboxd

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// Lifecycle is the small interface the HTTP server depends on for
// sandbox create/delete. Splitting this out of *Manager lets tests
// inject a fake without standing up real VMs (container.Run needs
// KVM + root and is not unit-testable in isolation).
type Lifecycle interface {
	Create(req CreateSandboxRequest) (Sandbox, error)
	Delete(id string) error
}

// Server wires HTTP routes onto a Lifecycle (typically a *Manager)
// and a Store. Constructed via NewServer; Handler() returns the mux.
type Server struct {
	Lifecycle Lifecycle
	Store     *Store
}

// NewServer builds a Server backed by the given manager. Convenience
// constructor for the common case where Lifecycle and Store both come
// from the same manager.
func NewServer(m *Manager) *Server {
	return &Server{Lifecycle: m, Store: m.Store}
}

// Handler returns the mux for this slice's routes. Future slices
// add more routes in additional files (process_handlers.go, etc.)
// and chain them off the same mux.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("POST /sandboxes", s.handleCreateSandbox)
	mux.HandleFunc("GET /sandboxes", s.handleListSandboxes)
	mux.HandleFunc("GET /sandboxes/{id}", s.handleGetSandbox)
	mux.HandleFunc("DELETE /sandboxes/{id}", s.handleDeleteSandbox)
	return mux
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleCreateSandbox(w http.ResponseWriter, r *http.Request) {
	var req CreateSandboxRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("decode request: %w", err))
		return
	}
	sb, err := s.Lifecycle.Create(req)
	if err != nil {
		// ErrInvalidRequest → 400 so clients know to fix the request
		// instead of retrying blindly. Any other Create failure
		// (runtime / VM setup / OCI pull) → 500 + the partial
		// sandbox row so callers can DELETE it and try again.
		status := http.StatusInternalServerError
		if errors.Is(err, ErrInvalidRequest) {
			status = http.StatusBadRequest
		}
		writeJSON(w, status, map[string]any{
			"error":   err.Error(),
			"sandbox": sb,
		})
		return
	}
	writeJSON(w, http.StatusCreated, CreateSandboxResponse{Sandbox: sb})
}

func (s *Server) handleListSandboxes(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, ListSandboxesResponse{Sandboxes: s.Store.List()})
}

func (s *Server) handleGetSandbox(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sb, ok := s.Store.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, ErrSandboxNotFound)
		return
	}
	writeJSON(w, http.StatusOK, sb)
}

func (s *Server) handleDeleteSandbox(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.Lifecycle.Delete(id); err != nil {
		if errors.Is(err, ErrSandboxNotFound) {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, code int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}
