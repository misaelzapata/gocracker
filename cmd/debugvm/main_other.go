//go:build !linux

// debugvm is a Linux-only KVM debug helper. Phase 2 of the Windows port
// adds an equivalent for WHP partitions; until then this stub keeps the
// cross-compile pipeline green.
package main

import (
	"fmt"
	"os"
	"runtime"
)

func main() {
	fmt.Fprintln(os.Stderr, "debugvm: Linux-only on "+runtime.GOOS+"; pending Phase 2 (WHP backend).")
	os.Exit(2)
}
