package hostguard

import (
	"fmt"
	"os"
	"strings"
)

func CheckPTYSupport() error {
	fd, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err == nil {
		_ = fd.Close()
		return nil
	}

	mountInfo, readErr := os.ReadFile("/proc/self/mountinfo")
	if readErr == nil {
		if opts, ok := devPtsMountOptions(string(mountInfo)); ok {
			if strings.Contains(opts, "ptmxmode=000") {
				return fmt.Errorf("/dev/ptmx is unusable because /dev/pts is mounted with %q; fix with: sudo mount -o remount,gid=5,mode=620,ptmxmode=666 /dev/pts", opts)
			}
		}
	}

	return fmt.Errorf("/dev/ptmx is not usable: %w", err)
}

func devPtsMountOptions(mountInfo string) (string, bool) {
	for _, line := range strings.Split(mountInfo, "\n") {
		if !strings.Contains(line, " /dev/pts ") || !strings.Contains(line, " - devpts ") {
			continue
		}
		parts := strings.SplitN(line, " - ", 2)
		if len(parts) != 2 {
			continue
		}
		fields := strings.Fields(parts[1])
		if len(fields) < 3 || fields[0] != "devpts" {
			continue
		}
		return fields[2], true
	}
	return "", false
}
