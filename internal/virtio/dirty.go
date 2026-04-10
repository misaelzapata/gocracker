//go:build linux

package virtio

import (
	"os"
	"sync"
)

// DirtyTracker tracks host-side guest-memory writes that bypass KVM dirty logging,
// such as device DMA into virtqueue buffers and used rings.
type DirtyTracker struct {
	mu       sync.Mutex
	memSize  uint64
	pageSize uint64
	bits     []uint64
}

func NewDirtyTracker(memSize uint64) *DirtyTracker {
	pageSize := uint64(os.Getpagesize())
	pageCount := (memSize + pageSize - 1) / pageSize
	wordCount := (pageCount + 63) / 64
	return &DirtyTracker{
		memSize:  memSize,
		pageSize: pageSize,
		bits:     make([]uint64, wordCount),
	}
}

func (d *DirtyTracker) Mark(addr, length uint64) {
	if d == nil || length == 0 || addr >= d.memSize {
		return
	}
	end := addr + length
	if end < addr || end > d.memSize {
		end = d.memSize
	}
	startPage := addr / d.pageSize
	endPage := (end - 1) / d.pageSize

	d.mu.Lock()
	defer d.mu.Unlock()
	for page := startPage; page <= endPage; page++ {
		word := page / 64
		bit := page % 64
		d.bits[word] |= 1 << bit
	}
}

func (d *DirtyTracker) SnapshotAndReset() []uint64 {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]uint64, len(d.bits))
	copy(out, d.bits)
	clear(d.bits)
	return out
}

func (d *DirtyTracker) Reset() {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	clear(d.bits)
}

func (d *DirtyTracker) PageSize() uint64 {
	if d == nil {
		return uint64(os.Getpagesize())
	}
	return d.pageSize
}
