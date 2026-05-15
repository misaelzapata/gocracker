//go:build !linux && !windows

// gocracker on non-Linux/non-Windows is a stub binary. On Linux the full
// CLI is in main.go (KVM-backed VMM, jailer, seccomp, hostnet, OCI build,
// REST API server, snapshot/restore, live migration). On Windows the WHP-
// backed `run` subcommand lives in main_windows.go.
//
// Other GOOS values (darwin, freebsd, etc.) have no backend yet; the
// binary exits with a clear message instead of producing confusing
// runtime failures.
package main

import (
	"fmt"
	"os"
	"runtime"
)

func main() {
	fmt.Fprintln(os.Stderr, "gocracker: native "+runtime.GOOS+"/"+runtime.GOARCH+" support is not yet planned.")
	fmt.Fprintln(os.Stderr, "Run gocracker inside a Linux VM (KVM is the supported hypervisor),")
	fmt.Fprintln(os.Stderr, "or on Windows where WHP-backed boot is available via gocracker.exe run.")
	os.Exit(2)
}
