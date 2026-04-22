package templates

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/gocracker/gocracker/pkg/container"
	"github.com/gocracker/gocracker/pkg/warmcache"
)

// ColdBooter abstracts the cold-boot-and-capture step so tests can
// inject a fake (returning a fabricated snapshot dir) instead of
// standing up real KVM. Production wires the default via
// containerColdBooter which calls container.Run + waits for
// WarmDone + warmcache.Lookup.
type ColdBooter interface {
	BootAndCapture(ctx context.Context, opts container.RunOptions) (snapshotDir string, err error)
}

// Builder turns a Spec into a ready Template by cold-booting the VM
// once, capturing the warmcache snapshot, and storing the dir in the
// registry. Subsequent Builds with the same Spec hit the cache and
// return in ~ms instead of re-running the cold-boot.
//
// The builder is intentionally minimal — it doesn't own goroutine
// lifecycle (callers can wrap Build in their own goroutine if they
// want async builds). Concurrent Builds for the SAME SpecHash are
// serialized via per-hash mutex so two near-simultaneous POST
// /templates calls don't both cold-boot the same image.
type Builder struct {
	Registry *Registry
	// Booter is the cold-boot-and-capture implementation. nil =
	// production default (containerColdBooter). Tests inject a
	// fake.
	Booter ColdBooter

	// VMMBinary / JailerBinary are plumbed into the container.Run
	// RunOptions on every Build. These aren't part of Spec (they're
	// per-operator, not per-template — the SAME snapshot can be
	// produced by different operator paths) so they live on the
	// Builder instead of SpecHash.
	VMMBinary    string
	JailerBinary string

	mu       sync.Mutex
	building map[string]*sync.Mutex // spec_hash → per-hash lock
}

// NewBuilder wires a builder onto a registry. Both must be set.
func NewBuilder(r *Registry) *Builder {
	return &Builder{
		Registry: r,
		building: map[string]*sync.Mutex{},
	}
}

// ErrSpecRequired is returned by Build when the spec doesn't pass
// Validate.
var ErrSpecRequired = errors.New("templates: spec validation failed")

// BuildResult is what Build returns. ID is the registry key (newly
// generated for a fresh build, reused for a cache hit). CacheHit
// distinguishes the two paths so callers / metrics can track ratio.
type BuildResult struct {
	Template Template
	CacheHit bool
}

// Build registers the spec with the given ID (or generates one if
// id == "") and returns a Ready template. If a Ready template with
// the same SpecHash already exists, Build is a near-no-op: the new
// id maps to the existing snapshot, no cold-boot runs.
//
// On cache miss, Build cold-boots via container.Run with WarmCapture
// + InteractiveExec, waits for WarmDone, stops the VM, and stores
// the snapshot dir on the template record. On error the template
// transitions to StateError with LastError populated.
//
// ctx cancellation aborts the build; the underlying container.Run
// won't gracefully cancel mid-boot but Build returns promptly once
// the VM lands and Close runs. A subsequent Build for the same Spec
// will retry from scratch (the failed template stays in the registry
// for diagnostic; FindBySpecHash skips it).
func (b *Builder) Build(ctx context.Context, id string, spec Spec) (BuildResult, error) {
	if err := spec.Validate(); err != nil {
		return BuildResult{}, err
	}
	hash := SpecHash(spec)

	// Per-hash lock: serialize concurrent Build calls for the same
	// spec so we never cold-boot twice for the same snapshot.
	hashLock := b.lockForHash(hash)
	hashLock.Lock()
	defer hashLock.Unlock()

	// Cache hit fast path.
	if existing, ok := b.Registry.FindBySpecHash(hash); ok {
		// Verify the snapshot dir still exists — operators sometimes
		// rm -rf the cache to free disk. If gone, fall through to
		// rebuild.
		if existing.SnapshotDir != "" && pathExists(existing.SnapshotDir) {
			tmpl := &Template{
				ID:          chooseID(id),
				SpecHash:    hash,
				Spec:        spec,
				State:       StateReady,
				SnapshotDir: existing.SnapshotDir,
				CreatedAt:   time.Now().UTC(),
			}
			if err := b.Registry.Add(tmpl); err != nil {
				return BuildResult{}, fmt.Errorf("templates: register cache hit: %w", err)
			}
			return BuildResult{Template: *tmpl, CacheHit: true}, nil
		}
		// Stale registry entry — continue to rebuild path.
	}

	// Cache miss: register Building, cold-boot, transition to Ready
	// or Error.
	tmpl := &Template{
		ID:        chooseID(id),
		SpecHash:  hash,
		Spec:      spec,
		State:     StateBuilding,
		CreatedAt: time.Now().UTC(),
	}
	if err := b.Registry.Add(tmpl); err != nil {
		return BuildResult{}, fmt.Errorf("templates: register building: %w", err)
	}

	booter := b.Booter
	if booter == nil {
		booter = containerColdBooter{}
	}
	opts := tmpl.AsRunOptions()
	opts.VMMBinary = b.VMMBinary
	opts.JailerBinary = b.JailerBinary
	snapshotDir, err := booter.BootAndCapture(ctx, opts)
	if err != nil {
		b.Registry.Update(tmpl.ID, func(t *Template) {
			t.State = StateError
			t.LastError = err.Error()
		})
		return BuildResult{}, fmt.Errorf("templates: build %s: %w", tmpl.ID, err)
	}
	b.Registry.Update(tmpl.ID, func(t *Template) {
		t.State = StateReady
		t.SnapshotDir = snapshotDir
	})
	final, _ := b.Registry.Get(tmpl.ID)
	return BuildResult{Template: final, CacheHit: false}, nil
}

// containerColdBooter is the production ColdBooter: cold-boots via
// container.Run with WarmCapture, waits for WarmDone (snapshot
// write), stops the VM, and returns the warmcache dir.
type containerColdBooter struct{}

func (containerColdBooter) BootAndCapture(ctx context.Context, opts container.RunOptions) (string, error) {
	result, err := container.Run(opts)
	if err != nil {
		return "", err
	}
	defer result.Close()
	defer result.VM.Stop()
	if result.WarmDone != nil {
		select {
		case <-result.WarmDone:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	// After WarmDone, container.Run has populated the warmcache. Look
	// up the snapshot dir for the spec's key.
	//
	// container.Run applies a few defaults to opts before computing
	// the key (Arch=runtime.GOARCH when empty; same for a couple of
	// other fields). Replicate them here so our Lookup key matches
	// the Store key — otherwise the lookup misses for a snapshot we
	// just successfully captured, which was the original symptom of
	// this bug.
	lookupOpts := opts
	if lookupOpts.Arch == "" {
		lookupOpts.Arch = runtime.GOARCH
	}
	key, ok := container.ComputeWarmCacheKey(lookupOpts)
	if !ok {
		return "", errors.New("templates: warmcache key not derivable from spec")
	}
	dir, hit := warmcache.Lookup(warmcache.DefaultRoot(), key)
	if !hit {
		return "", errors.New("templates: warm-cache lookup miss after capture (snapshot bundling may have failed)")
	}
	return dir, nil
}

// Delete removes the template from the registry AND attempts to
// delete the snapshot dir from disk. The snapshot dir is shared
// across templates with the same SpecHash — only deleted if no
// other Ready template references the same dir.
//
// Returns ErrTemplateNotFound when the id is unknown.
func (b *Builder) Delete(id string) error {
	t, ok := b.Registry.Remove(id)
	if !ok {
		return ErrTemplateNotFound
	}
	if t.SnapshotDir == "" {
		return nil
	}
	// Don't delete the snapshot if any other Ready template still
	// points at it (same SpecHash → same dir).
	for _, other := range b.Registry.List() {
		if other.SnapshotDir == t.SnapshotDir && other.State == StateReady {
			return nil
		}
	}
	_ = os.RemoveAll(t.SnapshotDir)
	return nil
}

func (b *Builder) lockForHash(hash string) *sync.Mutex {
	b.mu.Lock()
	defer b.mu.Unlock()
	if m, ok := b.building[hash]; ok {
		return m
	}
	m := &sync.Mutex{}
	b.building[hash] = m
	return m
}

// chooseID returns the requested id or generates a new one if empty.
// Format "tmpl-XXXXXX" with 12 hex chars (~48 bits of entropy —
// plenty given templates are user-created, low cardinality).
func chooseID(requested string) string {
	if requested != "" {
		return requested
	}
	var b [6]byte
	_, _ = rand.Read(b[:])
	return "tmpl-" + hex.EncodeToString(b[:])
}

func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
