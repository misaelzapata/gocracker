//go:build !linux

// Package pool is the warm-pool manager. On non-Linux hosts it
// transitively depends on pkg/container -> internal/oci which itself
// requires Linux-only syscalls (flock, etc.), so the package is
// reduced to a sentinel error here until the underlying stack grows
// Windows support (Phase 2 / WHP). Importers should gate their own
// call sites with //go:build linux; this stub exists purely so that
// `go vet ./...` on Windows does not error with "no Go files".
package pool

import "errors"

// ErrUnsupported is returned by every public entry point on non-Linux
// builds. Currently nothing on Windows calls into the pool — the
// gocracker-sandboxd binary uses a main_other.go stub — but exporting
// the error keeps the package shape coherent for future cross-platform
// callers.
var ErrUnsupported = errors.New("sandboxes/internal/pool: Linux-only (cold-boot pool requires pkg/container)")
