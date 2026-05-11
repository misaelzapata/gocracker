//go:build windows

package whp

import "testing"

// TestAllocateGuestMemoryRoundTrip exercises the VirtualAlloc-backed
// GuestMemory and proves the slice header points at the live region:
// we write into the host slice, the bytes are visible in subsequent
// reads. No partition involved.
func TestAllocateGuestMemoryRoundTrip(t *testing.T) {
	const sz = 4 * 1024 * 1024 // 4 MiB
	mem, err := AllocateGuestMemory(sz)
	if err != nil {
		t.Fatalf("AllocateGuestMemory: %v", err)
	}
	t.Cleanup(func() {
		if err := mem.Close(); err != nil {
			t.Errorf("GuestMemory.Close: %v", err)
		}
	})
	if mem.Base == 0 {
		t.Fatal("GuestMemory.Base is 0")
	}
	if mem.Size != sz {
		t.Fatalf("Size=%d, want %d", mem.Size, sz)
	}
	buf := mem.HostBytes()
	if len(buf) != sz {
		t.Fatalf("HostBytes len=%d, want %d", len(buf), sz)
	}
	// Write a marker at the start, middle, and end. Read it back via a
	// fresh HostBytes() call — the slice header should still alias the
	// VirtualAlloc'd region.
	buf[0] = 0xAB
	buf[sz/2] = 0xCD
	buf[sz-1] = 0xEF
	again := mem.HostBytes()
	if again[0] != 0xAB || again[sz/2] != 0xCD || again[sz-1] != 0xEF {
		t.Fatalf("HostBytes() returned a stale view: [%#x %#x %#x]", again[0], again[sz/2], again[sz-1])
	}
}

// TestMapGuestMemoryIntoPartition is the real smoke: allocate guest RAM,
// create a partition with 1 vCPU, configure + setup, then map the RAM
// at guest physical address 0 with full RWX. Validates the entire
// allocate → MapGpaRange path on a live hypervisor.
func TestMapGuestMemoryIntoPartition(t *testing.T) {
	if !Available() {
		t.Skip("WinHvPlatform.dll not loadable")
	}
	present, err := HypervisorPresent()
	if err != nil || !present {
		t.Skip("Hypervisor Platform feature not enabled on this host")
	}

	mem, err := AllocateGuestMemory(2 * 1024 * 1024) // 2 MiB guest RAM
	if err != nil {
		t.Fatalf("AllocateGuestMemory: %v", err)
	}
	t.Cleanup(func() { mem.Close() })

	h, err := CreatePartition()
	if err != nil {
		t.Fatalf("CreatePartition: %v", err)
	}
	t.Cleanup(func() { DeletePartition(h) })

	if err := SetPartitionPropertyU32(h, PropProcessorCount, 1); err != nil {
		t.Fatalf("SetPartitionProperty(ProcessorCount=1): %v", err)
	}
	if err := SetupPartition(h); err != nil {
		t.Fatalf("SetupPartition: %v", err)
	}

	if err := MapGuestMemory(h, mem, 0); err != nil {
		t.Fatalf("MapGuestMemory(gpa=0, size=%d): %v", mem.Size, err)
	}
	t.Logf("mapped %d bytes of guest RAM at GPA 0 in partition %#x", mem.Size, uintptr(h))

	if err := UnmapGpaRange(h, 0, mem.Size); err != nil {
		t.Fatalf("UnmapGpaRange: %v", err)
	}
}
