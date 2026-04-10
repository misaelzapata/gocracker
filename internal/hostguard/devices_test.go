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

// ---- NEW TESTS ----

func TestBaseDevices_ExpectedPaths(t *testing.T) {
	expected := map[string]bool{
		"null":    false,
		"zero":    false,
		"full":    false,
		"random":  false,
		"urandom": false,
		"tty":     false,
	}
	for _, dev := range baseDevices {
		expected[dev.relPath] = true
	}
	for name, found := range expected {
		if !found {
			t.Errorf("base device %q not found in baseDevices", name)
		}
	}
}

func TestBaseDevices_MajorMinor(t *testing.T) {
	// Verify specific known device numbers
	for _, dev := range baseDevices {
		switch dev.relPath {
		case "null":
			if dev.major != 1 || dev.minor != 3 {
				t.Errorf("null = %d:%d, want 1:3", dev.major, dev.minor)
			}
		case "zero":
			if dev.major != 1 || dev.minor != 5 {
				t.Errorf("zero = %d:%d, want 1:5", dev.major, dev.minor)
			}
		case "full":
			if dev.major != 1 || dev.minor != 7 {
				t.Errorf("full = %d:%d, want 1:7", dev.major, dev.minor)
			}
		case "random":
			if dev.major != 1 || dev.minor != 8 {
				t.Errorf("random = %d:%d, want 1:8", dev.major, dev.minor)
			}
		case "urandom":
			if dev.major != 1 || dev.minor != 9 {
				t.Errorf("urandom = %d:%d, want 1:9", dev.major, dev.minor)
			}
		case "tty":
			if dev.major != 5 || dev.minor != 0 {
				t.Errorf("tty = %d:%d, want 5:0", dev.major, dev.minor)
			}
		}
	}
}

func TestOptionalDevices_TunPath(t *testing.T) {
	tunSpec, ok := optionalDevices["tun"]
	if !ok {
		t.Fatal("missing tun in optionalDevices")
	}
	if tunSpec.relPath != filepath.Join("net", "tun") {
		t.Fatalf("tun relPath = %q, want net/tun", tunSpec.relPath)
	}
}

func TestCheckDeviceTree_BothKVMAndTUN(t *testing.T) {
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
	// Requiring both KVM and TUN should fail in test env
	err := CheckDeviceTree(root, DeviceRequirements{NeedKVM: true, NeedTun: true})
	if err == nil {
		t.Fatal("expected error when both KVM and TUN are required but missing")
	}
}

func TestCheckDeviceTree_FullDeviceSymlinks(t *testing.T) {
	root := t.TempDir()
	// Create all base devices + kvm + tun as symlinks to real /dev
	allDevices := append([]charDeviceSpec{}, baseDevices...)
	allDevices = append(allDevices, optionalDevices["kvm"], optionalDevices["tun"])
	for _, device := range allDevices {
		hostPath := filepath.Join("/dev", device.relPath)
		devDir := filepath.Dir(filepath.Join(root, device.relPath))
		if err := os.MkdirAll(devDir, 0755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		// Only symlink if the host device exists
		if _, err := os.Stat(hostPath); err == nil {
			if err := os.Symlink(hostPath, filepath.Join(root, device.relPath)); err != nil {
				t.Fatalf("symlink %s: %v", device.relPath, err)
			}
		}
	}
	// Base-only check should pass if host has /dev/null etc.
	if err := CheckDeviceTree(root, DeviceRequirements{}); err != nil {
		t.Fatalf("CheckDeviceTree base: %v", err)
	}
}

func TestCheckDeviceTree_RegularFileInsteadOfDevice(t *testing.T) {
	root := t.TempDir()
	// Create all base devices as symlinks except "zero" which is a regular file
	for _, device := range baseDevices {
		devPath := filepath.Join(root, device.relPath)
		if err := os.MkdirAll(filepath.Dir(devPath), 0755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if device.relPath == "zero" {
			if err := os.WriteFile(devPath, []byte("fake"), 0644); err != nil {
				t.Fatalf("write: %v", err)
			}
			continue
		}
		if err := os.Symlink(filepath.Join("/dev", device.relPath), devPath); err != nil {
			t.Fatalf("symlink: %v", err)
		}
	}
	err := CheckDeviceTree(root, DeviceRequirements{})
	if err == nil {
		t.Fatal("expected error for regular file instead of device")
	}
	if !strings.Contains(err.Error(), "zero") {
		t.Fatalf("error = %q, expected to mention zero", err.Error())
	}
}

func TestCheckHostDevices_UsesDevRoot(t *testing.T) {
	// CheckHostDevices should use /dev as root
	// On a real system this should work; we just test it doesn't panic
	_ = CheckHostDevices(DeviceRequirements{})
}
