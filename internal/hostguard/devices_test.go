package hostguard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckDeviceTree_BaseDevicesViaSymlinks(t *testing.T) {
	root := t.TempDir()
	for _, device := range baseDevices {
		hostPath := filepath.Join("/dev", device.relPath)
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, device.relPath)), 0755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.Symlink(hostPath, filepath.Join(root, device.relPath)); err != nil {
			t.Fatalf("symlink %s: %v", device.relPath, err)
		}
	}
	if err := CheckDeviceTree(root, DeviceRequirements{}); err != nil {
		t.Fatalf("CheckDeviceTree(): %v", err)
	}
}

func TestCheckDeviceTree_RejectsBrokenNull(t *testing.T) {
	root := t.TempDir()
	for _, device := range baseDevices {
		target := filepath.Join("/dev", device.relPath)
		if device.relPath == "null" {
			target = filepath.Join(root, "fake-null")
			if err := os.WriteFile(target, []byte("not-a-device"), 0644); err != nil {
				t.Fatalf("write fake null: %v", err)
			}
		}
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, device.relPath)), 0755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.Symlink(target, filepath.Join(root, device.relPath)); err != nil {
			t.Fatalf("symlink %s: %v", device.relPath, err)
		}
	}
	err := CheckDeviceTree(root, DeviceRequirements{})
	if err == nil || !strings.Contains(err.Error(), "null") {
		t.Fatalf("expected null device error, got %v", err)
	}
}
