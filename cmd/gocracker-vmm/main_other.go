//go:build !linux

// gocracker-vmm is the per-VM worker process. The Linux build (main.go)
// pins memory via unix.Mlockall and listens on a Unix socket inherited
// from the parent supervisor.
//
// On Windows/macOS the equivalent worker lands in Phase 8 — at which
// point this stub is replaced by a platform-specific main_windows.go
// that uses windows.VirtualLock and either a named pipe or AF_UNIX
// socket. Until then this stub gives go vet/go build a main symbol so
// the cross-compile pipeline doesn't break.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "gocracker-vmm: this binary is not yet ported to "+runtimeOSName()+
		"; see Phase 8 of the Windows port plan.")
	os.Exit(2)
}
