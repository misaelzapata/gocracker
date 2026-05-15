//go:build windows

// Windows controlled-stub for Dockerfile RUN.
//
// On Linux, Dockerfile RUN executes inside the target rootfs via user
// namespaces (rootless) or chroot+mount-namespace (privileged). Neither
// primitive exists on Windows, so we cannot run Linux-targeted RUN steps
// directly on the host.
//
// The "real" answer is to spin up a throwaway micro-VM via
// pkg/vmm.BootLinuxOnWHP and execute the RUN command inside the guest, with
// the build rootfs bind-mounted as a virtio-fs share. That plumbing is on
// the windows-port roadmap but not wired up end-to-end yet (the seam touches
// pkg/vmm, internal/sharedfs, and the guest agent, which are still
// stabilizing on this branch).
//
// In the meantime this file provides a controlled stub: instead of hard-
// failing the entire dockerfile build on the first RUN instruction (which
// would block every CI smoke test that pulls an image with an apt-get or
// pip install step), we log a clear warning and skip the RUN. The rest of
// the build — FROM, COPY, ADD, ENV, WORKDIR, CMD, ENTRYPOINT, layer
// committing, and image config emission — proceeds normally. Builds that
// only need to repackage an upstream OCI image plus COPY/ADD/ENV will
// produce a usable rootfs; builds that depend on RUN side effects (package
// installs, code compilation) will produce an incomplete rootfs and that
// limitation is surfaced loudly so callers don't silently ship a broken
// image to production.
//
// To exit the stub regime and fail hard instead (recommended for any
// release build on Windows), set GOCRACKER_WINDOWS_RUN_STRICT=1 in the
// builder's environment. To eagerly opt into the WHP micro-VM path once
// it lands, set GOCRACKER_WINDOWS_RUN_BACKEND=whp (currently a no-op that
// also returns nil, kept here so callers can start adopting the env var
// before the backend is wired).

package dockerfile

import (
	"fmt"
	"os"
	"strings"
)

const (
	// windowsRunStrictEnv, when set to "1" / "true", makes the Windows
	// stub fail hard on RUN instead of warn-and-skip. Useful for release
	// builds where a silently-incomplete rootfs is unacceptable.
	windowsRunStrictEnv = "GOCRACKER_WINDOWS_RUN_STRICT"
	// windowsRunBackendEnv selects the future Windows RUN backend. The
	// only currently-recognized values are "" (default, stub) and "whp"
	// (also stub today; reserved for the throwaway-microVM path).
	windowsRunBackendEnv = "GOCRACKER_WINDOWS_RUN_BACKEND"
)

// runRootless on Windows is a controlled stub. See package-level comment.
func (b *builder) runRootless(args, envArgs []string) error {
	return windowsRunStub("rootless", args, nil)
}

// runPrivileged on Windows is a controlled stub. See package-level comment.
func (b *builder) runPrivileged(args, envArgs []string, mounts []RunMount) error {
	return windowsRunStub("privileged", args, mounts)
}

// rootlessErrorMessage formats an error string the same way the Linux
// implementation does, so the cross-platform caller in dockerfile.go can
// rely on a non-nil result.
func rootlessErrorMessage(err error) string {
	if err == nil {
		return ""
	}
	return strings.TrimSpace(err.Error())
}

// windowsRunStub centralizes the stub behavior so both runRootless and
// runPrivileged emit identical diagnostics. It honors
// GOCRACKER_WINDOWS_RUN_STRICT and GOCRACKER_WINDOWS_RUN_BACKEND.
func windowsRunStub(mode string, args []string, mounts []RunMount) error {
	displayCmd := "<empty>"
	if len(args) > 0 {
		// Render the command without dumping a 4 KB shell heredoc to the
		// console: clamp at 120 characters with an ellipsis.
		joined := strings.Join(args, " ")
		const maxLen = 120
		if len(joined) > maxLen {
			joined = joined[:maxLen] + "..."
		}
		displayCmd = joined
	}

	if isTruthyEnv(windowsRunStrictEnv) {
		return fmt.Errorf("RUN is not implemented on Windows yet (mode=%s, cmd=%s); "+
			"set %s=0 to fall back to the warn-and-skip stub", mode, displayCmd, windowsRunStrictEnv)
	}

	backend := strings.ToLower(strings.TrimSpace(os.Getenv(windowsRunBackendEnv)))
	switch backend {
	case "", "stub":
		// Default: warn and skip.
	case "whp":
		// Reserved for the future BootLinuxOnWHP path. Today this still
		// returns nil so callers that have already adopted the env var
		// don't break, but we mark the diagnostic so the gap is obvious.
		fmt.Fprintf(os.Stderr,
			"[build] WARNING: %s=whp requested but the WHP micro-VM RUN backend is not wired up yet; "+
				"falling back to controlled stub\n",
			windowsRunBackendEnv)
	default:
		return fmt.Errorf("unrecognized %s=%q (expected \"\", \"stub\", or \"whp\")",
			windowsRunBackendEnv, backend)
	}

	mountHint := ""
	if len(mounts) > 0 {
		mountHint = fmt.Sprintf(" (with %d --mount spec(s) ignored)", len(mounts))
	}
	fmt.Fprintf(os.Stderr,
		"[build] WARNING: Dockerfile RUN is not end-to-end on Windows yet; "+
			"skipping %s RUN%s: %s\n",
		mode, mountHint, displayCmd)
	fmt.Fprintf(os.Stderr,
		"[build]          Set %s=1 to fail hard, or build on Linux for a complete rootfs.\n",
		windowsRunStrictEnv)
	return nil
}

// isTruthyEnv returns true when the env var is set to a typical "yes"
// value. Anything else (including unset) is false.
func isTruthyEnv(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
