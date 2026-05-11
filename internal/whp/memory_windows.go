//go:build windows

package whp

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// GuestMemory is a chunk of host virtual address space allocated for guest
// RAM. The underlying pages MUST come from windows.VirtualAlloc (or a
// similar pinning allocator) — Go-managed slices cannot be passed to
// WHvMapGpaRange because the Go GC may move them, leaving the hypervisor
// pointing at freed pages.
//
// Lifetime: the host allocation is owned by GuestMemory. Calling Close
// releases it via VirtualFree; do NOT call Close while the range is
// still mapped into a partition (call partition.UnmapMemory first).
type GuestMemory struct {
	// Base is the host virtual address of the first page. Valid until
	// Close. The slice form (HostBytes) is convenient for byte-level
	// writes (e.g. copying a kernel image into guest RAM before boot).
	Base uintptr

	// Size is the allocation size in bytes. Always a multiple of
	// the system page size (4 KiB on x86_64).
	Size uint64

	// hostBytes is the same memory exposed as a Go slice. The slice
	// header points to the VirtualAlloc'd region, so Go code can do
	// copy(hostBytes, kernelImage) and the hypervisor sees the bytes
	// without any extra copy. NOT safe to grow/realloc.
	hostBytes []byte
}

// HostBytes returns the host memory as a byte slice. Mutating the slice
// is visible to the guest immediately (no flush needed — these are real
// pages, not a copy). The slice MUST NOT be appended to or have its
// header rewritten; doing so disassociates it from the VirtualAlloc'd
// region and breaks subsequent guest reads.
func (g *GuestMemory) HostBytes() []byte { return g.hostBytes }

// AllocateGuestMemory reserves and commits `size` bytes of host virtual
// memory suitable for backing a guest RAM region. The allocation comes
// from windows.VirtualAlloc with MEM_COMMIT|MEM_RESERVE and
// PAGE_READWRITE — guest reads/writes both work, and the pages are
// committed (not lazy) so a guest page fault doesn't surprise us.
//
// size must be a multiple of the 4 KiB page size; it's rounded up.
func AllocateGuestMemory(size uint64) (*GuestMemory, error) {
	// Round up to 4 KiB. WHP requires 4 KiB-aligned mappings; an
	// arbitrary VirtualAlloc respects the system allocation granularity
	// (64 KiB) for the base, which is even stricter.
	const pageSize = 4096
	if size == 0 {
		return nil, fmt.Errorf("whp.AllocateGuestMemory: size must be > 0")
	}
	if size%pageSize != 0 {
		size = (size + pageSize - 1) &^ (pageSize - 1)
	}

	base, err := windows.VirtualAlloc(
		0, // let the kernel pick an address
		uintptr(size),
		windows.MEM_COMMIT|windows.MEM_RESERVE,
		windows.PAGE_READWRITE,
	)
	if err != nil {
		return nil, fmt.Errorf("VirtualAlloc(%d bytes): %w", size, err)
	}
	// Build a Go slice header that references the VirtualAlloc'd region
	// directly. unsafe.Slice (Go 1.17+) gives us a slice without a copy.
	// The slice's backing array is the VirtualAlloc region; callers
	// must not append/grow.
	hostBytes := unsafe.Slice((*byte)(unsafe.Pointer(base)), size)
	return &GuestMemory{
		Base:      base,
		Size:      size,
		hostBytes: hostBytes,
	}, nil
}

// Close releases the host memory. Safe to call multiple times.
func (g *GuestMemory) Close() error {
	if g == nil || g.Base == 0 {
		return nil
	}
	// VirtualFree with MEM_RELEASE requires size = 0 (release the
	// entire reservation rooted at Base).
	if err := windows.VirtualFree(g.Base, 0, windows.MEM_RELEASE); err != nil {
		return fmt.Errorf("VirtualFree: %w", err)
	}
	g.Base = 0
	g.hostBytes = nil
	return nil
}

// MapGuestMemory maps a previously-allocated GuestMemory into a partition
// at the given guest physical address with full RWX access. This is the
// common case for boot RAM; finer-grained flags (read-only ROM, dirty
// tracking) go through MapGpaRange directly.
func MapGuestMemory(h PartitionHandle, mem *GuestMemory, gpa uint64) error {
	if mem == nil || mem.Base == 0 {
		return fmt.Errorf("whp.MapGuestMemory: memory not allocated")
	}
	if err := loadDLL(); err != nil {
		return err
	}
	hr, _, _ := procMapGpaRange.Call(
		uintptr(h),
		mem.Base,
		uintptr(gpa),
		uintptr(mem.Size),
		uintptr(MapGpaRead|MapGpaWrite|MapGpaExecute),
	)
	if HResult(hr) != sOK {
		return HResult(hr)
	}
	return nil
}
