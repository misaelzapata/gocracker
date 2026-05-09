//go:build !windows

// Package whp on non-Windows platforms exposes the same surface but
// every entry point reports the platform doesn't support WHP. Lets
// callers (pkg/vmm hypervisor selection) compile across the matrix
// without per-platform import guards.
package whp

import "errors"

type PartitionHandle uintptr
type CapabilityCode uint32
type PartitionPropertyCode uint32
type MapGpaRangeFlags uint32
type HResult uint32

const (
	CapHypervisorPresent CapabilityCode = 0
	CapFeatures          CapabilityCode = 1
	CapExtendedVMExits   CapabilityCode = 2
	CapProcessorVendor   CapabilityCode = 0x1000
	CapProcessorFeatures CapabilityCode = 0x1001

	PropExtendedVMExits   PartitionPropertyCode = 0x1
	PropProcessorFeatures PartitionPropertyCode = 0x1001
	PropCpuidExitList     PartitionPropertyCode = 0x2
	PropProcessorCount    PartitionPropertyCode = 0x1fff

	MapGpaNone    MapGpaRangeFlags = 0
	MapGpaRead    MapGpaRangeFlags = 1
	MapGpaWrite   MapGpaRangeFlags = 2
	MapGpaExecute MapGpaRangeFlags = 4
	MapGpaTrack   MapGpaRangeFlags = 8
)

var errNotWindows = errors.New("whp: WinHvPlatform.dll is only available on Windows")

func (h HResult) Error() string { return "whp: not a Windows host" }

func Available() bool                                                                         { return false }
func HypervisorPresent() (bool, error)                                                        { return false, errNotWindows }
func CreatePartition() (PartitionHandle, error)                                               { return 0, errNotWindows }
func DeletePartition(PartitionHandle) error                                                   { return errNotWindows }
func SetupPartition(PartitionHandle) error                                                    { return errNotWindows }
func SetPartitionPropertyU32(PartitionHandle, PartitionPropertyCode, uint32) error            { return errNotWindows }
func MapGpaRange(PartitionHandle, []byte, uint64, MapGpaRangeFlags) error                     { return errNotWindows }
func UnmapGpaRange(PartitionHandle, uint64, uint64) error                                     { return errNotWindows }
func CreateVirtualProcessor(PartitionHandle, uint32) error                                    { return errNotWindows }
func DeleteVirtualProcessor(PartitionHandle, uint32) error                                    { return errNotWindows }
func CancelRunVirtualProcessor(PartitionHandle, uint32) error                                 { return errNotWindows }
