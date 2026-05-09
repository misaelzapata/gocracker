//go:build linux

package container

import (
	"os"
	"syscall"
)

// ficlone is the Linux ioctl number for FICLONE (copy-on-write clone).
const ficlone = 0x40049409

func tryReflink(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		return err
	}
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, out.Fd(), ficlone, in.Fd())
	if errno != 0 {
		out.Close()
		os.Remove(dst)
		return errno
	}
	return out.Close()
}
