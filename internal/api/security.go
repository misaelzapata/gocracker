package api

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

func normalizeTrustedDirs(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		abs, err := filepath.Abs(value)
		if err != nil {
			continue
		}
		abs = filepath.Clean(abs)
		if _, ok := seen[abs]; ok {
			continue
		}
		seen[abs] = struct{}{}
		out = append(out, abs)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	if strings.TrimSpace(s.authToken) == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimSpace(r.Header.Get("Authorization"))
		want := "Bearer " + s.authToken
		if got != want {
			apiErr(w, http.StatusUnauthorized, "missing or invalid bearer token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) validateKernelPathForServer(path string) error {
	return validateServerPath(path, s.trustedKernelDirs, pathKindFile, "kernel_path")
}

func (s *Server) validateWorkPathForServer(path, label string) error {
	return validateServerPath(path, s.trustedWorkDirs, pathKindAny, label)
}

func (s *Server) validateSnapshotPathForServer(path string, mustExist bool) error {
	kind := pathKindAny
	if mustExist {
		kind = pathKindDir
	}
	return validateServerPath(path, s.trustedSnapshotDirs, kind, "snapshot_dir")
}

type validatedPathKind int

const (
	pathKindAny validatedPathKind = iota
	pathKindFile
	pathKindDir
)

func validateServerPath(value string, trustedRoots []string, kind validatedPathKind, label string) error {
	value = strings.TrimSpace(value)
	if value == "" || len(trustedRoots) == 0 {
		return nil
	}
	abs, err := filepath.Abs(value)
	if err != nil {
		return fmt.Errorf("%s: resolve path: %w", label, err)
	}
	resolved, err := resolvePathForContainment(abs)
	if err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	if !pathWithinTrustedRoots(resolved, trustedRoots) {
		return fmt.Errorf("%s: path %q is outside trusted directories", label, abs)
	}
	if kind == pathKindAny {
		return nil
	}
	info, err := os.Stat(abs)
	if err != nil {
		return fmt.Errorf("%s: stat %q: %w", label, abs, err)
	}
	switch kind {
	case pathKindFile:
		if info.IsDir() {
			return fmt.Errorf("%s: %q must be a file", label, abs)
		}
	case pathKindDir:
		if !info.IsDir() {
			return fmt.Errorf("%s: %q must be a directory", label, abs)
		}
	}
	return nil
}

func resolvePathForContainment(path string) (string, error) {
	path = filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return filepath.Clean(resolved), nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	parent := filepath.Dir(path)
	resolvedParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", fmt.Errorf("resolve parent %q: %w", parent, err)
	}
	return filepath.Join(filepath.Clean(resolvedParent), filepath.Base(path)), nil
}

func pathWithinTrustedRoots(path string, trustedRoots []string) bool {
	for _, root := range trustedRoots {
		if root == "" {
			continue
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			continue
		}
		if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))) {
			return true
		}
	}
	return false
}
