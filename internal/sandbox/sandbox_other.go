//go:build !linux && !windows

package sandbox

import (
	"errors"
	"fmt"
	"runtime"
)

// ErrAlreadyApplied is exported for parity with the Linux/Windows
// backings; it is never returned by this stub.
var ErrAlreadyApplied = errors.New("sandbox: already applied to this process")

// apply on unsupported platforms returns an error so callers don't
// silently run unsandboxed. macOS could in principle gain a sandbox-exec
// or seatbelt implementation; until then this is a deliberate refusal.
func apply(_ Config) error {
	return fmt.Errorf("sandbox: %s/%s is not a supported platform", runtime.GOOS, runtime.GOARCH)
}
