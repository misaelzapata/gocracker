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
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// KeyInput is the set of inputs that must match between snapshot capture
// and restore for the cache hit to be safe. Any field that affects guest
// state captured in the snapshot MUST be included here — adding a new
// override to RestoreOptions without adding it here would let the cache
// hand out snapshots that corrupt the guest on restore.
type KeyInput struct {
	ImageDigest string // OCI image digest or rootfs content hash
	KernelHash  string // sha256 of the vmlinux/bzImage bytes
	Cmdline     string // kernel command line
	MemMB       uint64
	VCPUs       int
	Arch        string // "amd64" | "arm64"
}

// Key returns the canonical hex SHA-256 of the input. Two KeyInputs with
// the same fields always produce the same key, regardless of map order or
// whitespace variation in the cmdline.
func Key(in KeyInput) string {
	parts := []string{
		"image=" + in.ImageDigest,
		"kernel=" + in.KernelHash,
		"cmdline=" + canonicalCmdline(in.Cmdline),
		fmt.Sprintf("mem=%d", in.MemMB),
		fmt.Sprintf("vcpus=%d", in.VCPUs),
		"arch=" + in.Arch,
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

// HashFile returns the hex SHA-256 of the file at path. Used to derive
// the KernelHash field of KeyInput — callers hand their kernel path to
// HashFile before computing Key.
func HashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
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
