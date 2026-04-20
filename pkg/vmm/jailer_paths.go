package vmm

import (
	"path/filepath"
	"strings"
)

// WorkerJailBindMount is the path inside the jail where the worker's
// per-VM run directory is bind-mounted. See internal/worker/vmm.go.
const WorkerJailBindMount = "/worker"

// ResolveHostSidePath translates a file path that the VMM uses internally
// (e.g. VsockConfig.UDSPath) into the path a process outside the VMM must
// use to reach the same file on disk. When the VMM is jailed, files it
// creates at /foo actually live at <jailRoot>/foo on the host. When not
// jailed, the host path is identical to the internal path.
//
// guestPath is cleaned before use and joined as a RELATIVE path under
// jailRoot so that traversal (e.g. "/../etc/passwd") cannot escape the
// chroot in the host-side result. jailRoot is WorkerMetadata.JailRoot
// ("" when not jailed).
func ResolveHostSidePath(jailRoot, guestPath string) string {
	if guestPath == "" {
		return ""
	}
	clean := filepath.Clean(guestPath)
	if jailRoot == "" {
		return clean
	}
	// TrimLeft so filepath.Join treats clean as relative; otherwise Join
	// followed by path traversal would produce a path outside jailRoot.
	return filepath.Join(jailRoot, strings.TrimLeft(clean, string(filepath.Separator)))
}

// ResolveWorkerHostSidePath is jailer-aware AND worker-bind-mount aware.
// The jailer runs the VMM in a private mount namespace where /worker is
// bind-mounted from WorkerMetadata.RunDir. Files the VMM creates under
// /worker are visible on the host at <RunDir>/..., not at
// <JailRoot>/worker/... (the latter is hidden by the bind). Non-bind
// paths fall back to ResolveHostSidePath.
//
// In jailer-off mode the guest path is the host path.
func ResolveWorkerHostSidePath(meta WorkerMetadata, guestPath string) string {
	if guestPath == "" {
		return ""
	}
	// Clean first so "/worker/../x" resolves to "/x" and does NOT match
	// the /worker prefix (would otherwise escape RunDir on the host).
	clean := filepath.Clean(guestPath)
	if meta.JailRoot == "" {
		return clean
	}
	if meta.RunDir != "" {
		prefix := WorkerJailBindMount + "/"
		if strings.HasPrefix(clean, prefix) {
			return filepath.Join(meta.RunDir, strings.TrimPrefix(clean, WorkerJailBindMount))
		}
		if clean == WorkerJailBindMount {
			return meta.RunDir
		}
	}
	return filepath.Join(meta.JailRoot, strings.TrimLeft(clean, string(filepath.Separator)))
}
