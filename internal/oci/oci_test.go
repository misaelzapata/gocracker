package oci

import (
	"archive/tar"
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

func TestRegistryPullLockPath_UsesCacheDirForDockerHub(t *testing.T) {
	ref, err := name.ParseReference("alpine:3.20")
	if err != nil {
		t.Fatalf("ParseReference(): %v", err)
	}
	cacheDir := filepath.Join(t.TempDir(), "oci-cache")
	got := registryPullLockPath(cacheDir, ref)
	want := filepath.Join(cacheDir, "dockerhub.lock")
	if got != want {
		t.Fatalf("registryPullLockPath() = %q, want %q", got, want)
	}
}

func TestRegistryPullLockPath_SkipsNonDockerHubRegistry(t *testing.T) {
	ref, err := name.ParseReference("ghcr.io/example/app:latest")
	if err != nil {
		t.Fatalf("ParseReference(): %v", err)
	}
	if got := registryPullLockPath(t.TempDir(), ref); got != "" {
		t.Fatalf("registryPullLockPath() = %q, want empty", got)
	}
}

// makeTar builds an in-memory tar archive from the given entries.
func makeTar(t *testing.T, entries []tarEntry) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		hdr := &tar.Header{
			Name:     e.Name,
			Typeflag: e.Type,
			Mode:     e.Mode,
			Size:     int64(len(e.Body)),
			Linkname: e.Linkname,
			Uid:      e.UID,
			Gid:      e.GID,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write tar header %q: %v", e.Name, err)
		}
		if len(e.Body) > 0 {
			if _, err := tw.Write([]byte(e.Body)); err != nil {
				t.Fatalf("write tar body %q: %v", e.Name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar writer: %v", err)
	}
	return &buf
}

type tarEntry struct {
	Name     string
	Type     byte
	Mode     int64
	Body     string
	Linkname string
	UID      int
	GID      int
}

func TestApplyTar_RegularFiles(t *testing.T) {
	dir := t.TempDir()
	buf := makeTar(t, []tarEntry{
		{Name: "hello.txt", Type: tar.TypeReg, Mode: 0644, Body: "hello world"},
		{Name: "subdir/nested.txt", Type: tar.TypeReg, Mode: 0644, Body: "nested content"},
	})
	if err := applyTar(dir, buf); err != nil {
		t.Fatalf("applyTar: %v", err)
	}

	// Verify hello.txt
	data, err := os.ReadFile(filepath.Join(dir, "hello.txt"))
	if err != nil {
		t.Fatalf("read hello.txt: %v", err)
	}
	if string(data) != "hello world" {
		t.Errorf("hello.txt = %q, want %q", data, "hello world")
	}

	// Verify subdir/nested.txt
	data, err = os.ReadFile(filepath.Join(dir, "subdir", "nested.txt"))
	if err != nil {
		t.Fatalf("read nested.txt: %v", err)
	}
	if string(data) != "nested content" {
		t.Errorf("nested.txt = %q, want %q", data, "nested content")
	}
}

func TestApplyTar_Directories(t *testing.T) {
	dir := t.TempDir()
	buf := makeTar(t, []tarEntry{
		{Name: "mydir/", Type: tar.TypeDir, Mode: 0755},
		{Name: "mydir/file.txt", Type: tar.TypeReg, Mode: 0644, Body: "in dir"},
	})
	if err := applyTar(dir, buf); err != nil {
		t.Fatalf("applyTar: %v", err)
	}

	fi, err := os.Stat(filepath.Join(dir, "mydir"))
	if err != nil {
		t.Fatalf("stat mydir: %v", err)
	}
	if !fi.IsDir() {
		t.Error("mydir should be a directory")
	}
}

func TestApplyTar_Symlinks(t *testing.T) {
	dir := t.TempDir()
	buf := makeTar(t, []tarEntry{
		{Name: "target.txt", Type: tar.TypeReg, Mode: 0644, Body: "target data"},
		{Name: "link.txt", Type: tar.TypeSymlink, Linkname: "target.txt"},
	})
	if err := applyTar(dir, buf); err != nil {
		t.Fatalf("applyTar: %v", err)
	}

	linkPath := filepath.Join(dir, "link.txt")
	dest, err := os.Readlink(linkPath)
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if dest != "target.txt" {
		t.Errorf("symlink target = %q, want %q", dest, "target.txt")
	}
}

// Regression: messense/rust-musl-cross ships symlinks like
// `usr/lib/llvm-14/build/Debug+Asserts -> ..`. A strict root-containment
// check on the resolved symlink target would wrongly reject these because
// filepath.Base("..") is ".." which joins above root. Symlink *files* are
// stored pointers resolved lazily by the guest; only the entry's write
// path needs containment, not the link's target.
func TestApplyTar_SymlinkToParentIsAllowed(t *testing.T) {
	dir := t.TempDir()
	buf := makeTar(t, []tarEntry{
		{Name: "usr/lib/llvm-14/build/", Type: tar.TypeDir, Mode: 0755},
		{Name: "usr/lib/llvm-14/build/Debug+Asserts", Type: tar.TypeSymlink, Linkname: ".."},
		{Name: "usr/lib/llvm-14/build/Release", Type: tar.TypeSymlink, Linkname: ".."},
	})
	if err := applyTar(dir, buf); err != nil {
		t.Fatalf("applyTar: %v", err)
	}
	for _, name := range []string{"Debug+Asserts", "Release"} {
		link := filepath.Join(dir, "usr/lib/llvm-14/build", name)
		dest, err := os.Readlink(link)
		if err != nil {
			t.Fatalf("readlink %s: %v", name, err)
		}
		if dest != ".." {
			t.Errorf("%s symlink target = %q, want %q", name, dest, "..")
		}
	}
}

func TestApplyTar_AbsoluteEntryStaysInsideRoot(t *testing.T) {
	dir := t.TempDir()
	buf := makeTar(t, []tarEntry{
		{Name: "/etc/passwd", Type: tar.TypeReg, Mode: 0644, Body: "safe"},
	})
	if err := applyTar(dir, buf); err != nil {
		t.Fatalf("applyTar: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "etc", "passwd"))
	if err != nil {
		t.Fatalf("read extracted file: %v", err)
	}
	if string(data) != "safe" {
		t.Fatalf("file content = %q, want %q", data, "safe")
	}
}

func TestApplyTar_FileUnderRelativeSymlinkParentStaysInsideRoot(t *testing.T) {
	dir := t.TempDir()
	buf := makeTar(t, []tarEntry{
		{Name: "var/", Type: tar.TypeDir, Mode: 0755},
		{Name: "var/lib", Type: tar.TypeSymlink, Linkname: "../../usr/lib"},
		{Name: "var/lib/app/config.txt", Type: tar.TypeReg, Mode: 0644, Body: "payload"},
	})
	if err := applyTar(dir, buf); err != nil {
		t.Fatalf("applyTar: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "usr", "lib", "app", "config.txt"))
	if err != nil {
		t.Fatalf("read redirected file: %v", err)
	}
	if string(data) != "payload" {
		t.Fatalf("file content = %q, want %q", data, "payload")
	}
}

func TestApplyTar_HardlinkArchiveRootRelative(t *testing.T) {
	dir := t.TempDir()
	buf := makeTar(t, []tarEntry{
		{Name: "usr/lib/python3.6/site-packages/__pycache__/", Type: tar.TypeDir, Mode: 0755},
		{Name: "usr/lib/python3.6/site-packages/__pycache__/easy_install.cpython-36.opt-1.pyc", Type: tar.TypeReg, Mode: 0644, Body: "payload"},
		{
			Name:     "usr/lib/python3.6/site-packages/__pycache__/easy_install.cpython-36.pyc",
			Type:     tar.TypeLink,
			Linkname: "usr/lib/python3.6/site-packages/__pycache__/easy_install.cpython-36.opt-1.pyc",
		},
	})
	if err := applyTar(dir, buf); err != nil {
		t.Fatalf("applyTar: %v", err)
	}

	orig := filepath.Join(dir, "usr", "lib", "python3.6", "site-packages", "__pycache__", "easy_install.cpython-36.opt-1.pyc")
	link := filepath.Join(dir, "usr", "lib", "python3.6", "site-packages", "__pycache__", "easy_install.cpython-36.pyc")

	origInfo, err := os.Stat(orig)
	if err != nil {
		t.Fatalf("stat orig: %v", err)
	}
	linkInfo, err := os.Stat(link)
	if err != nil {
		t.Fatalf("stat link: %v", err)
	}
	origStat, ok := origInfo.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("orig stat is %T, want *syscall.Stat_t", origInfo.Sys())
	}
	linkStat, ok := linkInfo.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("link stat is %T, want *syscall.Stat_t", linkInfo.Sys())
	}
	if origStat.Ino != linkStat.Ino {
		t.Fatalf("hardlink inode mismatch: orig=%d link=%d", origStat.Ino, linkStat.Ino)
	}
}

func TestApplyTar_HardlinkSameDirectoryRelative(t *testing.T) {
	dir := t.TempDir()
	buf := makeTar(t, []tarEntry{
		{Name: "subdir/", Type: tar.TypeDir, Mode: 0755},
		{Name: "subdir/original.txt", Type: tar.TypeReg, Mode: 0644, Body: "payload"},
		{Name: "subdir/link.txt", Type: tar.TypeLink, Linkname: "original.txt"},
	})
	if err := applyTar(dir, buf); err != nil {
		t.Fatalf("applyTar: %v", err)
	}

	orig := filepath.Join(dir, "subdir", "original.txt")
	link := filepath.Join(dir, "subdir", "link.txt")

	origInfo, err := os.Stat(orig)
	if err != nil {
		t.Fatalf("stat orig: %v", err)
	}
	linkInfo, err := os.Stat(link)
	if err != nil {
		t.Fatalf("stat link: %v", err)
	}
	origStat, ok := origInfo.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("orig stat is %T, want *syscall.Stat_t", origInfo.Sys())
	}
	linkStat, ok := linkInfo.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("link stat is %T, want *syscall.Stat_t", linkInfo.Sys())
	}
	if origStat.Ino != linkStat.Ino {
		t.Fatalf("hardlink inode mismatch: orig=%d link=%d", origStat.Ino, linkStat.Ino)
	}
}

func TestApplyTar_FileUnderAbsoluteSymlinkParentStaysInsideRoot(t *testing.T) {
	dir := t.TempDir()
	buf := makeTar(t, []tarEntry{
		{Name: "etc/", Type: tar.TypeDir, Mode: 0755},
		{Name: "etc/crypto-policies/", Type: tar.TypeDir, Mode: 0755},
		{Name: "etc/crypto-policies/back-ends", Type: tar.TypeSymlink, Linkname: "/usr/share/crypto-policies/back-ends"},
		{Name: "etc/crypto-policies/back-ends/nss.config", Type: tar.TypeReg, Mode: 0644, Body: "payload"},
	})
	if err := applyTar(dir, buf); err != nil {
		t.Fatalf("applyTar: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "usr", "share", "crypto-policies", "back-ends", "nss.config"))
	if err != nil {
		t.Fatalf("read redirected file: %v", err)
	}
	if string(data) != "payload" {
		t.Fatalf("file content = %q, want %q", data, "payload")
	}
	linkTarget, err := os.Readlink(filepath.Join(dir, "etc", "crypto-policies", "back-ends"))
	if err != nil {
		t.Fatalf("readlink(back-ends): %v", err)
	}
	if linkTarget != "/usr/share/crypto-policies/back-ends" {
		t.Fatalf("symlink target = %q, want %q", linkTarget, "/usr/share/crypto-policies/back-ends")
	}
}

func TestApplyTar_RegularFileReplacesAbsoluteSymlinkAcrossLayers(t *testing.T) {
	dir := t.TempDir()
	layer1 := makeTar(t, []tarEntry{
		{Name: "etc/", Type: tar.TypeDir, Mode: 0755},
		{Name: "etc/crypto-policies/", Type: tar.TypeDir, Mode: 0755},
		{Name: "etc/crypto-policies/back-ends/", Type: tar.TypeDir, Mode: 0755},
		{Name: "etc/crypto-policies/back-ends/nss.config", Type: tar.TypeSymlink, Linkname: "/usr/share/crypto-policies/DEFAULT/nss.txt"},
	})
	if err := applyTar(dir, layer1); err != nil {
		t.Fatalf("applyTar layer1: %v", err)
	}

	layer2 := makeTar(t, []tarEntry{
		{Name: "etc/crypto-policies/back-ends/nss.config", Type: tar.TypeReg, Mode: 0644, Body: "payload"},
	})
	if err := applyTar(dir, layer2); err != nil {
		t.Fatalf("applyTar layer2: %v", err)
	}

	target := filepath.Join(dir, "etc", "crypto-policies", "back-ends", "nss.config")
	info, err := os.Lstat(target)
	if err != nil {
		t.Fatalf("lstat target: %v", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("target should no longer be a symlink")
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if string(data) != "payload" {
		t.Fatalf("target content = %q, want %q", data, "payload")
	}
}

func TestApplyTar_Whiteout(t *testing.T) {
	dir := t.TempDir()

	// First layer: create a file
	buf1 := makeTar(t, []tarEntry{
		{Name: "remove_me.txt", Type: tar.TypeReg, Mode: 0644, Body: "doomed"},
		{Name: "keep_me.txt", Type: tar.TypeReg, Mode: 0644, Body: "safe"},
	})
	if err := applyTar(dir, buf1); err != nil {
		t.Fatalf("applyTar layer 1: %v", err)
	}

	// Second layer: whiteout remove_me.txt
	buf2 := makeTar(t, []tarEntry{
		{Name: ".wh.remove_me.txt", Type: tar.TypeReg, Mode: 0644},
	})
	if err := applyTar(dir, buf2); err != nil {
		t.Fatalf("applyTar layer 2: %v", err)
	}

	// remove_me.txt should be gone
	if _, err := os.Stat(filepath.Join(dir, "remove_me.txt")); !os.IsNotExist(err) {
		t.Error("remove_me.txt should have been removed by whiteout")
	}
	// keep_me.txt should still exist
	if _, err := os.Stat(filepath.Join(dir, "keep_me.txt")); err != nil {
		t.Error("keep_me.txt should still exist")
	}
}

func TestApplyTar_OpaqueWhiteout(t *testing.T) {
	dir := t.TempDir()

	// First layer: create a directory with files
	buf1 := makeTar(t, []tarEntry{
		{Name: "data/", Type: tar.TypeDir, Mode: 0755},
		{Name: "data/old1.txt", Type: tar.TypeReg, Mode: 0644, Body: "old1"},
		{Name: "data/old2.txt", Type: tar.TypeReg, Mode: 0644, Body: "old2"},
	})
	if err := applyTar(dir, buf1); err != nil {
		t.Fatalf("applyTar layer 1: %v", err)
	}

	// Second layer: opaque whiteout wipes the directory contents
	buf2 := makeTar(t, []tarEntry{
		{Name: "data/.wh..wh..opq", Type: tar.TypeReg, Mode: 0644},
		{Name: "data/new.txt", Type: tar.TypeReg, Mode: 0644, Body: "new"},
	})
	if err := applyTar(dir, buf2); err != nil {
		t.Fatalf("applyTar layer 2: %v", err)
	}

	// Old files should be gone
	if _, err := os.Stat(filepath.Join(dir, "data", "old1.txt")); !os.IsNotExist(err) {
		t.Error("old1.txt should have been removed by opaque whiteout")
	}
	if _, err := os.Stat(filepath.Join(dir, "data", "old2.txt")); !os.IsNotExist(err) {
		t.Error("old2.txt should have been removed by opaque whiteout")
	}
	// new.txt should exist
	data, err := os.ReadFile(filepath.Join(dir, "data", "new.txt"))
	if err != nil {
		t.Fatalf("read new.txt: %v", err)
	}
	if string(data) != "new" {
		t.Errorf("new.txt = %q, want %q", data, "new")
	}
}

func TestApplyTar_PathTraversal(t *testing.T) {
	dir := t.TempDir()

	// Entries with ".." paths should be skipped entirely
	buf := makeTar(t, []tarEntry{
		{Name: "../etc/passwd", Type: tar.TypeReg, Mode: 0644, Body: "pwned"},
		{Name: "../../escape.txt", Type: tar.TypeReg, Mode: 0644, Body: "escaped"},
		{Name: "safe.txt", Type: tar.TypeReg, Mode: 0644, Body: "okay"},
	})
	if err := applyTar(dir, buf); err != nil {
		t.Fatalf("applyTar: %v", err)
	}

	// The ".." entries should not have created files outside dir
	if _, err := os.Stat(filepath.Join(dir, "..", "etc", "passwd")); err == nil {
		t.Error("path traversal file should not exist")
	}
	// safe.txt should still be created
	if _, err := os.Stat(filepath.Join(dir, "safe.txt")); err != nil {
		t.Error("safe.txt should exist")
	}
}

func TestApplyTar_EmptyArchive(t *testing.T) {
	dir := t.TempDir()
	buf := makeTar(t, nil)
	if err := applyTar(dir, buf); err != nil {
		t.Fatalf("applyTar on empty archive: %v", err)
	}
}

func TestApplyTar_OverwriteFile(t *testing.T) {
	dir := t.TempDir()

	buf1 := makeTar(t, []tarEntry{
		{Name: "data.txt", Type: tar.TypeReg, Mode: 0644, Body: "version1"},
	})
	if err := applyTar(dir, buf1); err != nil {
		t.Fatalf("applyTar layer 1: %v", err)
	}

	buf2 := makeTar(t, []tarEntry{
		{Name: "data.txt", Type: tar.TypeReg, Mode: 0644, Body: "version2"},
	})
	if err := applyTar(dir, buf2); err != nil {
		t.Fatalf("applyTar layer 2: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "data.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "version2" {
		t.Errorf("data.txt = %q, want %q", data, "version2")
	}
}

func TestBuildExt4_ClearsReadonlyCompat(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "hello.txt"), []byte("hello"), 0644); err != nil {
		t.Fatalf("write rootfs file: %v", err)
	}

	image := filepath.Join(t.TempDir(), "disk.ext4")
	if err := BuildExt4(root, image, 64); err != nil {
		t.Fatalf("BuildExt4(): %v", err)
	}

	f, err := os.Open(image)
	if err != nil {
		t.Fatalf("open ext4 image: %v", err)
	}
	defer f.Close()

	var buf [4]byte
	if _, err := f.ReadAt(buf[:], ext4FeatureRoCompatOffset); err != nil {
		t.Fatalf("ReadAt ro_compat: %v", err)
	}
	features := binary.LittleEndian.Uint32(buf[:])
	if features&ext4RoCompatReadonly != 0 {
		t.Fatalf("ext4 ro_compat still has readonly bit set: %#x", features)
	}
}

func TestPullUsesLocalCache(t *testing.T) {
	cacheDir := t.TempDir()
	callCount := 0
	restore := remoteImage
	remoteImage = func(ref name.Reference, options ...remote.Option) (v1.Image, error) {
		callCount++
		return empty.Image, nil
	}
	defer func() { remoteImage = restore }()

	opts := PullOptions{
		Ref:      "ghcr.io/example/cache-test:latest",
		OS:       "linux",
		Arch:     "amd64",
		CacheDir: cacheDir,
	}

	first, err := Pull(opts)
	if err != nil {
		t.Fatalf("first Pull(): %v", err)
	}
	if first == nil {
		t.Fatal("first Pull() returned nil image")
	}
	second, err := Pull(opts)
	if err != nil {
		t.Fatalf("second Pull(): %v", err)
	}
	if second == nil {
		t.Fatal("second Pull() returned nil image")
	}
	if callCount != 1 {
		t.Fatalf("remote image fetch count = %d, want 1", callCount)
	}
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		t.Fatalf("ReadDir(cacheDir): %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("expected cache directory to contain image data")
	}
}

func TestBuildExt4_ExpandsWithinExistingGroups(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "hello.txt"), []byte("hello"), 0644); err != nil {
		t.Fatalf("write rootfs file: %v", err)
	}

	image := filepath.Join(t.TempDir(), "disk.ext4")
	if err := BuildExt4(root, image, 64); err != nil {
		t.Fatalf("BuildExt4(): %v", err)
	}

	f, err := os.Open(image)
	if err != nil {
		t.Fatalf("open ext4 image: %v", err)
	}
	defer f.Close()

	geo, err := readExt4Geometry(f)
	if err != nil {
		t.Fatalf("readExt4Geometry(): %v", err)
	}
	if geo.blockSize != 4096 {
		t.Fatalf("blockSize = %d, want 4096", geo.blockSize)
	}

	wantBlocks := uint32((64 * 1024 * 1024) / int(geo.blockSize))
	if geo.blocksCount != wantBlocks {
		t.Fatalf("blocksCount = %d, want %d", geo.blocksCount, wantBlocks)
	}
	if geo.blocksPerGroup != 32768 {
		t.Fatalf("blocksPerGroup = %d, want 32768", geo.blocksPerGroup)
	}
	if geo.freeBlocks < geo.blocksCount/2 {
		t.Fatalf("freeBlocks = %d, want at least %d", geo.freeBlocks, geo.blocksCount/2)
	}
}

func TestBuildExt4_GrowsAcrossAdditionalGroups(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "hello.txt"), []byte("hello"), 0644); err != nil {
		t.Fatalf("write rootfs file: %v", err)
	}

	image := filepath.Join(t.TempDir(), "disk.ext4")
	if err := BuildExt4(root, image, 256); err != nil {
		t.Fatalf("BuildExt4(): %v", err)
	}

	f, err := os.Open(image)
	if err != nil {
		t.Fatalf("open ext4 image: %v", err)
	}
	defer f.Close()

	geo, err := readExt4Geometry(f)
	if err != nil {
		t.Fatalf("readExt4Geometry(): %v", err)
	}

	wantBlocks := uint32((256 * 1024 * 1024) / int(geo.blockSize))
	if geo.blocksCount != wantBlocks {
		t.Fatalf("blocksCount = %d, want %d", geo.blocksCount, wantBlocks)
	}
	if got := blocksToGroups(geo.blocksCount, geo.blocksPerGroup); got < 2 {
		t.Fatalf("group count = %d, want at least 2", got)
	}
	if geo.freeBlocks < geo.blocksCount/2 {
		t.Fatalf("freeBlocks = %d, want at least %d", geo.freeBlocks, geo.blocksCount/2)
	}
	if geo.inodesCount <= geo.inodesPerGroup {
		t.Fatalf("inodesCount = %d, want more than one group of inodes (%d)", geo.inodesCount, geo.inodesPerGroup)
	}
}

func TestApplyTar_PreservesOwnership(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to verify OCI layer ownership restoration")
	}

	dir := t.TempDir()
	buf := makeTar(t, []tarEntry{
		{Name: "owned/", Type: tar.TypeDir, Mode: 0755, UID: 123, GID: 456},
		{Name: "owned/file.txt", Type: tar.TypeReg, Mode: 0640, Body: "payload", UID: 123, GID: 456},
	})
	if err := applyTar(dir, buf); err != nil {
		t.Fatalf("applyTar: %v", err)
	}

	assertOwnership := func(path string, wantUID, wantGID uint32) {
		t.Helper()
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatalf("Lstat(%s): %v", path, err)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			t.Fatalf("stat(%s): unexpected type %T", path, info.Sys())
		}
		if stat.Uid != wantUID || stat.Gid != wantGID {
			t.Fatalf("%s ownership = %d:%d, want %d:%d", path, stat.Uid, stat.Gid, wantUID, wantGID)
		}
	}

	assertOwnership(filepath.Join(dir, "owned"), 123, 456)
	assertOwnership(filepath.Join(dir, "owned", "file.txt"), 123, 456)
}
