//go:build !linux

// Package templates manages reusable, content-addressed VM snapshot
// templates. The full implementation is Linux-only because the
// template-build step boots a container.Run -> KVM VM and captures
// a snapshot — none of which exists on Windows yet (Phase 2 / WHP).
// This stub keeps the import path valid so `go vet ./sandboxes/...`
// on Windows does not fail with "no Go files".
package templates

import "errors"

// ErrUnsupported is returned by every public entry point on non-Linux
// builds. Currently no Windows binary imports the templates package —
// the gocracker-sandboxd cmd has its own main_other.go stub — but the
// sentinel is here so cross-platform tooling has something to check.
var ErrUnsupported = errors.New("sandboxes/internal/templates: Linux-only (template build path requires KVM cold-boot)")
