//go:build !linux

// gocracker-jailer is a Linux-only privilege-drop wrapper using unshare,
// pivot_root, and cgroups. On Windows/macOS the equivalent isolation is
// provided in-process by internal/winsandbox (job objects + restricted
// tokens), so this stub binary exists only to keep the cross-compile
// pipeline green; running it always errors.
package main

import (
	"fmt"
	"os"
	"runtime"
)

func main() {
	fmt.Fprintln(os.Stderr, "gocracker-jailer: this binary is Linux-only; "+
		"on "+runtime.GOOS+" the equivalent isolation runs in-process via internal/winsandbox.")
	os.Exit(2)
}
