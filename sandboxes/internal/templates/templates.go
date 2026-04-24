// Package templates manages reusable, content-addressed VM snapshot
// templates. A Template captures the inputs needed to cold-boot a
// gocracker VM (image, kernel, mem, vCPUs, env...) into a stable
// SpecHash, then memoizes the resulting warm-cache snapshot under
// that hash. A second Build of the same Spec is a no-op (cache hit).
//
// Why templates exist:
//   - Pool registration on a template_id can skip the per-pool
//     prewarm cold-boot — the snapshot already exists.
//   - Identical template specs across processes / sandboxd restarts
//     share one snapshot directory on disk.
//   - User-defined templates (e.g. "my-python-app with fastapi
//     pre-installed") build once + are reused across many
//     LeaseSandbox calls without repeating the install.
//
// This file (slice 1) defines the data model + SpecHash + a
// JSON-backed in-memory registry. Slice 2 adds the Build path;
// slice 3 wires HTTP; slice 4 plugs into the pool.
package templates

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/gocracker/gocracker/pkg/container"
)

// State is the lifecycle of a template.
type State string

const (
	// StateBuilding: cold-boot + WarmCapture in progress.
	StateBuilding State = "building"
	// StateReady: snapshot written, cache hit on next Build.
	StateReady State = "ready"
	// StateError: build failed; LastError carries the message.
	StateError State = "error"
)

// Spec is the deterministic input to SpecHash. Everything that would
// produce a different snapshot belongs here; everything that
// shouldn't (LastError, CreatedAt, ID) does not. Two Specs that hash
// to the same value are interchangeable as far as the snapshot
// content is concerned.
//
// JSON serialization is the canonical representation — SpecHash
// hashes the JSON bytes, so adding a field with a non-zero default
// changes every existing template's hash. Bump CACHE_FORMAT_VERSION
// in that case so old caches are abandoned cleanly instead of
// silently pretending to match.
type Spec struct {
	Image      string   `json:"image,omitempty"`
	Dockerfile string   `json:"dockerfile,omitempty"`
	Context    string   `json:"context,omitempty"`
	KernelPath string   `json:"kernel_path"`
	MemMB      uint64   `json:"mem_mb,omitempty"`
	CPUs       int      `json:"cpus,omitempty"`
	Cmd        []string `json:"cmd,omitempty"`
	Env        []string `json:"env,omitempty"`
	WorkDir    string   `json:"workdir,omitempty"`

	// Readiness, when non-nil, tells the builder to wait until the app
	// inside the guest responds 2xx on Readiness.HTTPPort/HTTPPath
	// before taking the snapshot. The captured memory image then
	// contains the already-running app — a lease restoring this
	// snapshot comes up with the service live, skipping the 2-4 s
	// "wait for postgres/flask init" tax that would otherwise run on
	// every lease. Nil = standard boot-only snapshot (fast to build,
	// but the caller's CMD needs to run again on each lease).
	Readiness *ReadinessProbe `json:"readiness,omitempty"`
}

// ReadinessProbe is an HTTP readiness check the template builder runs
// after cold-boot, before taking the snapshot. Any 2xx response counts
// as ready; anything else retries until Timeout elapses. The probe goes
// through the toolbox agent's /proxy/http/<port> surface so the app
// doesn't need its own vsock listener.
type ReadinessProbe struct {
	// HTTPPort is the guest-side TCP port the app listens on.
	HTTPPort uint16 `json:"http_port"`
	// HTTPPath is the request path; defaults to "/".
	HTTPPath string `json:"http_path,omitempty"`
	// Timeout caps total wait before the build errors. Default 2 min.
	Timeout time.Duration `json:"timeout,omitempty"`
	// Interval is the gap between attempts. Default 500 ms.
	Interval time.Duration `json:"interval,omitempty"`
}

// CacheFormatVersion bumps when the Spec/Build pipeline changes in
// a way that invalidates existing snapshots (e.g. new Spec field
// with default value, change to WarmCapture's snapshot.json schema).
// Embedded into SpecHash so old cached snapshots no longer collide
// with new specs.
const CacheFormatVersion = 1

// SpecHash returns a deterministic SHA-256 hex over the spec's JSON
// representation prefixed with the cache format version. Two Specs
// that produce the same SpecHash MUST produce identical snapshots —
// that's the whole guarantee callers rely on.
//
// The version prefix means a CACHE_FORMAT_VERSION bump invalidates
// every prior hash; the on-disk snapshot dirs become orphan but
// don't risk a mismatch. Garbage-collection of orphans is a
// separate concern (slice 3 / fase 6 follow-up).
func SpecHash(s Spec) string {
	// Marshal with sorted keys so map ordering doesn't perturb the
	// hash. encoding/json already sorts map keys for json.Marshal,
	// but Spec is a struct so this is just defense in depth.
	canonical := struct {
		Version int  `json:"v"`
		Spec    Spec `json:"spec"`
	}{Version: CacheFormatVersion, Spec: s}
	body, _ := json.Marshal(canonical)
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// Template is the persisted record. ID is human-readable (e.g.
// "tmpl-abc123") for log/CLI ergonomics; SpecHash is the
// content-addressed key. Multiple Templates can share a SpecHash
// (e.g. a user creates "my-python" twice with identical specs); the
// builder de-duplicates so they share the same snapshot dir.
type Template struct {
	ID          string    `json:"id"`
	SpecHash    string    `json:"spec_hash"`
	Spec        Spec      `json:"spec"`
	State       State     `json:"state"`
	SnapshotDir string    `json:"snapshot_dir,omitempty"` // populated when State=ready
	LastError   string    `json:"last_error,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// AsRunOptions converts the template's Spec into container.RunOptions
// suitable for a fresh container.Run call (with WarmCapture true so
// the snapshot lands in the warmcache).
func (t *Template) AsRunOptions() container.RunOptions {
	return container.RunOptions{
		Image:           t.Spec.Image,
		Dockerfile:      t.Spec.Dockerfile,
		Context:         t.Spec.Context,
		KernelPath:      t.Spec.KernelPath,
		MemMB:           t.Spec.MemMB,
		CPUs:            t.Spec.CPUs,
		Cmd:             t.Spec.Cmd,
		Env:             t.Spec.Env,
		WorkDir:         t.Spec.WorkDir,
		ExecEnabled:     true,
		WarmCapture:     true,
		InteractiveExec: true,
	}
}

// ErrTemplateNotFound is returned by Get/Delete when the id is unknown.
var ErrTemplateNotFound = errors.New("templates: template not found")

// ErrInvalidSpec is returned by Validate when required fields are missing.
var ErrInvalidSpec = errors.New("templates: invalid spec")

// Validate checks the spec is internally consistent. Required:
// KernelPath, plus one of Image / Dockerfile.
func (s Spec) Validate() error {
	if s.KernelPath == "" {
		return fmt.Errorf("%w: kernel_path required", ErrInvalidSpec)
	}
	if s.Image == "" && s.Dockerfile == "" {
		return fmt.Errorf("%w: image or dockerfile required", ErrInvalidSpec)
	}
	return nil
}

// Registry is the in-memory + JSON-backed catalog of templates.
// Persistence is best-effort (same shape as sandboxd.Store): on
// process restart the registry rebuilds from disk; missing snapshot
// dirs are detected lazily by Build (cache miss → rebuild).
type Registry struct {
	statePath string

	mu        sync.Mutex
	templates map[string]*Template
}

// NewRegistry opens (or creates) a JSON-backed registry at the given
// path. statePath="" means in-memory only — useful for tests.
func NewRegistry(statePath string) (*Registry, error) {
	r := &Registry{statePath: statePath, templates: map[string]*Template{}}
	if statePath == "" {
		return r, nil
	}
	if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
		return nil, fmt.Errorf("templates: mkdir parent %s: %w", filepath.Dir(statePath), err)
	}
	data, err := os.ReadFile(statePath)
	if err != nil {
		if os.IsNotExist(err) {
			return r, nil
		}
		return nil, fmt.Errorf("templates: read %s: %w", statePath, err)
	}
	if len(data) == 0 {
		return r, nil
	}
	var loaded map[string]*Template
	if err := json.Unmarshal(data, &loaded); err != nil {
		// Same recovery shape as sandboxd.Store: rename corrupt
		// file aside, start fresh. Better than refusing to boot.
		sidecar := fmt.Sprintf("%s.corrupt-%d", statePath, time.Now().Unix())
		if rerr := os.Rename(statePath, sidecar); rerr != nil {
			_ = os.Remove(statePath)
		}
		return r, nil
	}
	r.templates = loaded
	return r, nil
}

// Add inserts a fresh template and persists. Returns an error if the
// ID is taken — caller should generate non-colliding IDs.
func (r *Registry) Add(t *Template) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.templates[t.ID]; exists {
		return fmt.Errorf("templates: id %q already in use", t.ID)
	}
	now := time.Now().UTC()
	if t.CreatedAt.IsZero() {
		t.CreatedAt = now
	}
	t.UpdatedAt = now
	r.templates[t.ID] = t
	r.persistLocked()
	return nil
}

// Get returns a snapshot of the template by id. Snapshots are values
// (decoupled from concurrent Updates) so callers can JSON-encode
// them without locking.
func (r *Registry) Get(id string) (Template, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.templates[id]
	if !ok {
		return Template{}, false
	}
	return *t, true
}

// FindBySpecHash looks up an existing READY template that matches the
// hash. Used by Build to short-circuit when a duplicate spec is
// re-registered. Returns the first ready match (oldest); building
// templates are skipped.
func (r *Registry) FindBySpecHash(hash string) (Template, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var best *Template
	for _, t := range r.templates {
		if t.SpecHash != hash || t.State != StateReady {
			continue
		}
		if best == nil || t.CreatedAt.Before(best.CreatedAt) {
			best = t
		}
	}
	if best == nil {
		return Template{}, false
	}
	return *best, true
}

// Update applies fn to the template under the registry lock and
// persists. Returns false if the id isn't found.
func (r *Registry) Update(id string, fn func(*Template)) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.templates[id]
	if !ok {
		return false
	}
	fn(t)
	t.UpdatedAt = time.Now().UTC()
	r.persistLocked()
	return true
}

// Remove deletes the template from the registry. Returns the removed
// record so the caller can clean up the on-disk snapshot dir
// (Registry doesn't own filesystem; that's the builder's job).
func (r *Registry) Remove(id string) (*Template, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.templates[id]
	if !ok {
		return nil, false
	}
	delete(r.templates, id)
	r.persistLocked()
	return t, true
}

// List returns snapshots of every template sorted by CreatedAt ascending.
func (r *Registry) List() []Template {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Template, 0, len(r.templates))
	for _, t := range r.templates {
		out = append(out, *t)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

func (r *Registry) persistLocked() {
	if r.statePath == "" {
		return
	}
	data, err := json.MarshalIndent(r.templates, "", "  ")
	if err != nil {
		return
	}
	tmp := r.statePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, r.statePath)
}
