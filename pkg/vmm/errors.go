package vmm

import "errors"

// Platform capability errors returned when a feature is not available on the
// current operating system or hypervisor backend.
var (
	ErrSnapshotNotSupported  = errors.New("vmm: snapshots are not supported on this platform")
	ErrMigrationNotSupported = errors.New("vmm: live migration is not supported on this platform")
	ErrHotplugNotSupported   = errors.New("vmm: memory hotplug is not supported on this platform")
	ErrTAPNotSupported       = errors.New("vmm: TAP networking is not available on this platform; use NAT mode")
)
