// Package tempprune sweeps stale gocracker-created temp directories that
// leak when parent processes get SIGKILL'd (sweep timeouts, container
// OOM-kills, manual Ctrl-C on `gocracker serve` / `gocracker repo` that
// doesn't reach the deferred Cleanup call).
//
// Every gocracker temp-dir creation site (internal/repo, internal/dockerfile,
// internal/worker, internal/api/migration) already does cleanup on the happy
// path. The pruner targets the unhappy path — long-running serve processes
// that inherit a pile of abandoned trees when they start up.
package tempprune

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DefaultPrefixes lists the /tmp/<prefix>-* patterns gocracker is known to
// leave behind. Keep in sync with the MkdirTemp call sites in:
//   - internal/dockerfile/dockerfile.go (stages, run-cache)
//   - internal/repo/repo.go (repo)
//   - internal/worker/vmm.go (vmm-worker, vmm-restore)
//   - internal/api/migration.go (migrate-src, migrate-dst)
var DefaultPrefixes = []string{
	"gocracker-stages",
	"gocracker-run-cache",
	"gocracker-repo",
	"gocracker-vmm-worker",
	"gocracker-vmm-restore",
	"gocracker-migrate-src",
	"gocracker-migrate-dst",
}

// PruneResult summarizes what PruneStaleTempDirs removed. Zero-valued on
// first startup with a clean /tmp; non-zero after a previous crash.
type PruneResult struct {
	Scanned   int
	Removed   int
	BytesFree int64
	Errors    []error
}

// PruneStaleTempDirs walks os.TempDir() looking for entries whose name
// starts with "<prefix>-" for any prefix in `prefixes`, and whose mtime
// is older than `maxAge`. Removes matches with os.RemoveAll. Fresher
// matches (potentially in-flight by a concurrent gocracker process) are
// left alone. Non-fatal: individual RemoveAll failures go into
// PruneResult.Errors but don't stop the walk.
//
// Intended call site: once at the start of `gocracker serve`, BEFORE the
// HTTP listener binds, so no active request can be holding a temp path.
func PruneStaleTempDirs(prefixes []string, maxAge time.Duration) PruneResult {
	var result PruneResult
	tmp := os.TempDir()
	entries, err := os.ReadDir(tmp)
	if err != nil {
		result.Errors = append(result.Errors, fmt.Errorf("read %s: %w", tmp, err))
		return result
	}
	cutoff := time.Now().Add(-maxAge)
	for _, e := range entries {
		name := e.Name()
		matched := false
		for _, pfx := range prefixes {
			if strings.HasPrefix(name, pfx+"-") {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		result.Scanned++
		full := filepath.Join(tmp, name)
		info, err := e.Info()
		if err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("stat %s: %w", full, err))
			continue
		}
		if info.ModTime().After(cutoff) {
			continue
		}
		size := dirSize(full)
		if err := os.RemoveAll(full); err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("remove %s: %w", full, err))
			continue
		}
		result.Removed++
		result.BytesFree += size
	}
	return result
}

// dirSize is a best-effort size accumulator. Failures are silent — the
// prune runs regardless of how accurate the reclaimed-bytes report is.
func dirSize(path string) int64 {
	var total int64
	_ = filepath.WalkDir(path, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		total += info.Size()
		return nil
	})
	return total
}
