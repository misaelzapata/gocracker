package container

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func applyMounts(rootfsDir string, mounts []Mount) error {
	for _, mount := range mounts {
		if mount.Source == "" || mount.Target == "" {
			return fmt.Errorf("mount source and target are required")
		}
		info, err := os.Stat(mount.Source)
		if err != nil {
			return fmt.Errorf("stat mount source %s: %w", mount.Source, err)
		}

		target := rootfsPath(rootfsDir, mount.Target)
		if mount.Populate {
			if err := populateMountSource(rootfsDir, mount.Source, mount.Target, info.IsDir()); err != nil {
				return err
			}
			info, err = os.Stat(mount.Source)
			if err != nil {
				return fmt.Errorf("restat populated mount source %s: %w", mount.Source, err)
			}
		}
		if mount.Backend == MountBackendVirtioFS {
			if !info.IsDir() {
				return fmt.Errorf("virtio-fs mount source %s must be a directory", mount.Source)
			}
			continue
		}

		if err := os.RemoveAll(target); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove mount target %s: %w", target, err)
		}
		if info.IsDir() {
			if err := copyDir(mount.Source, target); err != nil {
				return fmt.Errorf("copy dir mount %s -> %s: %w", mount.Source, target, err)
			}
		} else {
			if err := copyFileOrSymlink(mount.Source, target); err != nil {
				return fmt.Errorf("copy file mount %s -> %s: %w", mount.Source, target, err)
			}
		}
	}
	return nil
}

func populateMountSource(rootfsDir, source, target string, sourceIsDir bool) error {
	entries, err := os.ReadDir(source)
	if err == nil && len(entries) > 0 {
		return nil
	}
	if err != nil && !os.IsNotExist(err) && !sourceIsDir {
		if _, statErr := os.Stat(source); statErr == nil {
			return nil
		}
	}

	guestPath := rootfsPath(rootfsDir, target)
	info, err := os.Lstat(guestPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	if sourceIsDir {
		if err := os.MkdirAll(source, 0755); err != nil {
			return err
		}
		if info.IsDir() {
			return copyDirContents(guestPath, source)
		}
		return copyFileOrSymlink(guestPath, filepath.Join(source, filepath.Base(guestPath)))
	}
	if info.IsDir() {
		return nil
	}
	return copyFileOrSymlink(guestPath, source)
}

func copyDir(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, info.Mode().Perm()); err != nil {
		return err
	}
	return copyDirContents(src, dst)
}

func copyDirContents(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())
		if entry.IsDir() {
			if err := copyDir(srcPath, dstPath); err != nil {
				return err
			}
			continue
		}
		if err := copyFileOrSymlink(srcPath, dstPath); err != nil {
			return err
		}
	}
	return nil
}

func copyFileOrSymlink(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if shouldSkipSpecialFile(info.Mode()) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		link, err := os.Readlink(src)
		if err != nil {
			return err
		}
		_ = os.RemoveAll(dst)
		return os.Symlink(link, dst)
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return nil
}

func shouldSkipSpecialFile(mode os.FileMode) bool {
	return !mode.IsRegular() && mode&os.ModeSymlink == 0 && !mode.IsDir()
}

func rootfsPath(rootfs, path string) string {
	cleaned := filepath.Clean(path)
	if cleaned == "." || cleaned == string(filepath.Separator) {
		return rootfs
	}
	return filepath.Join(rootfs, strings.TrimPrefix(cleaned, string(filepath.Separator)))
}
