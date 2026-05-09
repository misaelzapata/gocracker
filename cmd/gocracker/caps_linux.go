//go:build linux

package main

import "golang.org/x/sys/unix"

// raiseAmbientNetAdmin promotes CAP_NET_ADMIN to the ambient capability set so
// that every child process exec'd by this server (ip, iptables, etc.) inherits
// the capability automatically. This allows gocracker to run as a non-root user
// with `setcap cap_net_admin+ep` while still delegating network setup to those
// utilities.
//
// Steps:
//  1. Add CAP_NET_ADMIN to the inheritable set (required by Linux before a cap
//     can be raised to ambient).
//  2. Call prctl(PR_CAP_AMBIENT, PR_CAP_AMBIENT_RAISE, CAP_NET_ADMIN).
//
// Errors are silently ignored: kernels < 4.3 don't have ambient caps, and any
// failure means gocracker simply falls back to requiring root for sub-process
// network operations.
func raiseAmbientNetAdmin() {
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var data [2]unix.CapUserData
	if err := unix.Capget(&hdr, &data[0]); err != nil {
		return
	}
	// CAP_NET_ADMIN = 12; it fits in the low 32-bit word.
	data[0].Inheritable |= 1 << unix.CAP_NET_ADMIN
	if err := unix.Capset(&hdr, &data[0]); err != nil {
		return
	}
	_ = unix.Prctl(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_RAISE, unix.CAP_NET_ADMIN, 0, 0)
}
