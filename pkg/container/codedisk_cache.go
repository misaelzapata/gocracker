package container

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// CodeDiskCache is a content-addressed cache of built ext4 code-disk
// images. The intended use is: caller hashes a source directory (the
// user's app code), looks the hash up in the cache, and either reuses
// the existing ext4 image or builds a fresh one and stores it.
//
// Keying covers everything that influences the resulting image:
//
//   - the SHA-256 of every file's path + size + mode + content
//   - the requested filesystem type (default "ext4")
//   - the read-only flag (image content is identical, but we want
//     distinct cache entries so callers can reason about RO vs RW
//     reuse)
//
// Entries are plain files at <root>/<hash>-<fs>-<ro|rw>.<fs>; lookup
// is O(1) on the path, no manifest. Eviction is the caller's
// responsibility for now (drop the file or rmtree the root). When
// Phase 3 lands we expect sandboxd to drive eviction with an LRU.
type CodeDiskCache struct {
	root string
}

// NewCodeDiskCache creates a cache rooted at the given directory.
// The directory is created with 0o755 if missing. Returns the cache
// or an error if the directory could not be set up.
func NewCodeDiskCache(root string) (*CodeDiskCache, error) {
	if root == "" {
		return nil, fmt.Errorf("CodeDiskCache: root path required")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("CodeDiskCache: mkdir %s: %w", root, err)
	}
	return &CodeDiskCache{root: root}, nil
}

// Root returns the cache root directory.
func (c *CodeDiskCache) Root() string { return c.root }

// HashSourceDir computes the cache key for the given source directory.
// Walks the dir, hashing each entry's relative path, mode bits,
// regular-file size, and (for regular files) content. Symlinks are
// hashed as their target path (no follow). Returns a hex digest.
//
// Empty or non-existent dir yields a stable digest of just the
// fsType + readOnly flags so callers see a non-empty key.
func HashSourceDir(srcDir, fsType string, readOnly bool) (string, error) {
	h := sha256.New()
	fmt.Fprintf(h, "fs=%s\nro=%t\n---\n", normalizeFSType(fsType), readOnly)
	if srcDir == "" {
		return hex.EncodeToString(h.Sum(nil)), nil
	}
	// Collect entries with relative paths so the digest is stable
	// regardless of the absolute prefix.
	type entry struct {
		rel  string
		info fs.FileInfo
	}
	var entries []entry
	root, err := filepath.Abs(srcDir)
	if err != nil {
		return "", fmt.Errorf("hash: abs %s: %w", srcDir, err)
	}
	if st, err := os.Stat(root); err != nil {
		if os.IsNotExist(err) {
			return hex.EncodeToString(h.Sum(nil)), nil
		}
		return "", err
	} else if !st.IsDir() {
		return "", fmt.Errorf("hash: %s is not a directory", root)
	}
	walkErr := filepath.Walk(root, func(path string, info fs.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		if rel == "." {
			return nil
		}
		entries = append(entries, entry{rel: filepath.ToSlash(rel), info: info})
		return nil
	})
	if walkErr != nil {
		return "", fmt.Errorf("hash: walk %s: %w", root, walkErr)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].rel < entries[j].rel })
	for _, e := range entries {
		mode := uint32(e.info.Mode())
		fmt.Fprintf(h, "%s\n%s\n%d\n", e.rel, modeString(mode), e.info.Size())
		switch {
		case e.info.Mode()&os.ModeSymlink != 0:
			target, lerr := os.Readlink(filepath.Join(root, filepath.FromSlash(e.rel)))
			if lerr != nil {
				return "", fmt.Errorf("hash: readlink %s: %w", e.rel, lerr)
			}
			fmt.Fprintf(h, "symlink:%s\n", target)
		case e.info.Mode().IsRegular():
			f, ferr := os.Open(filepath.Join(root, filepath.FromSlash(e.rel)))
			if ferr != nil {
				return "", fmt.Errorf("hash: open %s: %w", e.rel, ferr)
			}
			if _, copyErr := io.Copy(h, f); copyErr != nil {
				_ = f.Close()
				return "", fmt.Errorf("hash: read %s: %w", e.rel, copyErr)
			}
			_ = f.Close()
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// CachePath returns the on-disk path the cache would use for the given
// hash + fs + readOnly tuple. The file may or may not exist; use
// Lookup if you only want a hit.
func (c *CodeDiskCache) CachePath(hash, fsType string, readOnly bool) string {
	mode := "rw"
	if readOnly {
		mode = "ro"
	}
	fs := normalizeFSType(fsType)
	return filepath.Join(c.root, fmt.Sprintf("%s-%s-%s.%s", hash, fs, mode, fs))
}

// Lookup returns (path, true, nil) on cache hit; (path, false, nil) on
// cache miss (path is where the entry would live). Errors are returned
// only on real I/O problems (e.g. stat fails for a reason other than
// not-exist).
func (c *CodeDiskCache) Lookup(hash, fsType string, readOnly bool) (string, bool, error) {
	path := c.CachePath(hash, fsType, readOnly)
	if st, err := os.Stat(path); err == nil {
		// Reject zero-byte sentinels left over from interrupted
		// builds — the next caller will rebuild instead of mounting
		// an empty image.
		if st.Size() == 0 {
			_ = os.Remove(path)
			return path, false, nil
		}
		return path, true, nil
	} else if !os.IsNotExist(err) {
		return path, false, err
	}
	return path, false, nil
}

// Store atomically copies srcImage into the cache slot for the given
// hash/fs/readOnly. Returns the cached path. Uses tmp+rename so a
// concurrent reader never observes a half-written file.
func (c *CodeDiskCache) Store(hash, fsType string, readOnly bool, srcImage string) (string, error) {
	target := c.CachePath(hash, fsType, readOnly)
	tmp, err := os.CreateTemp(c.root, ".tmp-codedisk-*")
	if err != nil {
		return "", fmt.Errorf("store: tmp: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }
	src, err := os.Open(srcImage)
	if err != nil {
		_ = tmp.Close()
		cleanup()
		return "", fmt.Errorf("store: open %s: %w", srcImage, err)
	}
	if _, err := io.Copy(tmp, src); err != nil {
		_ = src.Close()
		_ = tmp.Close()
		cleanup()
		return "", fmt.Errorf("store: copy: %w", err)
	}
	_ = src.Close()
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return "", fmt.Errorf("store: sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return "", fmt.Errorf("store: close: %w", err)
	}
	if err := os.Rename(tmpPath, target); err != nil {
		cleanup()
		return "", fmt.Errorf("store: rename %s -> %s: %w", tmpPath, target, err)
	}
	return target, nil
}

// Evict removes the cache entry for the given key. Returns true if a
// file was removed, false if it was already absent.
func (c *CodeDiskCache) Evict(hash, fsType string, readOnly bool) (bool, error) {
	path := c.CachePath(hash, fsType, readOnly)
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func normalizeFSType(fs string) string {
	fs = strings.TrimSpace(fs)
	if fs == "" {
		return "ext4"
	}
	return fs
}

// modeString renders the file mode as a stable string for hashing.
// Using strconv keeps it deterministic across Go versions.
func modeString(m uint32) string {
	return strconv.FormatUint(uint64(m), 8)
}
