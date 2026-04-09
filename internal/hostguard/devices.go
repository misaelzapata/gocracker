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
