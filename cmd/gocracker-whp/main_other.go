//go:build !windows

// gocracker-whp is a Windows-only binary that boots a Linux kernel via
// the Windows Hypervisor Platform. On non-Windows platforms this stub
// keeps the cross-compile pipeline green.
package main

import (
	"fmt"
	"os"
	"runtime"
)

func main() {
	fmt.Fprintln(os.Stderr, "gocracker-whp: Windows-only (uses WinHvPlatform.dll); running on "+runtime.GOOS)
	os.Exit(2)
}
