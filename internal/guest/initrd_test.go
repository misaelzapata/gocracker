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
	"github.com/gocracker/gocracker/internal/runtimecfg"
	"github.com/klauspost/compress/zstd"
)

func TestEnvToKernelCmdline_Empty(t *testing.T) {
	got := EnvToKernelCmdline(nil)
	if got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}

func TestEnvToKernelCmdline_SingleVar(t *testing.T) {
	got := EnvToKernelCmdline([]string{"HOME=/root"})
	if got == "" {
		t.Fatal("expected structured env fragment, got empty string")
	}
	if !strings.Contains(got, "gc.format=2") || !strings.Contains(got, "gc.env.0=") {
		t.Errorf("got %q, want structured env fragment", got)
	}
}

func TestEnvToKernelCmdline_MultipleVars(t *testing.T) {
	got := EnvToKernelCmdline([]string{"FOO=bar", "BAZ=qux"})
	if !strings.Contains(got, "gc.env.0=") || !strings.Contains(got, "gc.env.1=") {
		t.Errorf("got %q, want two encoded env entries", got)
	}
}

func TestEnvToKernelCmdline_SpaceEscaping(t *testing.T) {
	got := EnvToKernelCmdline([]string{"MSG=hello world"})
	if strings.Contains(got, "hello world") {
		t.Errorf("got %q, want encoded env value", got)
	}
}

func TestEnvToKernelCmdline_EmptyValue(t *testing.T) {
	got := EnvToKernelCmdline([]string{"EMPTY="})
	if !strings.Contains(got, "gc.env.0=") {
		t.Errorf("got %q, want encoded env entry", got)
	}
}

func TestCopyFile_Success(t *testing.T) {
	dir := t.TempDir()
	srcPath := dir + "/source.txt"
	dstPath := dir + "/dest.txt"

	content := []byte("test file content for copy")
	if err := os.WriteFile(srcPath, content, 0644); err != nil {
		t.Fatal(err)
	}

	if err := copyFile(srcPath, dstPath, 0755); err != nil {
		t.Fatalf("copyFile: %v", err)
	}

	data, err := os.ReadFile(dstPath)
	if err != nil {
		t.Fatalf("read dest: %v", err)
	}
	if string(data) != string(content) {
		t.Errorf("content mismatch: got %q, want %q", data, content)
	}

	// Verify permissions
	fi, err := os.Stat(dstPath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0755 {
		t.Errorf("perm = %o, want 0755", fi.Mode().Perm())
	}
}

func TestCopyFile_MissingSrc(t *testing.T) {
	dir := t.TempDir()
	err := copyFile(dir+"/nonexistent", dir+"/dest", 0644)
	if err == nil {
		t.Fatal("expected error for missing source")
	}
}

func TestCopyFile_InvalidDst(t *testing.T) {
	dir := t.TempDir()
	srcPath := dir + "/source.txt"
	os.WriteFile(srcPath, []byte("data"), 0644)

	err := copyFile(srcPath, "/nonexistent-dir/sub/dest", 0644)
	if err == nil {
		t.Fatal("expected error for invalid destination path")
	}
}

func TestCopyFile_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	srcPath := dir + "/empty.txt"
	dstPath := dir + "/empty_copy.txt"

	os.WriteFile(srcPath, []byte{}, 0644)
	if err := copyFile(srcPath, dstPath, 0644); err != nil {
		t.Fatalf("copyFile: %v", err)
	}

	data, err := os.ReadFile(dstPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 0 {
		t.Errorf("expected empty file, got %d bytes", len(data))
	}
}

func TestCopyFile_LargeFile(t *testing.T) {
	dir := t.TempDir()
	srcPath := dir + "/large.bin"
	dstPath := dir + "/large_copy.bin"

	// 1 MiB of data
	data := make([]byte, 1024*1024)
	for i := range data {
		data[i] = byte(i % 256)
	}
	os.WriteFile(srcPath, data, 0644)

	if err := copyFile(srcPath, dstPath, 0644); err != nil {
		t.Fatalf("copyFile: %v", err)
	}

	got, err := os.ReadFile(dstPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(data) {
		t.Fatalf("size mismatch: got %d, want %d", len(got), len(data))
	}
	for i := range data {
		if got[i] != data[i] {
			t.Fatalf("byte mismatch at offset %d", i)
		}
	}
}

func TestCopyFile_Overwrite(t *testing.T) {
	dir := t.TempDir()
	srcPath := dir + "/src.txt"
	dstPath := dir + "/dst.txt"

	os.WriteFile(srcPath, []byte("new content"), 0644)
	os.WriteFile(dstPath, []byte("old content that is longer"), 0644)

	if err := copyFile(srcPath, dstPath, 0644); err != nil {
		t.Fatalf("copyFile: %v", err)
	}

	data, err := os.ReadFile(dstPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "new content" {
		t.Errorf("got %q, want %q", data, "new content")
	}
}

func TestStageKernelModules_DecompressesZstdAndWritesManifest(t *testing.T) {
	dir := t.TempDir()
	modulePath := filepath.Join(dir, "virtiofs.ko.zst")

	moduleData := []byte("fake-module-payload")
	writeZstdFile(t, modulePath, moduleData)

	root := filepath.Join(dir, "root")
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}

	if err := stageKernelModules(root, []KernelModule{{HostPath: modulePath}}); err != nil {
		t.Fatalf("stageKernelModules: %v", err)
	}

	stagedPath := filepath.Join(root, "lib", "modules", "gocracker", "virtiofs.ko")
	gotModule, err := os.ReadFile(stagedPath)
	if err != nil {
		t.Fatalf("read staged module: %v", err)
	}
	if string(gotModule) != string(moduleData) {
		t.Fatalf("staged module mismatch: got %q want %q", gotModule, moduleData)
	}

	manifest, err := os.ReadFile(filepath.Join(root, strings.TrimPrefix(moduleManifestPath, "/")))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if got := string(manifest); got != "/lib/modules/gocracker/virtiofs.ko\n" {
		t.Fatalf("manifest = %q", got)
	}
}

func TestSanitizeModuleName(t *testing.T) {
	got := sanitizeModuleName(KernelModule{HostPath: "/lib/modules/virtiofs.ko.zst"})
	if got != "virtiofs" {
		t.Fatalf("sanitizeModuleName = %q, want virtiofs", got)
	}
}

func TestBuildInitrdWithOptions_IncludesKernelModuleManifestAndPayload(t *testing.T) {
	dir := t.TempDir()
	modulePath := filepath.Join(dir, "virtiofs.ko.zst")
	moduleData := []byte("fake-module-payload")
	writeZstdFile(t, modulePath, moduleData)

	initrdPath := filepath.Join(dir, "initrd.img")
	if err := BuildInitrdWithOptions(initrdPath, InitrdOptions{
		KernelModules: []KernelModule{{HostPath: modulePath}},
	}); err != nil {
		t.Fatalf("BuildInitrdWithOptions: %v", err)
	}

	entries := readInitrdEntries(t, initrdPath)
	if got := entries["etc/gocracker/modules.list"]; got != "/lib/modules/gocracker/virtiofs.ko\n" {
		t.Fatalf("modules.list = %q", got)
	}
	if got := entries["lib/modules/gocracker/virtiofs.ko"]; got != string(moduleData) {
		t.Fatalf("module payload = %q, want %q", got, moduleData)
	}
}

func TestBuildInitrdWithOptions_IncludesRuntimeSpec(t *testing.T) {
	dir := t.TempDir()
	initrdPath := filepath.Join(dir, "initrd.img")
	spec := runtimecfg.GuestSpec{
		Process: runtimecfg.Process{
			Exec: "/bin/server",
			Args: []string{"--port", "8080"},
		},
		Env:     []string{"PATH=/usr/bin", "DEBUG=1"},
		WorkDir: "/srv/app",
		User:    "1000:1000",
	}

	if err := BuildInitrdWithOptions(initrdPath, InitrdOptions{
		RuntimeSpec: &spec,
	}); err != nil {
		t.Fatalf("BuildInitrdWithOptions: %v", err)
	}

	entries := readInitrdEntries(t, initrdPath)
	raw, ok := entries[strings.TrimPrefix(runtimecfg.GuestSpecPath, "/")]
	if !ok {
		t.Fatalf("missing %s in initrd", runtimecfg.GuestSpecPath)
	}
	got, err := runtimecfg.UnmarshalGuestSpecJSON([]byte(raw))
	if err != nil {
		t.Fatalf("UnmarshalGuestSpecJSON() error = %v", err)
	}
	if got.Process.Exec != spec.Process.Exec || got.WorkDir != spec.WorkDir || got.User != spec.User {
		t.Fatalf("runtime spec = %#v, want %#v", got, spec)
	}
	if strings.Join(got.Env, ",") != strings.Join(spec.Env, ",") {
		t.Fatalf("env = %#v, want %#v", got.Env, spec.Env)
	}
}

func writeZstdFile(t *testing.T, path string, data []byte) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	enc, err := zstd.NewWriter(f)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := enc.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
}

func readInitrdEntries(t *testing.T, path string) map[string]string {
	t.Helper()

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	gr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer gr.Close()

	cr := cpiolib.NewReader(gr)
	entries := map[string]string{}
	for {
		hdr, err := cr.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatal(err)
		}
		name := strings.TrimPrefix(strings.TrimPrefix(hdr.Name, "./"), "/")
		data, err := io.ReadAll(cr)
		if err != nil {
			t.Fatal(err)
		}
		entries[name] = string(data)
	}
	return entries
}
