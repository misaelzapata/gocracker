//go:build windows

package sandbox

import (
	"errors"
	"fmt"
	"os"

	"github.com/gocracker/gocracker/internal/winsandbox"
)

// ErrAlreadyApplied is returned when Apply is invoked twice in the
// same process. The Windows backing assigns the current process to a
// Job Object — re-assignment is technically possible (Win8+ allows
// nested jobs) but for predictable semantics we forbid it.
var ErrAlreadyApplied = errors.New("sandbox: already applied to this process")

var applied bool

func apply(cfg Config) error {
	if applied {
		return ErrAlreadyApplied
	}
	applied = true

	if cfg.WorkingDir != "" {
		if err := os.Chdir(cfg.WorkingDir); err != nil {
			return fmt.Errorf("sandbox: chdir %s: %w", cfg.WorkingDir, err)
		}
	}
	winCfg := winsandbox.Config{
		MemoryLimitBytes: cfg.MemoryLimitBytes,
		CPUShares:        cfg.CPUShares,
		NoNetwork:        cfg.NoNetwork,
		KillOnJobClose:   cfg.KillOnParentExit,
	}
	return winsandbox.Apply(winCfg)
}
