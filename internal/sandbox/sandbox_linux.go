//go:build linux

package sandbox

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// ErrAlreadyApplied is returned when Apply is invoked twice in the
// same process. The Linux backing is built on jailer which is also
// one-shot (chroot + pivot_root can only happen once).
var ErrAlreadyApplied = errors.New("sandbox: already applied to this process")

var applied bool

// apply implements the in-process side of internal/jailer. The
// existing jailer.Run is designed for the launcher use-case where it
// execs a fresh binary after chroot+pivot_root; that contract doesn't
// fit an "isolate the calling process" API. For the launcher path,
// callers still use jailer.Run directly from cmd/jailer.
//
// This adapter handles the subset that DOES apply mid-process:
// memory cap via RLIMIT_AS, working-dir change, and parent-death
// signal via prctl. Chroot / cgroup / netns are out of scope here
// because they require either root or a fresh process.
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
	// Resource limits: translate MemoryLimitBytes to RLIMIT_AS, and
	// CPUShares is ignored at this layer because cgroupv2 placement is
	// the caller's job (jailer.Run already does this for the launcher
	// path). We piggy-back on jailer's helper without spinning up a
	// full chroot.
	if cfg.MemoryLimitBytes > 0 {
		// jailer's applyResourceLimits supports only no-file / fsize;
		// for memory we set RLIMIT_AS directly via syscall.
		if err := setRLimitAS(cfg.MemoryLimitBytes); err != nil {
			return fmt.Errorf("sandbox: set memory limit: %w", err)
		}
	}
	if cfg.KillOnParentExit {
		if err := setPDeathSignalKILL(); err != nil {
			return fmt.Errorf("sandbox: set parent-death signal: %w", err)
		}
	}
	return nil
}

// setRLimitAS caps the process's virtual address space at limit bytes.
// The kernel returns ENOMEM from subsequent mmap/brk calls that would
// push usage past the cap, which is the closest userspace analogue to
// Windows' JOB_OBJECT_LIMIT_JOB_MEMORY.
func setRLimitAS(limit uint64) error {
	lim := &unix.Rlimit{Cur: limit, Max: limit}
	return unix.Setrlimit(unix.RLIMIT_AS, lim)
}

// setPDeathSignalKILL asks the kernel to deliver SIGKILL to this
// process when its parent exits. Set via prctl(PR_SET_PDEATHSIG, ...).
// Note: this only fires for the *original* parent; if the process is
// re-parented to init via daemonize this becomes a no-op.
func setPDeathSignalKILL() error {
	return unix.Prctl(unix.PR_SET_PDEATHSIG, uintptr(unix.SIGKILL), 0, 0, 0)
}
