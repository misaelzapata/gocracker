package hostguard

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

type DeviceRequirements struct {
	NeedKVM bool
	NeedTun bool
}

type charDeviceSpec struct {
	relPath string
	major   uint32
	minor   uint32
}

var baseDevices = []charDeviceSpec{
	{relPath: "null", major: 1, minor: 3},
	{relPath: "zero", major: 1, minor: 5},
	{relPath: "full", major: 1, minor: 7},
	{relPath: "random", major: 1, minor: 8},
	{relPath: "urandom", major: 1, minor: 9},
	{relPath: "tty", major: 5, minor: 0},
}

var optionalDevices = map[string]charDeviceSpec{
	"kvm": {relPath: "kvm", major: 10, minor: 232},
	"tun": {relPath: filepath.Join("net", "tun"), major: 10, minor: 200},
}

func CheckHostDevices(req DeviceRequirements) error {
	return CheckDeviceTree("/dev", req)
}

func CheckDeviceTree(root string, req DeviceRequirements) error {
	for _, device := range baseDevices {
		if err := checkCharDevice(root, device); err != nil {
			return err
		}
	}
	if req.NeedKVM {
		if err := checkCharDevice(root, optionalDevices["kvm"]); err != nil {
			return err
		}
	}
	if req.NeedTun {
		if err := checkCharDevice(root, optionalDevices["tun"]); err != nil {
			return err
		}
	}
	return nil
}

// HasNetAdmin reports whether the current process can create TAP/TUN
// devices. True for uid 0 and for any process that carries the
// CAP_NET_ADMIN capability in its effective set. Used by handleRun to
// fail /run requests with network_mode=auto fast (403), instead of
// queuing a VM that would crash at TUNSETIFF time.
func HasNetAdmin() bool {
	if os.Geteuid() == 0 {
		return true
	}
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return false
	}
	for _, line := range splitLines(data) {
		if len(line) < 7 || string(line[:7]) != "CapEff:" {
			continue
		}
		raw := string(line[7:])
		for len(raw) > 0 && (raw[0] == ' ' || raw[0] == '\t') {
			raw = raw[1:]
		}
		var cap uint64
		if _, err := fmt.Sscanf(raw, "%x", &cap); err != nil {
			return false
		}
		// CAP_NET_ADMIN is bit 12 (linux/capability.h).
		return cap&(1<<12) != 0
	}
	return false
}

func splitLines(data []byte) [][]byte {
	lines := make([][]byte, 0, 16)
	start := 0
	for i, b := range data {
		if b == '\n' {
			lines = append(lines, data[start:i])
			start = i + 1
		}
	}
	if start < len(data) {
		lines = append(lines, data[start:])
	}
	return lines
}

func checkCharDevice(root string, spec charDeviceSpec) error {
	path := filepath.Join(root, spec.relPath)
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%s is not usable: %w", path, err)
	}
	if info.Mode()&os.ModeDevice == 0 || info.Mode()&os.ModeCharDevice == 0 {
		return fmt.Errorf("%s is not a character device", path)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%s: unsupported stat payload %T", path, info.Sys())
	}
	major := unix.Major(uint64(stat.Rdev))
	minor := unix.Minor(uint64(stat.Rdev))
	if major != spec.major || minor != spec.minor {
		return fmt.Errorf("%s has wrong device number %d:%d (want %d:%d)", path, major, minor, spec.major, spec.minor)
	}
	return nil
}
