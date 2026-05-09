//go:build !linux

// gocracker-sandboxd transitively depends on pkg/vmm. Cross-compile stub.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "gocracker-sandboxd: Linux-only until pkg/vmm becomes hypervisor-agnostic (Phase 1.2 / Phase 2).")
	os.Exit(2)
}
