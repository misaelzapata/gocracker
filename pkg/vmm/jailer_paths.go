package vmm

import "path/filepath"

// ResolveHostSidePath translates a file path that the VMM uses internally
// (e.g. VsockConfig.UDSPath) into the path a process outside the VMM must
// use to reach the same file on disk. When the VMM is jailed, files it
// creates at /foo actually live at <jailRoot>/foo on the host. When not
// jailed, the host path is identical to the internal path.
//
// jailRoot is WorkerMetadata.JailRoot ("" when not jailed).
func ResolveHostSidePath(jailRoot, guestPath string) string {
	if guestPath == "" {
		return ""
	}
	if jailRoot == "" {
		return guestPath
	}
	return filepath.Join(jailRoot, guestPath)
}
