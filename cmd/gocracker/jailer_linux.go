//go:build linux

package main

import "github.com/gocracker/gocracker/internal/jailer"

func cmdJailer(args []string) {
	if err := jailer.RunCLI(args); err != nil {
		fatal(err.Error())
	}
}
