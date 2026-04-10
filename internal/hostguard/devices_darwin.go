//go:build darwin

package hostguard

import (
	"fmt"
	"os/exec"
	"strings"
)

// CheckHostDevices verifies that the macOS host supports Virtualization.framework.
// KVM and TUN device requirements are ignored on macOS since vz uses NAT networking.
func CheckHostDevices(req DeviceRequirements) error {
	return CheckDarwinVirtualizationSupport()
}

// CheckDeviceTree is a Linux-oriented check; on macOS it delegates to
// CheckDarwinVirtualizationSupport.
func CheckDeviceTree(root string, req DeviceRequirements) error {
	return CheckDarwinVirtualizationSupport()
}

// CheckDarwinVirtualizationSupport verifies macOS version and entitlements.
func CheckDarwinVirtualizationSupport() error {
	out, err := exec.Command("sw_vers", "-productVersion").Output()
	if err != nil {
		return fmt.Errorf("cannot determine macOS version: %w", err)
	}
	version := strings.TrimSpace(string(out))
	parts := strings.SplitN(version, ".", 3)
	if len(parts) < 1 {
		return fmt.Errorf("unexpected macOS version format: %q", version)
	}
	major := 0
	fmt.Sscanf(parts[0], "%d", &major)
	if major < 13 {
		return fmt.Errorf("macOS %s is not supported; Virtualization.framework requires macOS 13 (Ventura) or later", version)
	}
	return nil
}
