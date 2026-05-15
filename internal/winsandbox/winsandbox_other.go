//go:build !windows

// Package winsandbox is a no-op on non-Windows platforms. Linux users
// should call into internal/sandbox (which forwards to internal/jailer)
// instead. The stub is here only so cross-platform tooling can `go vet`
// every package without GOOS gymnastics.
package winsandbox

import "errors"

// Config matches the Windows definition field-for-field so callers
// outside the package can refer to it on any GOOS without build tags.
type Config struct {
	MemoryLimitBytes       uint64
	CPUShares              int
	NoNetwork              bool
	KillOnJobClose         bool
	DisableRestrictedToken bool
	DisableLowIntegrity    bool
}

// ErrAlreadyApplied mirrors the Windows symbol so symbol-imports
// resolve on every GOOS.
var ErrAlreadyApplied = errors.New("winsandbox: already applied to this process")

// ErrUnsupported is returned by every entrypoint on non-Windows.
var ErrUnsupported = errors.New("winsandbox: unsupported on this GOOS")

// Apply on non-Windows is a hard error. Callers should branch on GOOS
// via the internal/sandbox cross-platform interface — they should
// never see this stub at runtime in a correctly-built binary.
func Apply(_ Config) error { return ErrUnsupported }
