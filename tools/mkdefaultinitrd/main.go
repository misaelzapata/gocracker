// mkdefaultinitrd writes the default gocracker-guest-init initramfs.
// Used as a baseline smoke test — proves the kernel reaches userspace.
package main

import (
	"fmt"
	"os"

	"github.com/gocracker/gocracker/internal/guest"
)

func main() {
	out := "default-initrd.cpio.gz"
	if len(os.Args) > 1 {
		out = os.Args[1]
	}
	if err := guest.BuildInitrd(out, nil); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fi, _ := os.Stat(out)
	fmt.Println("wrote", out, fi.Size(), "bytes")
}
