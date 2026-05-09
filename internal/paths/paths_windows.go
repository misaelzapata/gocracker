//go:build windows

package paths

import (
	"os"
	"path/filepath"
)

// localAppData returns %LOCALAPPDATA%\gocracker, falling back to a temp
// directory if the environment variable is missing (rare, but possible
// inside a service running with an unloaded user profile).
func localAppData() string {
	if base := os.Getenv("LOCALAPPDATA"); base != "" {
		return filepath.Join(base, "gocracker")
	}
	return filepath.Join(os.TempDir(), "gocracker")
}

// programData returns %PROGRAMDATA%\gocracker. Falls back to LocalAppData
// if PROGRAMDATA is missing (extremely unusual).
func programData() string {
	if base := os.Getenv("PROGRAMDATA"); base != "" {
		return filepath.Join(base, "gocracker")
	}
	return localAppData()
}

func apiSocket() string {
	return filepath.Join(localAppData(), "sock", "api.sock")
}

func vmmSocket() string {
	return filepath.Join(localAppData(), "sock", "vmm.sock")
}

func buildSocket() string {
	return filepath.Join(localAppData(), "sock", "build.sock")
}

func serveStateDir() string {
	return filepath.Join(localAppData(), "state")
}

func snapshotsDir() string {
	return filepath.Join(localAppData(), "snapshots")
}

func jailerBaseDir() string {
	return filepath.Join(programData(), "jailer")
}

func toolboxLog() string {
	return filepath.Join(os.TempDir(), "gocracker-toolbox.log")
}

func gitConfigGlobal() string {
	return filepath.Join(os.TempDir(), "gocracker-gitconfig")
}
