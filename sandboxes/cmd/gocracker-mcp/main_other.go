//go:build !linux

// gocracker-mcp transitively depends on pkg/vmm (KVM-coupled) until Phase
// 1.2 lands. Cross-compile stub.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "gocracker-mcp: Linux-only until pkg/vmm becomes hypervisor-agnostic (Phase 1.2 / Phase 2).")
	os.Exit(2)
}
