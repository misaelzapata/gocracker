//go:build linux

// gocracker-guest-shell is a minimal pid 1 for the WHP boot-to-shell
// smoke path. It writes one byte at a time to /dev/console (raw
// syscall, no Go stdio buffering), echoes anything it reads, and never
// exits. No mounts, no Go runtime features beyond syscall — kept small
// so the only thing that can go wrong is the host UART path itself.
package main

import (
	"syscall"
	"time"
)

func main() {
	// First sign of life: write to fd 1, which the kernel wires to
	// /dev/console (= ttyS0) before exec'ing /init. No /proc or /dev
	// mount needed.
	write1("\r\n=== gocracker-guest-shell alive ===\r\n# ")

	// Try to also open /dev/ttyS0 directly as a belt-and-braces in case
	// fd 1 isn't routed through the console driver for some reason.
	if fd, err := syscall.Open("/dev/ttyS0", syscall.O_RDWR, 0); err == nil {
		rawWrite(fd, "(also reachable via /dev/ttyS0)\r\n")
		_ = syscall.Close(fd)
	}

	// Read from fd 0 (/dev/console) byte at a time and echo back.
	buf := make([]byte, 1)
	for {
		n, err := syscall.Read(0, buf)
		if err != nil || n == 0 {
			// stdin not wired — just pause and try again later.
			time.Sleep(100 * time.Millisecond)
			continue
		}
		// Echo char back so the user sees what they type.
		write1(string(buf[:n]))
		if buf[0] == '\r' || buf[0] == '\n' {
			write1("\r\n# ")
		}
	}
}

func write1(s string) { _, _ = syscall.Write(1, []byte(s)) }

func rawWrite(fd int, s string) { _, _ = syscall.Write(fd, []byte(s)) }
