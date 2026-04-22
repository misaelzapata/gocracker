package sandboxd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/gocracker/gocracker/sandboxes/internal/templates"
)

// Lifecycle is the small interface the HTTP server depends on for
// sandbox create/delete. Splitting this out of *Manager lets tests
// inject a fake without standing up real VMs (container.Run needs
// KVM + root and is not unit-testable in isolation).
type Lifecycle interface {
	Create(req CreateSandboxRequest) (Sandbox, error)
	Delete(id string) error
}

// PoolLifecycle is the Fase 5 superset that adds warm-pool routes.
// Optional — Server falls back to cold-only when the underlying
// Lifecycle doesn't implement it.
type PoolLifecycle interface {
	RegisterPool(ctx context.Context, req CreatePoolRequest) (PoolRegistration, error)
	UnregisterPool(templateID string) error
	ListPools() []PoolRegistration
	LeaseSandbox(ctx context.Context, req LeaseSandboxRequest) (Sandbox, error)
	ReleaseLeased(id string) error
}

// TemplateLifecycle is the Fase 6 superset that adds template routes.
// Same pattern as PoolLifecycle — optional, the Server mounts the
// routes only when the underlying Lifecycle implements it.
type TemplateLifecycle interface {
	CreateTemplate(ctx context.Context, req CreateTemplateRequest) (CreateTemplateResponse, error)
	GetTemplate(id string) (templates.Template, error)
	ListTemplates() []templates.Template
	DeleteTemplate(id string) error
}

// PreviewLifecycle is the Fase 7 superset that adds preview routes.
// Implemented by *Manager — kept as an interface so the Server can
// test-inject a fake (same shape as Pool/Template lifecycles).
type PreviewLifecycle interface {
	MintPreview(id string, port uint16) (MintPreviewResponse, error)
	ServePreview(w http.ResponseWriter, r *http.Request)
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
	if pl, ok := s.Lifecycle.(PoolLifecycle); ok {
		mux.HandleFunc("POST /pools", s.handleRegisterPool(pl))
		mux.HandleFunc("GET /pools", s.handleListPools(pl))
		mux.HandleFunc("DELETE /pools/{id}", s.handleUnregisterPool(pl))
		mux.HandleFunc("POST /sandboxes/lease", s.handleLeaseSandbox(pl))
	}
	if tl, ok := s.Lifecycle.(TemplateLifecycle); ok {
		mux.HandleFunc("POST /templates", s.handleCreateTemplate(tl))
		mux.HandleFunc("GET /templates", s.handleListTemplates(tl))
		mux.HandleFunc("GET /templates/{id}", s.handleGetTemplate(tl))
		mux.HandleFunc("DELETE /templates/{id}", s.handleDeleteTemplate(tl))
	}
	if pv, ok := s.Lifecycle.(PreviewLifecycle); ok {
		mux.HandleFunc("POST /sandboxes/{id}/preview/{port}", s.handleMintPreview(pv))
		// Go 1.22 ServeMux: {token...} path wildcard captures the
		// remainder, so /previews/<tok>/foo/bar all routes here.
		mux.HandleFunc("/previews/{token...}", func(w http.ResponseWriter, r *http.Request) {
			pv.ServePreview(w, r)
		})
	}
	return mux
}

func (s *Server) handleMintPreview(pv PreviewLifecycle) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		portRaw := r.PathValue("port")
		port, err := parsePreviewPort(portRaw)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		res, err := pv.MintPreview(id, port)
		if err != nil {
			if errors.Is(err, ErrSandboxNotFound) {
				writeError(w, http.StatusNotFound, err)
				return
			}
			if errors.Is(err, ErrPreviewDisabled) {
				writeError(w, http.StatusNotImplemented, err)
				return
			}
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusCreated, res)
	}
}

func (s *Server) handleCreateTemplate(tl TemplateLifecycle) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req CreateTemplateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("decode request: %w", err))
			return
		}
		res, err := tl.CreateTemplate(r.Context(), req)
		if err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, ErrInvalidRequest) || errors.Is(err, templates.ErrInvalidSpec) {
				status = http.StatusBadRequest
			}
			writeError(w, status, err)
			return
		}
		writeJSON(w, http.StatusCreated, res)
	}
}

func (s *Server) handleListTemplates(tl TemplateLifecycle) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, ListTemplatesResponse{Templates: tl.ListTemplates()})
	}
}

func (s *Server) handleGetTemplate(tl TemplateLifecycle) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		t, err := tl.GetTemplate(id)
		if err != nil {
			if errors.Is(err, templates.ErrTemplateNotFound) {
				writeError(w, http.StatusNotFound, err)
				return
			}
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, t)
	}
}

func (s *Server) handleDeleteTemplate(tl TemplateLifecycle) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := tl.DeleteTemplate(id); err != nil {
			if errors.Is(err, templates.ErrTemplateNotFound) {
				writeError(w, http.StatusNotFound, err)
				return
			}
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) handleRegisterPool(pl PoolLifecycle) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req CreatePoolRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("decode request: %w", err))
			return
		}
		reg, err := pl.RegisterPool(r.Context(), req)
		if err != nil {
			status := http.StatusInternalServerError
			switch {
			case errors.Is(err, ErrInvalidRequest):
				status = http.StatusBadRequest
			case errors.Is(err, ErrPoolAlreadyRegistered):
				status = http.StatusConflict
			}
			writeError(w, status, err)
			return
		}
		writeJSON(w, http.StatusCreated, reg)
	}
}

func (s *Server) handleListPools(pl PoolLifecycle) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"pools": pl.ListPools()})
	}
}

func (s *Server) handleUnregisterPool(pl PoolLifecycle) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := pl.UnregisterPool(id); err != nil {
			if errors.Is(err, ErrPoolNotFound) {
				writeError(w, http.StatusNotFound, err)
				return
			}
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) handleLeaseSandbox(pl PoolLifecycle) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req LeaseSandboxRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("decode request: %w", err))
			return
		}
		sb, err := pl.LeaseSandbox(r.Context(), req)
		if err != nil {
			status := http.StatusInternalServerError
			switch {
			case errors.Is(err, ErrInvalidRequest):
				status = http.StatusBadRequest
			case errors.Is(err, ErrPoolNotFound):
				status = http.StatusNotFound
			}
			writeError(w, status, err)
			return
		}
		writeJSON(w, http.StatusCreated, CreateSandboxResponse{Sandbox: sb})
	}
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
	// If the sandbox came from a lease, route teardown through the
	// pool's Release path so the IP is freed and the slot is recycled.
	// Cold-booted sandboxes go through the original Delete path.
	if pl, ok := s.Lifecycle.(PoolLifecycle); ok {
		if sb, found := s.Store.Get(id); found && sb.poolTemplateID != "" {
			if err := pl.ReleaseLeased(id); err != nil {
				if errors.Is(err, ErrSandboxNotFound) {
					writeError(w, http.StatusNotFound, err)
					return
				}
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
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
