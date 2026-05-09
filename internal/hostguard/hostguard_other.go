//go:build !linux

// Package hostguard's real implementation checks Linux-host invariants:
// /dev/kvm, /dev/net/tun, CAP_NET_ADMIN, /dev/ptmx PTY support. None of
// those concepts apply on Windows or macOS (the equivalent on Windows is
// the Hypervisor Platform feature flag, surfaced separately by Phase 12).
//
// This stub keeps non-Linux callers (internal/api, pkg/container,
// internal/console, etc.) compiling. Each export returns "not supported
// on this platform" so misuse is loud rather than silently misleading.
package hostguard

import "errors"

// errNotSupported is returned by every hostguard call on non-Linux. It
// is exported in shape (matches the unexported errNotSupported pattern
// used by hostnet/stacknet stubs) but kept package-private — the caller
// distinguishes via errors.Is on the sentinel below.
var errNotSupported = errors.New("hostguard checks are Linux-only on this platform")

// DeviceRequirements mirrors the Linux struct so callers compile.
type DeviceRequirements struct {
	NeedKVM bool
	NeedTun bool
}

// CheckHostDevices is the production entry point on Linux; on other
// platforms it returns errNotSupported so callers can branch.
func CheckHostDevices(req DeviceRequirements) error { return errNotSupported }

// CheckDeviceTree is the test helper variant on Linux; same stub here.
func CheckDeviceTree(root string, req DeviceRequirements) error { return errNotSupported }

// HasNetAdmin reports whether the current process has CAP_NET_ADMIN.
// On non-Linux there is no equivalent, so we return false — callers
// should already short-circuit on platform checks long before this.
func HasNetAdmin() bool { return false }

// CheckPTYSupport on Linux opens /dev/ptmx. Windows has ConPTY (since
// Win10 1809) which works differently; the relevant test lives in the
// console package, not here. Stub: return errNotSupported.
func CheckPTYSupport() error { return errNotSupported }
