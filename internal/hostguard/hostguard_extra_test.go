package hostguard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckCharDeviceNotADevice(t *testing.T) {
	root := t.TempDir()
	// Create a regular file instead of a device
	path := filepath.Join(root, "notdev")
	if err := os.WriteFile(path, []byte("nope"), 0644); err != nil {
		t.Fatal(err)
	}
	err := checkCharDevice(root, charDeviceSpec{relPath: "notdev", major: 1, minor: 3})
	if err == nil {
		t.Fatal("expected error for non-device file")
	}
	if !strings.Contains(err.Error(), "not a character device") {
		t.Fatalf("error = %q", err.Error())
	}
}

func TestCheckCharDeviceMissing(t *testing.T) {
	root := t.TempDir()
	err := checkCharDevice(root, charDeviceSpec{relPath: "missing", major: 1, minor: 3})
	if err == nil {
		t.Fatal("expected error for missing device")
	}
	if !strings.Contains(err.Error(), "not usable") {
		t.Fatalf("error = %q", err.Error())
	}
}

func TestCheckCharDeviceWrongMajorMinor(t *testing.T) {
	// Symlink to /dev/null (1:3) but expect wrong major/minor
	root := t.TempDir()
	if err := os.Symlink("/dev/null", filepath.Join(root, "null")); err != nil {
		t.Fatal(err)
	}
	err := checkCharDevice(root, charDeviceSpec{relPath: "null", major: 99, minor: 99})
	if err == nil {
		t.Fatal("expected error for wrong device number")
	}
	if !strings.Contains(err.Error(), "wrong device number") {
		t.Fatalf("error = %q", err.Error())
	}
}

func TestCheckCharDeviceCorrectMajorMinor(t *testing.T) {
	root := t.TempDir()
	if err := os.Symlink("/dev/null", filepath.Join(root, "null")); err != nil {
		t.Fatal(err)
	}
	err := checkCharDevice(root, charDeviceSpec{relPath: "null", major: 1, minor: 3})
	if err != nil {
		t.Fatalf("checkCharDevice(null) = %v", err)
	}
}

func TestCheckPTYSupportDoesNotPanic(t *testing.T) {
	_ = CheckPTYSupport()
}

func TestDevPtsMountOptionsPtmxmode000(t *testing.T) {
	t.Parallel()
	mountInfo := "30 29 0:26 / /dev/pts rw shared:3 - devpts devpts rw,gid=5,mode=620,ptmxmode=000\n"
	opts, ok := devPtsMountOptions(mountInfo)
	if !ok {
		t.Fatal("expected match")
	}
	if !strings.Contains(opts, "ptmxmode=000") {
		t.Fatalf("opts = %q, expected ptmxmode=000", opts)
	}
}

func TestCheckDeviceTreeWithTunSymlink(t *testing.T) {
	if _, err := os.Stat("/dev/net/tun"); err != nil {
		t.Skip("/dev/net/tun not available")
	}
	root := t.TempDir()
	for _, device := range baseDevices {
		devPath := filepath.Join(root, device.relPath)
		if err := os.MkdirAll(filepath.Dir(devPath), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join("/dev", device.relPath), devPath); err != nil {
			t.Fatal(err)
		}
	}
	tunDir := filepath.Join(root, "net")
	os.MkdirAll(tunDir, 0755)
	if err := os.Symlink("/dev/net/tun", filepath.Join(tunDir, "tun")); err != nil {
		t.Fatal(err)
	}
	if err := CheckDeviceTree(root, DeviceRequirements{NeedTun: true}); err != nil {
		t.Fatalf("CheckDeviceTree with TUN: %v", err)
	}
}
