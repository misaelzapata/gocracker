//go:build linux

package main

import (
	"fmt"
	"io"
	"os"

	"github.com/gocracker/gocracker/internal/jailer"
)

var runJailerCLI = jailer.RunCLI

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

func run(args []string, stderr io.Writer) int {
	if err := runJailerCLI(args); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	return 0
}
