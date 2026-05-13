// Package ext4 provides a pure-Go ext4 image builder. On Linux the
// host build path can still shell out to `mkfs.ext4` for speed, but
// on Windows there is no equivalent, so we ship a portable
// implementation via github.com/diskfs/go-diskfs.
//
// The shape mirrors what gocracker's existing image-building path
// needs: take a source directory (an unpacked OCI layer tree) plus a
// requested size, and produce a single ext4 disk image at a target
// path. Optionally embed an extra files map (used by the toolbox
// boot helper to drop /init and /etc/* without writing them into the
// source dir first).
//
// This package is intentionally cross-platform (no build tags). It
// brings go-diskfs into the dependency graph for every target,
// because the Windows port needs it; the Linux side can choose
// whether to call it or shell out per build.
package ext4

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/diskfs/go-diskfs"
	gdfsfilesystem "github.com/diskfs/go-diskfs/filesystem"
	"github.com/diskfs/go-diskfs/filesystem/ext4"
)

// BuildOptions configures BuildImage. All fields are optional.
type BuildOptions struct {
	// VolumeName is the ext4 label (max 16 bytes); empty means no label.
	VolumeName string

	// ExtraFiles is an absolute-path-keyed map of files to embed in the
	// image beyond what SourceDir contains. The path is the in-image
	// path (e.g. "/init"), with leading slash required.
	ExtraFiles map[string][]byte

	// ExtraFileMode is the permission bits applied to ExtraFiles. Zero
	// means 0o755.
	ExtraFileMode os.FileMode
}

// BuildImage creates an ext4 image at targetPath of exactly imageBytes
// in size, populated with the contents of sourceDir (recursively) plus
// any opts.ExtraFiles entries. sourceDir may be empty to produce an
// otherwise-empty image (useful for sparse rootfs scenarios).
//
// imageBytes must be at least 16 MiB (smaller and go-diskfs's ext4
// implementation refuses; smaller-than-necessary just yields a
// "no space left on device" error during copy).
//
// Returns an error if any path in sourceDir can't be opened or any
// write fails. On error, targetPath may be left half-built; the
// caller should delete it.
func BuildImage(targetPath string, imageBytes int64, sourceDir string, opts BuildOptions) error {
	if imageBytes < 16*1024*1024 {
		return fmt.Errorf("ext4 image must be ≥16 MiB; got %d bytes", imageBytes)
	}

	// Create the disk container — a flat file, default sector size.
	disk, err := diskfs.Create(targetPath, imageBytes, diskfs.SectorSizeDefault)
	if err != nil {
		return fmt.Errorf("diskfs.Create %s: %w", targetPath, err)
	}
	defer disk.Close()

	params := &ext4.Params{
		VolumeName: opts.VolumeName,
	}
	gdfs, err := ext4.Create(disk.Backend, imageBytes, 0, disk.LogicalBlocksize, params)
	if err != nil {
		return fmt.Errorf("ext4.Create: %w", err)
	}
	defer gdfs.Close()

	if sourceDir != "" {
		if err := copyTree(gdfs, sourceDir); err != nil {
			return fmt.Errorf("copy %s into image: %w", sourceDir, err)
		}
	}
	if len(opts.ExtraFiles) > 0 {
		mode := opts.ExtraFileMode
		if mode == 0 {
			mode = 0o755
		}
		if err := writeExtras(gdfs, opts.ExtraFiles, mode); err != nil {
			return fmt.Errorf("write extra files: %w", err)
		}
	}
	return nil
}

// copyTree walks sourceDir and re-creates everything in fs at the
// matching relative path. Directories first, then regular files; we
// honour file mode but not ownership (Linux uid/gid mappings differ
// across hosts — gocracker's runner adjusts ownership on the guest
// side).
func copyTree(targetFS *ext4.FileSystem, sourceDir string) error {
	sourceDir = filepath.Clean(sourceDir)
	return filepath.WalkDir(sourceDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(sourceDir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		// go-diskfs's ext4 Mkdir is anchored at the root and rejects
		// leading slashes ("invalid argument" otherwise). OpenFile,
		// confusingly, DOES accept them. We feed Mkdir the slash-less
		// form and keep guestPath with the leading slash for OpenFile.
		relSlash := filepath.ToSlash(rel)
		guestPath := "/" + relSlash
		if d.IsDir() {
			if err := targetFS.Mkdir(relSlash); err != nil {
				return fmt.Errorf("mkdir %s: %w", guestPath, err)
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			// Skip symlinks, devices, etc. — gocracker's existing image
			// pipeline turns these into regular-file placeholders too.
			return nil
		}
		if err := copyFile(targetFS, guestPath, path, info.Mode()); err != nil {
			return err
		}
		return nil
	})
}

// copyFile creates guestPath inside the ext4 image and streams the
// host file's bytes into it.
func copyFile(targetFS *ext4.FileSystem, guestPath, hostPath string, mode os.FileMode) error {
	src, err := os.Open(hostPath)
	if err != nil {
		return fmt.Errorf("open host %s: %w", hostPath, err)
	}
	defer src.Close()

	dst, err := targetFS.OpenFile(guestPath, os.O_CREATE|os.O_RDWR|os.O_TRUNC)
	if err != nil {
		return fmt.Errorf("create guest %s: %w", guestPath, err)
	}
	if _, err := io.Copy(asFileWriter(dst), src); err != nil {
		return fmt.Errorf("copy bytes to %s: %w", guestPath, err)
	}
	if err := targetFS.Chmod(guestPath, mode); err != nil {
		// Best-effort; non-fatal if the underlying filesystem doesn't
		// support mode bits (ext4 does).
		_ = err
	}
	return nil
}

// writeExtras drops the in-memory ExtraFiles entries into the image.
func writeExtras(targetFS *ext4.FileSystem, files map[string][]byte, mode os.FileMode) error {
	for path, data := range files {
		if !strings.HasPrefix(path, "/") {
			return fmt.Errorf("extra file path %q must be absolute (start with /)", path)
		}
		// Ensure parent dirs exist.
		if parent := filepath.Dir(filepath.FromSlash(path)); parent != "." && parent != "/" {
			_ = mkdirAll(targetFS, filepath.ToSlash(parent))
		}
		f, err := targetFS.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_TRUNC)
		if err != nil {
			return fmt.Errorf("create %s: %w", path, err)
		}
		if _, err := asFileWriter(f).Write(data); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
		_ = targetFS.Chmod(path, mode)
	}
	return nil
}

// mkdirAll creates every component of guestPath in the image, ignoring
// "already exists" errors. Mkdir wants the path without a leading
// slash; we feed it that form. "Already exists" doesn't have a typed
// error in go-diskfs, so we string-match it.
func mkdirAll(targetFS *ext4.FileSystem, guestPath string) error {
	guestPath = strings.TrimSuffix(guestPath, "/")
	if guestPath == "" || guestPath == "/" {
		return nil
	}
	parts := strings.Split(strings.TrimPrefix(guestPath, "/"), "/")
	var cur string
	for _, p := range parts {
		if cur == "" {
			cur = p
		} else {
			cur = cur + "/" + p
		}
		if err := targetFS.Mkdir(cur); err != nil {
			msg := err.Error()
			if strings.Contains(msg, "exist") || strings.Contains(msg, "EEXIST") {
				continue
			}
			return err
		}
	}
	return nil
}

// asFileWriter coerces a go-diskfs filesystem.File into io.Writer. The
// File interface is io.Reader + io.Writer + Seeker, so the assertion
// is safe; we keep it explicit for readability.
func asFileWriter(f gdfsfilesystem.File) io.Writer { return f }
