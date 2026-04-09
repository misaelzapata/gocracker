package compose

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/gocracker/gocracker/pkg/vmm"
)

const guestExitCodeFile = "/.gocracker-exit-code"

func waitForServiceExitCode(service *ServiceVM) (int, error) {
	if service == nil || service.Result == nil {
		return 0, fmt.Errorf("service is not running")
	}
	if service.apiClient != nil && service.VMID != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := waitForRemoteStop(ctx, service.apiClient, service.VMID); err != nil {
			return 0, err
		}
		if service.Result.DiskPath == "" {
			info, err := service.apiClient.GetVM(ctx, service.VMID)
			if err == nil {
				service.Result.DiskPath = strings.TrimSpace(info.Metadata["disk_path"])
			}
		}
		return readGuestExitCode(service.Result.DiskPath)
	}
	if service.VM == nil {
		return 0, fmt.Errorf("service has no VM handle")
	}
	for {
		if service.VM.State() == vmm.StateStopped {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	return readGuestExitCode(service.Result.DiskPath)
}

func readGuestExitCode(diskPath string) (int, error) {
	value, err := readGuestFileWithDebugfs(diskPath, guestExitCodeFile)
	if err != nil {
		return 0, err
	}
	code, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("parse guest exit code %q: %w", value, err)
	}
	return code, nil
}

func readGuestFileWithDebugfs(diskPath, guestPath string) (string, error) {
	out, err := exec.Command("debugfs", "-R", "cat "+guestPath, diskPath).Output()
	if err != nil {
		return "", fmt.Errorf("read %s from %s with debugfs: %w", guestPath, diskPath, err)
	}
	return strings.TrimSpace(string(out)), nil
}
