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

// --- Additional tests ---

func TestCheckDeviceTree_MissingKVM(t *testing.T) {
	root := t.TempDir()
	// Create valid symlinks for base devices only
	for _, device := range baseDevices {
		hostPath := filepath.Join("/dev", device.relPath)
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, device.relPath)), 0755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.Symlink(hostPath, filepath.Join(root, device.relPath)); err != nil {
			t.Fatalf("symlink %s: %v", device.relPath, err)
		}
	}
	// Base devices should pass without KVM requirement
	if err := CheckDeviceTree(root, DeviceRequirements{NeedKVM: false}); err != nil {
		t.Fatalf("CheckDeviceTree(NeedKVM=false): %v", err)
	}
	// Requiring KVM should fail since /dev/kvm doesn't exist in our temp root
	err := CheckDeviceTree(root, DeviceRequirements{NeedKVM: true})
	if err == nil {
		t.Fatal("expected error when KVM is required but missing")
	}
	if !strings.Contains(err.Error(), "kvm") {
		t.Fatalf("error = %q, expected to mention kvm", err.Error())
	}
}

func TestCheckDeviceTree_MissingTUN(t *testing.T) {
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
	// Without TUN requirement should pass
	if err := CheckDeviceTree(root, DeviceRequirements{NeedTun: false}); err != nil {
		t.Fatalf("CheckDeviceTree(NeedTun=false): %v", err)
	}
	// Requiring TUN should fail since net/tun doesn't exist in our temp root
	err := CheckDeviceTree(root, DeviceRequirements{NeedTun: true})
	if err == nil {
		t.Fatal("expected error when TUN is required but missing")
	}
	if !strings.Contains(err.Error(), "tun") {
		t.Fatalf("error = %q, expected to mention tun", err.Error())
	}
}

func TestCheckDeviceTree_EmptyRoot(t *testing.T) {
	root := t.TempDir()
	err := CheckDeviceTree(root, DeviceRequirements{})
	if err == nil {
		t.Fatal("expected error for empty device tree")
	}
}

func TestCheckDeviceTree_MissingDevice(t *testing.T) {
	root := t.TempDir()
	err := CheckDeviceTree(root, DeviceRequirements{})
	if err == nil {
		t.Fatal("expected error when base devices are missing")
	}
	if !strings.Contains(err.Error(), "not usable") {
		t.Fatalf("error = %q, expected 'not usable'", err.Error())
	}
}

func TestDeviceRequirements_Defaults(t *testing.T) {
	req := DeviceRequirements{}
	if req.NeedKVM {
		t.Fatal("NeedKVM should default to false")
	}
	if req.NeedTun {
		t.Fatal("NeedTun should default to false")
	}
}

func TestOptionalDevices_Constants(t *testing.T) {
	kvmSpec, ok := optionalDevices["kvm"]
	if !ok {
		t.Fatal("missing kvm in optionalDevices")
	}
	if kvmSpec.major != 10 || kvmSpec.minor != 232 {
		t.Fatalf("kvm device = %d:%d, want 10:232", kvmSpec.major, kvmSpec.minor)
	}

	tunSpec, ok := optionalDevices["tun"]
	if !ok {
		t.Fatal("missing tun in optionalDevices")
	}
	if tunSpec.major != 10 || tunSpec.minor != 200 {
		t.Fatalf("tun device = %d:%d, want 10:200", tunSpec.major, tunSpec.minor)
	}
}

func TestBaseDevices_Count(t *testing.T) {
	if len(baseDevices) != 6 {
		t.Fatalf("expected 6 base devices, got %d", len(baseDevices))
	}
}
