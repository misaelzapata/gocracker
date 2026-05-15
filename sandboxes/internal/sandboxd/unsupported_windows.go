//go:build !linux

// Package sandboxd is the gocracker sandbox control plane. The full
// implementation is Linux-only because it transitively depends on
// pkg/container -> internal/oci (Linux-specific flock + image
// pipelines). The non-Linux build keeps a single ErrUnsupported
// sentinel so `go vet ./sandboxes/...` succeeds without bringing in
// any Linux-only deps.
package sandboxd

import "errors"

// ErrUnsupported is returned (or referenced) by callers that try to
// reach into sandboxd on a non-Linux build. The gocracker-sandboxd
// binary itself has its own main_other.go stub, so this symbol is
// currently unused at runtime — it exists so that future cross-
// platform tooling can detect the gating cleanly.
var ErrUnsupported = errors.New("sandboxes/internal/sandboxd: Linux-only (requires pkg/container / pkg/vmm KVM path)")
