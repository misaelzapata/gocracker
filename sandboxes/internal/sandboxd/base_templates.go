package sandboxd

import (
	"context"
	"errors"
	"os"
	"sync"
	"time"

	gclog "github.com/gocracker/gocracker/internal/log"
	"github.com/gocracker/gocracker/sandboxes/internal/templates"
)

// BaseTemplate is a canonical template preregistered at sandboxd
// startup so the SDK can call create_sandbox(template="base-python")
// without the caller having to POST a fully-specified Dockerfile +
// image string. Matches the Daytona-style "built-in language runtimes"
// surface — users land in a familiar Python/Node/Bun/Go sandbox with
// the toolbox agent already live.
type BaseTemplate struct {
	ID    string // well-known name: "base-python", "base-node", ...
	Image string // OCI ref with the language runtime baked in
	// MemMB/CPUs: zero = container.Run defaults (256MB / 1 vCPU).
	MemMB uint64
	CPUs  int
}

// DefaultBaseTemplates is the canonical list we register when sandboxd
// boots with a kernel configured. Adding more languages (rust, deno,
// nextjs, etc.) goes here — the list is intentionally short: anything
// exotic belongs in user-defined templates so we don't bloat the
// "default sandboxd" with opinions the user didn't ask for.
func DefaultBaseTemplates() []BaseTemplate {
	return []BaseTemplate{
		{ID: "base-python", Image: "python:3.12-alpine"},
		{ID: "base-node", Image: "node:22-alpine"},
		{ID: "base-bun", Image: "oven/bun:1"},
		{ID: "base-go", Image: "golang:1.23-alpine"},
	}
}

// EnsureBaseTemplates creates the canonical base templates if they
// aren't already in the registry. Runs asynchronously from sandboxd's
// startup — builds happen in background goroutines so the HTTP listener
// is up immediately. Each build is a one-time ~2-3s cold-boot + warm
// capture; subsequent restarts hit the warm-cache and finish in ms.
//
// kernelPath MUST be non-empty — otherwise no templates are registered
// and the caller sees errors when asking for base-*. Errors per
// template are logged but don't cascade (one broken seed doesn't block
// the other three).
func (m *Manager) EnsureBaseTemplates(ctx context.Context, kernelPath string, seeds []BaseTemplate) {
	if kernelPath == "" {
		return
	}
	if seeds == nil {
		seeds = DefaultBaseTemplates()
	}
	var wg sync.WaitGroup
	for _, s := range seeds {
		s := s
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Fast path: already registered (warm-cache hit or a
			// previous startup's registration).
			if _, err := m.GetTemplate(s.ID); err == nil {
				gclog.VMM.Info("base template ready", "id", s.ID, "cache", "hit")
				return
			}
			req := CreateTemplateRequest{
				ID:         s.ID,
				Image:      s.Image,
				KernelPath: kernelPath,
				MemMB:      s.MemMB,
				CPUs:       s.CPUs,
			}
			t0 := time.Now()
			buildCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
			res, err := m.CreateTemplate(buildCtx, req)
			cancel()
			if err != nil {
				// ErrIDInUse (race: another goroutine created it
				// between the Get check and CreateTemplate) is benign.
				if errors.Is(err, templates.ErrTemplateNotFound) || isAlreadyInUse(err) {
					gclog.VMM.Info("base template already present", "id", s.ID)
					return
				}
				gclog.VMM.Warn("base template build failed", "id", s.ID, "image", s.Image, "err", err.Error())
				return
			}
			gclog.VMM.Info("base template built",
				"id", s.ID,
				"image", s.Image,
				"cache_hit", res.CacheHit,
				"build_ms", time.Since(t0).Milliseconds(),
			)
		}()
	}
	wg.Wait()
}

// isAlreadyInUse inspects a CreateTemplate error to detect the
// "ID already taken" race without pulling in a dedicated sentinel error
// (templates.Add returns a raw fmt.Errorf).
func isAlreadyInUse(err error) bool {
	s := err.Error()
	return containsAny(s, []string{"already in use", "already exists"})
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
	}
	return false
}

// BaseTemplateKernelPathFromEnv returns the kernel path from env
// GOCRACKER_KERNEL or the empty string if unset. Helper so main.go
// doesn't need to import os just for this one line.
func BaseTemplateKernelPathFromEnv() string {
	return os.Getenv("GOCRACKER_KERNEL")
}

