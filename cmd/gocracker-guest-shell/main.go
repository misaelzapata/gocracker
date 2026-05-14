//go:build linux

// gocracker-guest-shell is a minimal pid 1 that boots into an
// interactive shell on the serial console. Built statically and
// embedded as /init in a tiny initramfs for the WHP boot smoke path.
package main

import (
	"syscall"
	"time"
)

func main() {
	// Mount devtmpfs so /dev/ttyS0, /dev/kmsg, /dev/console etc.
	// appear automatically. Without this, hand-crafted mknod nodes
	// don't actually function as device files on most kernels.
	_ = syscall.Mkdir("/dev", 0o755)
	_ = syscall.Mount("devtmpfs", "/dev", "devtmpfs", syscall.MS_NOSUID, "mode=0755")
	_ = syscall.Mkdir("/proc", 0o755)
	_ = syscall.Mount("proc", "/proc", "proc", 0, "")
	_ = syscall.Mkdir("/sys", 0o755)
	_ = syscall.Mount("sysfs", "/sys", "sysfs", 0, "")

	// Banner via the kernel printk channel — guaranteed to reach the
	// host's console=ttyS0 via the printk forwarding path.
	klog("=== gocracker-guest-shell — Linux on WHP — alive as PID 1 ===")
	klog("Type characters on the serial console; they'll echo back.")
	klog("Press Enter to get a fresh prompt. Ctrl-C halts the guest.")

	// Open /dev/ttyS0 read-write with O_NOCTTY so it doesn't become
	// our controlling terminal (which would block reads in weird
	// ways). Use the same fd for input/output: serial drivers are
	// full-duplex.
	tty, err := syscall.Open("/dev/ttyS0", syscall.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		klog("open /dev/ttyS0 failed; sleeping forever")
		for {
			time.Sleep(time.Hour)
		}
	}
	defer syscall.Close(tty)

	_, _ = syscall.Write(tty, []byte("\r\n# "))

	buf := make([]byte, 64)
	for {
		n, err := syscall.Read(tty, buf)
		if err != nil || n == 0 {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		_, _ = syscall.Write(tty, buf[:n])
		for _, b := range buf[:n] {
			if b == '\r' || b == '\n' {
				_, _ = syscall.Write(tty, []byte("\r\n# "))
				break
			}
		}
	}
}

func klog(s string) {
	fd, err := syscall.Open("/dev/kmsg", syscall.O_WRONLY, 0)
	if err != nil {
		return
	}
	_, _ = syscall.Write(fd, []byte("<6>"+s+"\n"))
	_ = syscall.Close(fd)
}
