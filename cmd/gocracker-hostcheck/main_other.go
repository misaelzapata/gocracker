//go:build !linux

// gocracker-hostcheck verifies Linux-host invariants (/dev/kvm, /dev/net/tun,
// CAP_NET_ADMIN, /dev/ptmx). On non-Linux those concepts don't apply.
// Phase 12 of the Windows port plan adds Windows-specific checks
// (HypervisorPlatform feature, WinHvPlatform.dll, SE_LOCK_MEMORY_NAME);
// until then this stub reports the Linux-only status.
package main

import (
	"fmt"
	"os"
	"runtime"
)

func main() {
	fmt.Fprintln(os.Stderr, "gocracker-hostcheck: Linux-only on "+runtime.GOOS+
		"; Windows checks (WHP feature, WinHvPlatform.dll) land in Phase 12.")
	os.Exit(2)
}
