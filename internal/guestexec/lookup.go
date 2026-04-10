package guestexec

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ResolveExecutable mimics PATH lookup using the provided environment instead
// of the current process environment. Paths that already contain a slash are
// returned unchanged so the child can resolve them relative to its own cwd.
func ResolveExecutable(file string, env []string) (string, error) {
	if file == "" {
		return "", fmt.Errorf("exec: empty executable")
	}
	if strings.Contains(file, "/") {
		return file, nil
	}

	pathValue := envValue(env, "PATH")
	if pathValue == "" {
		return "", notFoundError(file)
	}

	for _, dir := range strings.Split(pathValue, ":") {
		if dir == "" {
			dir = "."
		}
		candidate := filepath.Join(dir, file)
		info, err := os.Stat(candidate)
		if err != nil {
			continue
		}
		if info.IsDir() {
			continue
		}
		if info.Mode()&0o111 == 0 {
			continue
		}
		return candidate, nil
	}

	return "", notFoundError(file)
}

func envValue(env []string, key string) string {
	prefix := key + "="
	for i := len(env) - 1; i >= 0; i-- {
		if strings.HasPrefix(env[i], prefix) {
			return strings.TrimPrefix(env[i], prefix)
		}
	}
	return ""
}

func notFoundError(file string) error {
	return fmt.Errorf("exec: %q: executable file not found in $PATH", file)
}
