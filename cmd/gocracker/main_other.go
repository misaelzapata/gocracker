//go:build !linux

// gocracker on non-Linux is a stub binary until the WHP backend lands
// (Phase 2 of the Windows port). Today the main CLI pulls pkg/vmm which
// imports internal/kvm — both Linux-only. Once Phase 1.2 (machineArchBackend
// refactor) and Phase 2 (WHP backend) land, this file is removed and
// main.go drops its //go:build linux constraint.
package main

import (
	"fmt"
	"os"
	"runtime"
)

func main() {
	fmt.Fprintln(os.Stderr, "gocracker: this binary is Linux-only on "+runtime.GOOS+"/"+runtime.GOARCH+".")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Native "+runtime.GOOS+" support requires the WHP backend, which is")
	fmt.Fprintln(os.Stderr, "scheduled for Phase 2 of the port plan in")
	fmt.Fprintln(os.Stderr, "C:/Users/misae/.claude/plans/analyze-all-the-repo-polished-rainbow.md.")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Until then, run gocracker inside WSL2 (KVM is available there).")
	os.Exit(2)
}
