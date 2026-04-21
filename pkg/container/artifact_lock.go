package container

import "sync"

// Concurrent container.Run calls for the same image hit the same
// artifact workDir (keyed by the OCI layer digest + kernel hash +
// runtime config). Each call unconditionally does:
//
//   RemoveAll(rootfsDir)
//   MkdirAll(rootfsDir)
//   ExtractToDir(rootfsDir)          ← OCI layer apply
//   BuildExt4(rootfsDir, diskPath)   ← disk image
//   defer RemoveAll(rootfsDir)       ← staging cleanup
//
// Without coordination the defer cleanup of one goroutine shows up
// mid-extract of another and mkdir("rootfs/bin") fails with
// "no such file or directory". Caught by a 10-way sandboxd stress
// test; fix is a simple in-process keyed mutex so only one
// goroutine per workDir runs the critical region at a time.
// Subsequent callers hit the cached disk.ext4 via
// inspectCachedRunArtifacts on the fast path.
//
// Cross-process concurrency on the same workDir is still a foot-gun
// (two gocracker processes sharing a cache dir). Callers that care
// should use explicit flock upstream; for the single-process
// sandboxd case this is sufficient.

var (
	artifactLocksMu sync.Mutex
	artifactLocks   = map[string]*sync.Mutex{}
)

func lockArtifactDir(workDir string) func() {
	artifactLocksMu.Lock()
	m, ok := artifactLocks[workDir]
	if !ok {
		m = &sync.Mutex{}
		artifactLocks[workDir] = m
	}
	artifactLocksMu.Unlock()
	m.Lock()
	return m.Unlock
}
