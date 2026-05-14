// mkshellinitrd builds an initramfs (gzipped cpio) whose /init is the
// statically-linked gocracker-guest-shell binary. Used by the WHP
// boot-to-shell smoke path before the full vsock exec-agent lands on
// Windows.
//
// Usage:
//
//	mkshellinitrd <shell-elf-path> <output-initrd-path>
package main

import (
	"fmt"
	"os"

	"github.com/gocracker/gocracker/internal/guest"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: mkshellinitrd <shell-elf> <output-initrd>")
		os.Exit(2)
	}
	shellELF, out := os.Args[1], os.Args[2]
	if _, err := os.Stat(shellELF); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	// ExtraFiles overrides the default embedded init: the cpio packer
	// writes the file at the requested guest path after the default
	// /init, so the shell ELF ends up as the actual pid 1.
	err := guest.BuildInitrdWithOptions(out, guest.InitrdOptions{
		ExtraFiles: map[string]string{
			"/init":      shellELF,
			"/sbin/init": shellELF,
		},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fi, _ := os.Stat(out)
	fmt.Printf("wrote %s (%d bytes)\n", out, fi.Size())
}
