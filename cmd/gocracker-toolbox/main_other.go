//go:build !linux

// gocracker-toolbox is the in-guest agent. The guest is always Linux,
// so this binary only matters on the Linux build. The non-Linux stub
// exists so cross-compilation produces a placeholder rather than a
// "no Go files" error.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "gocracker-toolbox: Linux-only (the gocracker guest is always Linux).")
	os.Exit(2)
}
