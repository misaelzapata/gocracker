// Package sandbox provides a cross-platform interface for confining the
// current process to a resource-limited container. On Linux it wraps
// the existing internal/jailer (chroot + cgroupv2 + namespaces). On
// Windows it wraps internal/winsandbox (Job Object + restricted token).
// On every other GOOS Apply returns an "unsupported platform" error so
// downstream code can compile but not silently skip sandboxing.
//
// The intended caller is the VMM entrypoint: spawn the process, then
// call sandbox.Apply early — before opening hypervisor handles or
// touching guest memory — so the OS enforces the limits for the rest
// of the process lifetime.
package sandbox

// Config selects which restrictions to apply. Zero-valued fields mean
// "no limit" / "don't change". Fields that don't translate across
// platforms (e.g. CPUShares on Windows is mapped to CPU rate control;
// MemoryLimitBytes on Linux becomes a cgroup memory.max) are documented
// on the platform-specific Apply implementations.
type Config struct {
	// MemoryLimitBytes caps total committed memory for the sandboxed
	// process / job. 0 means no limit. Translates to:
	//   Linux  : cgroupv2 memory.max
	//   Windows: JOB_OBJECT_LIMIT_JOB_MEMORY (per-job)
	MemoryLimitBytes uint64

	// CPUShares is a relative CPU weight (1–10000 on Windows JOBs;
	// converted to cpu.weight on cgroupv2). 0 means unconstrained.
	CPUShares int

	// WorkingDir, if non-empty, is the chroot target on Linux or the
	// SetCurrentDirectory target on Windows.
	WorkingDir string

	// NoNetwork drops network access. Linux: detach net namespace.
	// Windows: best-effort via job object UI restrictions + network
	// rate control (not a hard guarantee — Windows does not give us a
	// netns equivalent without HCS).
	NoNetwork bool

	// KillOnParentExit ties the sandboxed child to the parent so an
	// abrupt parent termination kills it. Linux: PR_SET_PDEATHSIG.
	// Windows: JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE on the job handle.
	KillOnParentExit bool
}

// Apply attaches the sandbox to the current process. Once it returns,
// every subsequently-allocated resource (memory, file handle, child
// process) is subject to the configured caps. Apply is one-shot: a
// second call from the same process is rejected with ErrAlreadyApplied.
//
// Errors from Apply leave the process in a partial state — callers
// should treat them as fatal. There is no rollback path because some
// of the underlying primitives (job object assignment, restricted
// token install) are not reversible.
func Apply(cfg Config) error {
	return apply(cfg)
}
