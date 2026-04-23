// Templates integration for sandboxd (Fase 6 slice 3). Wires the
// sandboxes/internal/templates package to sandboxd's HTTP control
// plane:
//
//   POST   /templates        — register + build (or cache-hit)
//   GET    /templates        — list registered templates
//   GET    /templates/{id}   — fetch one with state + snapshot dir
//   DELETE /templates/{id}   — remove from registry + snapshot dir
//
// Manager lazily initializes a Builder + Registry on first use,
// same sync.Once pattern as the pool integration from Fase 5.
// Templates persist to <StateDir>/templates.json.
package sandboxd

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/gocracker/gocracker/sandboxes/internal/templates"
)

// CreateTemplateRequest is the POST /templates payload. Mirrors
// templates.Spec with a convenience ID field the caller can set to
// pin a deterministic template id (CLI / tests use this; HTTP
// clients usually leave it empty to get a generated tmpl-<hex>).
type CreateTemplateRequest struct {
	ID         string                    `json:"id,omitempty"`
	Image      string                    `json:"image,omitempty"`
	Dockerfile string                    `json:"dockerfile,omitempty"`
	Context    string                    `json:"context,omitempty"`
	KernelPath string                    `json:"kernel_path"`
	MemMB      uint64                    `json:"mem_mb,omitempty"`
	CPUs       int                       `json:"cpus,omitempty"`
	Cmd        []string                  `json:"cmd,omitempty"`
	Env        []string                  `json:"env,omitempty"`
	WorkDir    string                    `json:"workdir,omitempty"`
	Readiness  *templates.ReadinessProbe `json:"readiness,omitempty"`
}

// CreateTemplateResponse wraps the built template + the CacheHit
// bit. Useful for operator visibility and for the CLI to report
// "already cached (Xms)" vs "built (Ys)".
type CreateTemplateResponse struct {
	Template templates.Template `json:"template"`
	CacheHit bool               `json:"cache_hit"`
}

// ListTemplatesResponse wraps a slice for consistency with the
// sandbox / pool list responses.
type ListTemplatesResponse struct {
	Templates []templates.Template `json:"templates"`
}

// templateManager holds the lazily-constructed Builder + Registry.
// Same shape as poolManager in pool.go.
type templateManager struct {
	registry *templates.Registry
	builder  *templates.Builder
}

// ensureTemplateManager initializes registry (<StateDir>/templates.json)
// + builder on first call. Subsequent calls return the same instance.
func (m *Manager) ensureTemplateManager() (*templateManager, error) {
	var err error
	m.tmplInit.Do(func() {
		var statePath string
		if m.StateDir != "" {
			statePath = filepath.Join(m.StateDir, "templates.json")
		}
		reg, regErr := templates.NewRegistry(statePath)
		if regErr != nil {
			err = fmt.Errorf("sandboxd: new template registry: %w", regErr)
			return
		}
		b := templates.NewBuilder(reg)
		b.VMMBinary = m.VMMBinary
		b.JailerBinary = m.JailerBinary
		m.tmplMgr = &templateManager{
			registry: reg,
			builder:  b,
		}
	})
	return m.tmplMgr, err
}

// CreateTemplate validates the request, derives a Spec, and calls
// the builder. Returns the resulting template + cache-hit flag.
// Invalid specs → ErrInvalidRequest (400 downstream).
func (m *Manager) CreateTemplate(ctx context.Context, req CreateTemplateRequest) (CreateTemplateResponse, error) {
	tm, err := m.ensureTemplateManager()
	if err != nil {
		return CreateTemplateResponse{}, err
	}
	spec := templates.Spec{
		Image:      req.Image,
		Dockerfile: req.Dockerfile,
		Context:    req.Context,
		KernelPath: req.KernelPath,
		MemMB:      req.MemMB,
		CPUs:       req.CPUs,
		Cmd:        req.Cmd,
		Env:        req.Env,
		WorkDir:    req.WorkDir,
		Readiness:  req.Readiness,
	}
	if err := spec.Validate(); err != nil {
		return CreateTemplateResponse{}, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	res, err := tm.builder.Build(ctx, req.ID, spec)
	if err != nil {
		return CreateTemplateResponse{}, err
	}
	return CreateTemplateResponse{Template: res.Template, CacheHit: res.CacheHit}, nil
}

// GetTemplate returns the template by id. Errors as ErrTemplateNotFound
// when the id is unknown.
func (m *Manager) GetTemplate(id string) (templates.Template, error) {
	tm, err := m.ensureTemplateManager()
	if err != nil {
		return templates.Template{}, err
	}
	t, ok := tm.registry.Get(id)
	if !ok {
		return templates.Template{}, templates.ErrTemplateNotFound
	}
	return t, nil
}

// ListTemplates returns all registered templates.
func (m *Manager) ListTemplates() []templates.Template {
	tm, err := m.ensureTemplateManager()
	if err != nil {
		return nil
	}
	return tm.registry.List()
}

// DeleteTemplate removes the template from the registry and
// (if this was the last reference to the snapshot dir) the snapshot
// from disk. Errors as ErrTemplateNotFound when the id is unknown.
func (m *Manager) DeleteTemplate(id string) error {
	tm, err := m.ensureTemplateManager()
	if err != nil {
		return err
	}
	delErr := tm.builder.Delete(id)
	if delErr != nil && errors.Is(delErr, templates.ErrTemplateNotFound) {
		return templates.ErrTemplateNotFound
	}
	return delErr
}
