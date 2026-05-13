package ext4

import (
	"os"
	"path/filepath"
	"testing"
)

// TestBuildImage_FromSourceTree builds an ext4 image populated with a
// handful of files and verifies they round-trip — we re-open the image
// and read the expected content back.
func TestBuildImage_FromSourceTree(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.MkdirAll(filepath.Join(src, "etc"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "etc", "hostname"), []byte("gocracker\n"), 0o644); err != nil {
		t.Fatalf("write hostname: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "init"), []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil {
		t.Fatalf("write init: %v", err)
	}

	img := filepath.Join(dir, "rootfs.ext4")
	const size = 16 * 1024 * 1024
	err := BuildImage(img, size, src, BuildOptions{
		VolumeName: "rootfs",
		ExtraFiles: map[string][]byte{
			"/etc/motd": []byte("welcome\n"),
		},
	})
	if err != nil {
		t.Fatalf("BuildImage: %v", err)
	}

	fi, err := os.Stat(img)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Size() != size {
		t.Errorf("image size = %d; want %d", fi.Size(), size)
	}

	// Re-open via go-diskfs to verify the files are readable.
	// go-diskfs's ext4 ReadFile rejects leading "/" (consistent with
	// its Mkdir API quirk), so paths here are root-relative.
	got := readbackFile(t, img, "etc/hostname")
	if string(got) != "gocracker\n" {
		t.Errorf("hostname = %q; want \"gocracker\\n\"", got)
	}
	got = readbackFile(t, img, "init")
	if string(got) != "#!/bin/sh\necho hi\n" {
		t.Errorf("init = %q", got)
	}
	got = readbackFile(t, img, "etc/motd")
	if string(got) != "welcome\n" {
		t.Errorf("motd = %q; want \"welcome\\n\"", got)
	}
}

// readbackFile reopens the image as a read-only ext4 filesystem and
// returns the bytes at the given path. Test helper.
func readbackFile(t *testing.T, imgPath, guestPath string) []byte {
	t.Helper()
	d, err := openImage(imgPath)
	if err != nil {
		t.Fatalf("open image: %v", err)
	}
	defer d.Close()
	fs, err := openExt4(d)
	if err != nil {
		t.Fatalf("open ext4: %v", err)
	}
	data, err := fs.ReadFile(guestPath)
	if err != nil {
		t.Fatalf("read %s: %v", guestPath, err)
	}
	return data
}
