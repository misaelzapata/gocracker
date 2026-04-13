package guest

import (
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	cpiolib "github.com/cavaliergopher/cpio"
)

func TestEmbeddedInitDigest(t *testing.T) {
	digest := EmbeddedInitDigest()
	if len(digest) != 64 { // SHA256 hex
		t.Fatalf("digest length = %d, want 64", len(digest))
	}
}

func TestBuildInitrd(t *testing.T) {
	dir := t.TempDir()
	initrdPath := filepath.Join(dir, "initrd.img")
	if err := BuildInitrd(initrdPath, nil); err != nil {
		t.Fatalf("BuildInitrd: %v", err)
	}
	// Verify the file exists and is gzip
	f, err := os.Open(initrdPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	_, err = gzip.NewReader(f)
	if err != nil {
		t.Fatalf("not gzip: %v", err)
	}
}

func TestBuildInitrdWithExtraFiles(t *testing.T) {
	dir := t.TempDir()
	extraFile := filepath.Join(dir, "extra.bin")
	if err := os.WriteFile(extraFile, []byte("extra"), 0644); err != nil {
		t.Fatal(err)
	}

	initrdPath := filepath.Join(dir, "initrd.img")
	if err := BuildInitrd(initrdPath, map[string]string{"/opt/extra.bin": extraFile}); err != nil {
		t.Fatalf("BuildInitrd: %v", err)
	}
	entries := readInitrdEntries(t, initrdPath)
	if _, ok := entries["opt/extra.bin"]; !ok {
		t.Fatal("missing extra.bin in initrd")
	}
}

func TestSanitizeModuleName_WithExplicitName(t *testing.T) {
	got := sanitizeModuleName(KernelModule{Name: "mymod", HostPath: "/lib/modules/other.ko"})
	if got != "mymod" {
		t.Fatalf("sanitizeModuleName = %q, want mymod", got)
	}
}

func TestSanitizeModuleName_GzExtension(t *testing.T) {
	got := sanitizeModuleName(KernelModule{HostPath: "/lib/modules/test.ko.gz"})
	if got != "test" {
		t.Fatalf("sanitizeModuleName = %q, want test", got)
	}
}

func TestSanitizeModuleName_XzExtension(t *testing.T) {
	got := sanitizeModuleName(KernelModule{HostPath: "/lib/modules/test.ko.xz"})
	if got != "test" {
		t.Fatalf("sanitizeModuleName = %q, want test", got)
	}
}

func TestSanitizeModuleName_PlainKo(t *testing.T) {
	got := sanitizeModuleName(KernelModule{HostPath: "/lib/modules/simple.ko"})
	if got != "simple" {
		t.Fatalf("sanitizeModuleName = %q, want simple", got)
	}
}

func TestStageKernelModulesEmpty(t *testing.T) {
	root := t.TempDir()
	if err := stageKernelModules(root, nil); err != nil {
		t.Fatalf("stageKernelModules(nil) = %v", err)
	}
}

func TestStageKernelModulesEmptyName(t *testing.T) {
	root := t.TempDir()
	err := stageKernelModules(root, []KernelModule{{HostPath: ""}})
	if err == nil {
		t.Fatal("expected error for empty module name")
	}
}

func TestStageKernelModulesMissingHostPath(t *testing.T) {
	root := t.TempDir()
	err := stageKernelModules(root, []KernelModule{{Name: "test"}})
	if err == nil {
		t.Fatal("expected error for empty host path")
	}
}

func TestStageKernelModulesPlainKo(t *testing.T) {
	dir := t.TempDir()
	modPath := filepath.Join(dir, "test.ko")
	if err := os.WriteFile(modPath, []byte("module"), 0644); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "root")
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}
	if err := stageKernelModules(root, []KernelModule{{HostPath: modPath}}); err != nil {
		t.Fatalf("stageKernelModules: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(root, "lib", "modules", "gocracker", "test.ko"))
	if string(data) != "module" {
		t.Fatalf("module = %q", data)
	}
}

func TestStageRuntimeSpecNil(t *testing.T) {
	root := t.TempDir()
	if err := stageRuntimeSpec(root, nil); err != nil {
		t.Fatalf("stageRuntimeSpec(nil) = %v", err)
	}
}

func TestCopyKernelModulePlainKo(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "mod.ko")
	if err := os.WriteFile(src, []byte("payload"), 0644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "mod_copy.ko")
	if err := copyKernelModule(src, dst); err != nil {
		t.Fatalf("copyKernelModule: %v", err)
	}
	data, _ := os.ReadFile(dst)
	if string(data) != "payload" {
		t.Fatalf("content = %q", data)
	}
}

func TestBuildInitrdContainsExpectedFiles(t *testing.T) {
	dir := t.TempDir()
	initrdPath := filepath.Join(dir, "initrd.img")
	if err := BuildInitrd(initrdPath, nil); err != nil {
		t.Fatal(err)
	}
	entries := readInitrdEntries(t, initrdPath)
	// Should contain init, sbin/init, etc/hosts, etc/hostname, etc/resolv.conf
	for _, expected := range []string{"init", "sbin/init", "etc/hosts", "etc/hostname", "etc/resolv.conf", "etc/passwd"} {
		if _, ok := entries[expected]; !ok {
			t.Errorf("missing %s in initrd", expected)
		}
	}
}

func TestPackCpioGzWithSymlink(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "target.txt"), []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target.txt", filepath.Join(dir, "link.txt")); err != nil {
		t.Fatal(err)
	}

	output := filepath.Join(t.TempDir(), "test.cpio.gz")
	if err := packCpioGz(dir, output); err != nil {
		t.Fatalf("packCpioGz: %v", err)
	}
	
	// Read it back
	f, _ := os.Open(output)
	defer f.Close()
	gr, _ := gzip.NewReader(f)
	defer gr.Close()
	cr := cpiolib.NewReader(gr)
	found := false
	for {
		hdr, err := cr.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatal(err)
		}
		name := strings.TrimPrefix(hdr.Name, "./")
		if name == "link.txt" {
			found = true
		}
	}
	if !found {
		t.Fatal("symlink not found in cpio archive")
	}
}

func TestDecompressZstdFileMissing(t *testing.T) {
	err := decompressZstdFile("/nonexistent", "/tmp/out", 0644)
	if err == nil {
		t.Fatal("expected error for missing source")
	}
}
