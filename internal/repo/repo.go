// Package repo clones a git repository (or uses a local path) and
// locates the Dockerfile and docker-compose.yml inside it.
package repo

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gocracker/gocracker/internal/discovery"
)

// Source describes where to get the source code from.
type Source struct {
	// URL is a git remote URL: https://github.com/user/repo
	// or a local path: /home/user/myproject
	URL string
	// Branch or tag to checkout (default: default branch)
	Ref string
	// Subdir inside the repo where the Dockerfile lives (default: root)
	Subdir string
	// Depth for shallow clone (default: 1 — fastest)
	Depth int
}

// CloneResult is the result of cloning/resolving a repo.
type CloneResult struct {
	// Dir is the root of the cloned/resolved directory
	Dir string
	// ContextDir is Dir/Subdir — the build context
	ContextDir string
	// DockerfilePath is the located Dockerfile
	DockerfilePath string
	// ComposePath is the located docker-compose.yml (may be empty)
	ComposePath string
	// IsLocal is true when URL was a local path (no clone performed)
	IsLocal bool
	// cleanup removes the temp clone dir; no-op for local paths
	cleanup func()
}

// Cleanup removes the temporary clone directory if one was created.
func (r *CloneResult) Cleanup() {
	if r.cleanup != nil {
		r.cleanup()
	}
}

// Resolve either clones the repo or validates a local path.
// The caller must call result.Cleanup() when done.
func Resolve(src Source) (*CloneResult, error) {
	if src.Depth == 0 {
		src.Depth = 1
	}

	// Local path?
	if isLocalPath(src.URL) {
		return resolveLocal(src)
	}
	return cloneRemote(src)
}

func isLocalPath(url string) bool {
	return strings.HasPrefix(url, "/") ||
		strings.HasPrefix(url, "./") ||
		strings.HasPrefix(url, "../") ||
		url == "."
}

func resolveLocal(src Source) (*CloneResult, error) {
	abs, err := filepath.Abs(src.URL)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(abs); err != nil {
		return nil, fmt.Errorf("local path %s: %w", abs, err)
	}
	r := &CloneResult{
		Dir:     abs,
		IsLocal: true,
		cleanup: func() {}, // nothing to delete
	}
	r.ContextDir = filepath.Join(abs, src.Subdir)
	locateFiles(r, isExplicitSubdir(src.Subdir))
	return r, nil
}

// isExplicitSubdir returns true when the caller actually pointed the clone
// at a subdirectory inside the repo, as opposed to the repo root. "." and
// "./" both mean "repo root", so they should NOT force exactOnly lookup —
// that would skip Dockerfiles in subdirs (e.g. grafana/tempo's
// tools/Dockerfile) and break the historical regression gate.
func isExplicitSubdir(s string) bool {
	return s != "" && s != "." && s != "./"
}

func isHexString(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		case c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

func cloneRemote(src Source) (*CloneResult, error) {
	tmp, err := os.MkdirTemp("", "gocracker-repo-*")
	if err != nil {
		return nil, err
	}

	fmt.Printf("[repo] cloning %s", src.URL)
	if src.Ref != "" {
		fmt.Printf(" @ %s", src.Ref)
	}
	fmt.Println()

	t0 := time.Now()

	// Two-phase clone so src.Ref can be either a branch/tag *or* a commit SHA.
	// `git clone --branch` only accepts named refs, so we instead init an
	// empty repo, fetch the ref (named OR full SHA), then checkout FETCH_HEAD.
	// Direct SHA fetch needs uploadpack.allowReachableSHA1InWant on the server
	// AND a full 40-char SHA. For short SHAs (<40 chars) we fall back to a
	// shallow full-history clone and resolve the abbreviated SHA locally.
	if src.Ref != "" {
		runGit := func(args ...string) error {
			cmd := exec.Command("git", args...)
			cmd.Dir = tmp
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			return cmd.Run()
		}
		isFullSHA := len(src.Ref) == 40 && isHexString(src.Ref)
		isShortSHA := !isFullSHA && len(src.Ref) >= 4 && len(src.Ref) <= 39 && isHexString(src.Ref)

		if isShortSHA {
			// Plain clone (no --branch); we'll resolve and checkout below.
			// Fetch tags too so Dockerfiles that call `git describe --tags`
			// during the build (e.g. gitleaks, dex) find a reachable tag.
			cmd := exec.Command("git", "clone", "--quiet", src.URL, tmp)
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err != nil {
				os.RemoveAll(tmp)
				return nil, fmt.Errorf("git clone (for short SHA %s): %w", src.Ref, err)
			}
			if err := runGit("checkout", "--quiet", src.Ref); err != nil {
				os.RemoveAll(tmp)
				return nil, fmt.Errorf("git checkout %s: %w", src.Ref, err)
			}
		} else {
			if err := runGit("init", "--quiet"); err != nil {
				os.RemoveAll(tmp)
				return nil, fmt.Errorf("git init: %w", err)
			}
			if err := runGit("remote", "add", "origin", src.URL); err != nil {
				os.RemoveAll(tmp)
				return nil, fmt.Errorf("git remote add: %w", err)
			}
			fetchArgs := []string{"fetch", "--depth", fmt.Sprintf("%d", src.Depth), "origin", src.Ref}
			if err := runGit(fetchArgs...); err != nil {
				os.RemoveAll(tmp)
				return nil, fmt.Errorf("git fetch %s: %w", src.Ref, err)
			}
			if err := runGit("checkout", "--quiet", "FETCH_HEAD"); err != nil {
				os.RemoveAll(tmp)
				return nil, fmt.Errorf("git checkout %s: %w", src.Ref, err)
			}
			// Best-effort: fetch all tags AND unshallow so `git describe --tags`
			// can walk commit history to find a reachable tagged ancestor
			// (gitleaks, dex, lego, etc. embed the version via git describe).
			// Failures are ignored because some remotes restrict tag fetch.
			_ = runGit("fetch", "--unshallow", "--tags", "origin")
		}
	} else {
		// --no-single-branch not needed but --tags is critical so build-time
		// `git describe --tags` works.
		args := []string{"clone", "--depth", fmt.Sprintf("%d", src.Depth), "--single-branch", src.URL, tmp}
		cmd := exec.Command("git", args...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			os.RemoveAll(tmp)
			return nil, fmt.Errorf("git clone: %w", err)
		}
	}

	fmt.Printf("[repo] cloned in %s\n", time.Since(t0).Round(time.Millisecond))

	r := &CloneResult{
		Dir:     tmp,
		IsLocal: false,
		cleanup: func() { os.RemoveAll(tmp) },
	}
	if src.Subdir != "" && subdirPointsToDockerfile(filepath.Join(tmp, src.Subdir)) {
		// Subdir is a path to a Dockerfile, not a directory: split into
		// {Dockerfile, parent dir} so the build resolver can use it.
		df := filepath.Join(tmp, src.Subdir)
		r.DockerfilePath = df
		r.ContextDir = filepath.Dir(df)
		return r, nil
	}
	r.ContextDir = filepath.Join(tmp, src.Subdir)
	locateFiles(r, isExplicitSubdir(src.Subdir))
	return r, nil
}

// subdirPointsToDockerfile returns true if the given path is a regular file
// whose basename matches a known Dockerfile naming pattern.
func subdirPointsToDockerfile(p string) bool {
	info, err := os.Stat(p)
	if err != nil || info.IsDir() {
		return false
	}
	base := filepath.Base(p)
	return base == "Dockerfile" || strings.HasPrefix(base, "Dockerfile.") || strings.EqualFold(base, "dockerfile")
}

// locateFiles finds Dockerfile and docker-compose.yml inside r.ContextDir.
func locateFiles(r *CloneResult, exactOnly bool) {
	if exactOnly {
		locateFilesExact(r)
		return
	}
	searchRoot := r.ContextDir
	if dockerfile, _, err := discovery.FindDockerfile(searchRoot); err == nil {
		r.DockerfilePath = dockerfile
	}
	if composePath, err := discovery.FindCompose(searchRoot); err == nil {
		r.ComposePath = composePath
	}
}

func locateFilesExact(r *CloneResult) {
	// Dockerfile candidates in priority order
	for _, name := range []string{
		"Dockerfile",
		"dockerfile",
		"Dockerfile.prod",
		"Dockerfile.production",
	} {
		p := filepath.Join(r.ContextDir, name)
		if _, err := os.Stat(p); err == nil {
			r.DockerfilePath = p
			break
		}
	}

	// docker-compose candidates
	for _, name := range []string{
		"docker-compose.yml",
		"docker-compose.yaml",
		"compose.yml",
		"compose.yaml",
	} {
		p := filepath.Join(r.ContextDir, name)
		if _, err := os.Stat(p); err == nil {
			r.ComposePath = p
			break
		}
	}
}

// Summary prints what was found.
func (r *CloneResult) Summary() {
	fmt.Printf("[repo] context: %s\n", r.ContextDir)
	if r.DockerfilePath != "" {
		fmt.Printf("[repo] Dockerfile: %s\n", r.DockerfilePath)
	} else {
		fmt.Println("[repo] WARNING: no Dockerfile found")
	}
	if r.ComposePath != "" {
		fmt.Printf("[repo] Compose: %s\n", r.ComposePath)
	}
}
