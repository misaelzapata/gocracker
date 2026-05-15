//go:build !linux && !windows

// gocracker-vmm is the per-VM worker process. The Linux build (main.go)
// pins memory via unix.Mlockall and listens on a Unix socket inherited
// from the parent supervisor. The Windows build (main_windows.go) runs
// a minimal AF_UNIX REST shim over pkg/vmm.BootLinuxOnWHP.
//
// On macOS and other Unixes the equivalent worker lands when an HVF
// backend ships — until then this stub gives go vet/go build a main
// symbol so the cross-compile pipeline doesn't break.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "gocracker-vmm: this binary is not yet ported to "+runtimeOSName()+
		"; see CHANGELOG.md for the port roadmap.")
	os.Exit(2)
}
