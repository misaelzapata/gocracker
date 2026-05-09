// Package paths centralises every host filesystem location gocracker uses
// at runtime. Linux defaults are byte-identical to the hard-coded strings
// they replace (e.g. /tmp/gocracker.sock); Windows defaults rooted under
// %LOCALAPPDATA%\gocracker (per-user) and %PROGRAMDATA%\gocracker (machine)
// follow Microsoft's app-data conventions.
//
// Callers should prefer these helpers over literals so the Windows port
// can override per-platform behaviour in one place.
package paths

import (
	"os"
	"path/filepath"
)

// APISocket returns the default Unix socket path for `gocracker serve`.
func APISocket() string { return apiSocket() }

// VMMSocket returns the default Unix socket path for `gocracker-vmm`.
func VMMSocket() string { return vmmSocket() }

// BuildSocket returns the default Unix socket path for `gocracker build-worker`.
func BuildSocket() string { return buildSocket() }

// ServeStateDir returns the default persistent state directory used by the
// supervisor for `gocracker serve`.
func ServeStateDir() string { return serveStateDir() }

// SnapshotsDir returns the default scratch directory for snapshot bundles
// produced by `/snapshots/save` API calls when no destination is supplied.
func SnapshotsDir() string { return snapshotsDir() }

// CacheDir returns the default OCI / build cache directory. Implementation
// uses os.TempDir() so it works identically on every platform.
func CacheDir() string { return filepath.Join(os.TempDir(), "gocracker", "cache") }

// JailerBaseDir returns the default jailer chroot base directory on Linux.
// On Windows there is no chroot, so this points at the equivalent
// per-instance working-directory base used by internal/winsandbox.
func JailerBaseDir() string { return jailerBaseDir() }

// ToolboxLog returns the default log path for `gocracker-toolbox spawn`.
// Note that the toolbox runs inside the (always-Linux) guest, so this is
// only relevant when cross-compiling host-side toolchains.
func ToolboxLog() string { return toolboxLog() }

// GitConfigGlobal returns the default GIT_CONFIG_GLOBAL path injected into
// Dockerfile RUN steps when the user has not set one.
func GitConfigGlobal() string { return gitConfigGlobal() }

// TempPrefix returns the leading filename component used by orphan-temp
// pruning on `serve` startup. Linux uses "gocracker-" so the existing
// /tmp/gocracker-* matchers continue to work.
func TempPrefix() string { return "gocracker-" }
