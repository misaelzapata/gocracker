// Package warmcache provides a content-addressable on-disk cache of VM
// snapshots keyed by everything that must match at restore time. A cache HIT
// lets `gocracker run` skip the ~250 ms cold-boot path and take the ~3 ms
// MAP_PRIVATE COW restore path instead.
//
// A cache entry is a directory containing the two files the VMM writes:
//
//	<root>/<key>/snapshot.json
//	<root>/<key>/mem.bin
//
// The key is a hex SHA-256 of a canonical string that includes the image
// digest, kernel binary hash, kernel cmdline, memory size, vCPU count and
// arch. If any of those changes the cache misses — safely, because every
// one of them is frozen in the snapshot and would corrupt the guest if
// mismatched at restore time.
//
// The package is deliberately tiny and has no dependency on pkg/vmm so
// callers can compute keys cheaply before deciding whether to boot fresh.
package warmcache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// KeyInput is the set of inputs that must match between snapshot capture
// and restore for the cache hit to be safe. Any field that affects guest
// state captured in the snapshot MUST be included here — adding a new
// override to RestoreOptions without adding it here would let the cache
// hand out snapshots that corrupt the guest on restore.
type KeyInput struct {
	ImageDigest    string // OCI image digest or rootfs content hash
	KernelHash     string // sha256 of the vmlinux/bzImage bytes
	Cmdline        string // kernel command line
	MemMB          uint64
	VCPUs          int
	Arch           string // "amd64" | "arm64"
	NetworkMode    string // "" (none) | "auto" | "slirp" — affects virtio-net presence/backend in snapshot
	ToolboxVersion string // internal/toolbox/spec.Version baked into the disk
}

// Key returns the canonical hex SHA-256 of the input. Two KeyInputs with
// the same fields always produce the same key, regardless of map order or
// whitespace variation in the cmdline.
//
// Adding ToolboxVersion to the key (Fase 2) deliberately invalidates every
// pre-toolbox snapshot in the on-disk cache: their disks were built without
// /opt/gocracker/toolbox/toolboxguest, so restoring one would silently leave
// vsock 10023 dead. First post-merge run of any image cold-boots, recaptures
// with the agent baked in, and subsequent runs hit warm normally.
func Key(in KeyInput) string {
	parts := []string{
		"image=" + in.ImageDigest,
		"kernel=" + in.KernelHash,
		"cmdline=" + canonicalCmdline(in.Cmdline),
		fmt.Sprintf("mem=%d", in.MemMB),
		fmt.Sprintf("vcpus=%d", in.VCPUs),
		"arch=" + in.Arch,
		"net=" + in.NetworkMode,
		"toolbox=" + in.ToolboxVersion,
	}
	sort.Strings(parts)
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

// canonicalCmdline collapses whitespace and trims so trivial formatting
// differences don't bust the cache. Guest-visible cmdline semantics are
// preserved: order of tokens is kept, only runs of whitespace collapse.
func canonicalCmdline(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// Dir returns the filesystem path where a snapshot with the given key
// lives (or would live if written). The parent directory is not created.
func Dir(root, key string) string {
	return filepath.Join(root, key)
}

// Lookup returns the snapshot directory for key if it exists and is
// complete (both snapshot.json and mem.bin present and non-empty). The
// ok flag is false on any missing/partial entry; callers should treat
// that exactly like a cache miss.
func Lookup(root, key string) (dir string, ok bool) {
	d := Dir(root, key)
	if !isCompleteSnapshot(d) {
		return "", false
	}
	// Touch the directory mtime so Evict() keeps hot entries.
	now := time.Now()
	_ = os.Chtimes(d, now, now)
	return d, true
}

// Store atomically promotes a prepared snapshot directory (srcDir) into
// the cache under key. Semantics:
//
//   - srcDir must already contain snapshot.json + mem.bin (the caller is
//     expected to have pointed vmm.TakeSnapshot at it).
//   - If a cache entry for key already exists, Store leaves it in place
//     and removes srcDir — first writer wins, subsequent writers are
//     silently successful. This avoids a partial-write race if two
//     processes warm the same key concurrently.
//   - Store uses rename(2), which is atomic on the same filesystem. If
//     srcDir and root are on different filesystems the call fails and
//     the caller should place their scratch dir under root's parent.
func Store(srcDir, root, key string) error {
	if !isCompleteSnapshot(srcDir) {
		return fmt.Errorf("warmcache: src %s is not a complete snapshot", srcDir)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return fmt.Errorf("warmcache: create root: %w", err)
	}
	dst := Dir(root, key)
	if isCompleteSnapshot(dst) {
		// Another writer got there first. Drop our copy.
		_ = os.RemoveAll(srcDir)
		return nil
	}
	// Clean up any partial prior attempt before renaming in.
	_ = os.RemoveAll(dst)
	if err := os.Rename(srcDir, dst); err != nil {
		return fmt.Errorf("warmcache: rename %s -> %s: %w", srcDir, dst, err)
	}
	return nil
}

// Evict removes cache entries whose directory mtime is older than maxAge.
// Corrupted (incomplete) entries are also cleaned up unconditionally.
// Returns the number of entries removed.
func Evict(root string, maxAge time.Duration) (int, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	cutoff := time.Now().Add(-maxAge)
	removed := 0
	for _, e := range entries {
		if !e.Type().IsDir() {
			continue
		}
		p := filepath.Join(root, e.Name())
		if !isCompleteSnapshot(p) {
			_ = os.RemoveAll(p)
			removed++
			continue
		}
		if maxAge <= 0 {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			_ = os.RemoveAll(p)
			removed++
		}
	}
	return removed, nil
}

// DefaultRoot returns the on-disk location of the cache. It honours
// XDG_CACHE_HOME if set, otherwise falls back to $HOME/.cache.
// The returned path is not created; callers should MkdirAll before use.
func DefaultRoot() string {
	if x := os.Getenv("XDG_CACHE_HOME"); x != "" {
		return filepath.Join(x, "gocracker", "snapshots")
	}
	if h := os.Getenv("HOME"); h != "" {
		return filepath.Join(h, ".cache", "gocracker", "snapshots")
	}
	// Fallback for environments with neither set (e.g. tests, containers).
	return filepath.Join(os.TempDir(), "gocracker-snapshots")
}

// hashFileCache memoises HashFile results within a process keyed by
// (path, mtime, size). The kernel sha256 in particular is ~5–15 ms on
// a 35 MB ELF; computing it on every gocracker run is pure waste once
// the file is steady-state. The (mtime, size) tuple invalidates the
// entry the moment the kernel binary is rebuilt, which is the only
// real correctness concern — same trick the in-process artifact-cache
// invalidator uses.
type hashCacheKey struct {
	path  string
	mtime time.Time
	size  int64
}

var (
	hashCacheMu sync.Mutex
	hashCache   = map[hashCacheKey]string{}
)

// HashFile returns the hex SHA-256 of the file at path. Used to derive
// the KernelHash field of KeyInput — callers hand their kernel path to
// HashFile before computing Key.
//
// Two-layer cache:
//
//  1. In-memory (path, mtime, size) → hash. Helps daemons / repeat
//     callers within a process.
//  2. On-disk sidecar at <UserCacheDir>/gocracker/file-hashes/<sha-of-
//     abs-path>.json. Helps single-shot CLI invocations where layer 1
//     is always cold. Each entry stores mtime+size; we verify them
//     against the current stat before trusting the hash.
//
// On cold hit (no cache), pay ~5–15 ms hashing the file once and
// populate both layers.
func HashFile(path string) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	key := hashCacheKey{path: path, mtime: st.ModTime(), size: st.Size()}

	// Layer 1: in-memory.
	hashCacheMu.Lock()
	cached, ok := hashCache[key]
	hashCacheMu.Unlock()
	if ok {
		return cached, nil
	}

	// Layer 2: on-disk sidecar.
	if h, ok := loadDiskHash(path, st); ok {
		hashCacheMu.Lock()
		hashCache[key] = h
		hashCacheMu.Unlock()
		return h, nil
	}

	// Cold miss: compute + populate both layers.
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	out := hex.EncodeToString(h.Sum(nil))

	hashCacheMu.Lock()
	if cur, statErr := os.Stat(path); statErr == nil &&
		cur.ModTime().Equal(key.mtime) && cur.Size() == key.size {
		hashCache[key] = out
		// Best-effort disk write; failures (read-only home, EACCES,
		// disk full) silently fall through — we already have the hash.
		storeDiskHash(path, st, out)
	}
	hashCacheMu.Unlock()
	return out, nil
}

// hashSidecarPath returns the on-disk cache file location for path.
// We can't drop the sidecar next to the kernel (the kernel dir may be
// read-only or world-readable but not world-writable), so we land it
// under the user's cache dir keyed by sha256(abs-path) so two
// kernels at different paths don't collide.
func hashSidecarPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	keySum := sha256.Sum256([]byte(abs))
	keyHex := hex.EncodeToString(keySum[:])
	root, err := os.UserCacheDir()
	if err != nil {
		root = filepath.Join(os.TempDir(), "gocracker-cache")
	}
	return filepath.Join(root, "gocracker", "file-hashes", keyHex+".json"), nil
}

type diskHashEntry struct {
	Path      string `json:"path"`
	MtimeUnix int64  `json:"mtime_unix_ns"`
	Size      int64  `json:"size"`
	Hash      string `json:"hash"`
}

func loadDiskHash(path string, st os.FileInfo) (string, bool) {
	sidecar, err := hashSidecarPath(path)
	if err != nil {
		return "", false
	}
	data, err := os.ReadFile(sidecar)
	if err != nil {
		return "", false
	}
	var e diskHashEntry
	if err := json.Unmarshal(data, &e); err != nil {
		return "", false
	}
	if e.MtimeUnix != st.ModTime().UnixNano() || e.Size != st.Size() {
		return "", false
	}
	if len(e.Hash) != 64 {
		return "", false
	}
	return e.Hash, true
}

func storeDiskHash(path string, st os.FileInfo, hash string) {
	sidecar, err := hashSidecarPath(path)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(sidecar), 0o755); err != nil {
		return
	}
	abs, _ := filepath.Abs(path)
	e := diskHashEntry{
		Path:      abs,
		MtimeUnix: st.ModTime().UnixNano(),
		Size:      st.Size(),
		Hash:      hash,
	}
	data, err := json.Marshal(&e)
	if err != nil {
		return
	}
	tmp := sidecar + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, sidecar)
}

func isCompleteSnapshot(dir string) bool {
	si, err := os.Stat(filepath.Join(dir, "snapshot.json"))
	if err != nil || si.Size() == 0 {
		return false
	}
	mi, err := os.Stat(filepath.Join(dir, "mem.bin"))
	if err != nil || mi.Size() == 0 {
		return false
	}
	return true
}
