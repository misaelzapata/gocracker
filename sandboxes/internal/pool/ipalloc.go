package pool

import (
	"errors"
	"fmt"
	"net"
	"sync"
)

// IPAllocator hands out non-overlapping /30 subnets from a base range.
// Each /30 holds 4 IPs: .0 (network), .1 (gateway), .2 (guest),
// .3 (broadcast) — the same shape Firecracker uses for its built-in
// network mode, so the host-side TAP setup that already runs for
// every booted VM transfers unchanged.
//
// The default base 198.19.0.0/16 is from RFC 2544 (benchmark testing
// range) — guaranteed-private, not in any public allocation, and not
// commonly used by container engines (which favor 172.16.0.0/12 and
// 10.0.0.0/8). 16 384 /30 slots fits a single sandboxd instance with
// orders-of-magnitude headroom over MaxPaused=8 / typical pool sizes.
//
// Implementation: a packed bitmap (16 384 bits = 2 KiB) plus a
// next-search hint that tracks the lowest possibly-free index. O(1)
// amortized Allocate; O(n) worst case when the bitmap is heavily
// fragmented (rare — Free updates the hint).
//
// The MAC address is derived from the IP: 02:XX:XX:XX:XX:XX where
// the last 4 bytes are the IPv4 octets. The 02 prefix means
// "locally administered, unicast" — collision-free against any
// physical NIC.
type IPAllocator struct {
	base    *net.IPNet // base range (e.g. 198.19.0.0/16)
	maskLen int        // /30 = 30
	slots   int        // total slots in base / 2^(maskLen-baseMask)

	mu      sync.Mutex
	bitmap  []uint64 // 1 = allocated, 0 = free
	nextHint int      // first index with possible free bit
}

// NewIPAllocator builds an allocator over base, slicing it into /maskLen
// subnets. Returns an error if base isn't IPv4 or if maskLen is too
// small for the base range (no slots).
//
// Typical use: NewIPAllocator("198.19.0.0/16", 30) → 16 384 /30 slots.
func NewIPAllocator(baseCIDR string, maskLen int) (*IPAllocator, error) {
	_, base, err := net.ParseCIDR(baseCIDR)
	if err != nil {
		return nil, fmt.Errorf("ipalloc: parse base %q: %w", baseCIDR, err)
	}
	if base.IP.To4() == nil {
		return nil, fmt.Errorf("ipalloc: base %q is not IPv4", baseCIDR)
	}
	baseMaskLen, _ := base.Mask.Size()
	if maskLen <= baseMaskLen || maskLen > 32 {
		return nil, fmt.Errorf("ipalloc: maskLen=%d must be in (%d,32]", maskLen, baseMaskLen)
	}
	slots := 1 << (maskLen - baseMaskLen)
	words := (slots + 63) / 64
	return &IPAllocator{
		base:    base,
		maskLen: maskLen,
		slots:   slots,
		bitmap:  make([]uint64, words),
	}, nil
}

// LeaseAddr is what Allocate hands back: a CIDR-formatted guest IP,
// a bare gateway IP, and a derived MAC. Pass straight into
// LeaseSpec{IP, Gateway, MAC}.
type LeaseAddr struct {
	Slot    int    // index into the bitmap; pass to Free
	IP      string // CIDR, e.g. "198.19.100.2/30"
	Gateway string // bare, e.g. "198.19.100.1"
	MAC     string // colon-hex, derived from IP
}

// ErrIPExhausted is returned when every slot in the range is taken.
// Operationally this means raise MaxPaused or expand the base range
// (a /16 holds 16 384 /30s — running out is a misconfiguration, not
// a transient failure).
var ErrIPExhausted = errors.New("ipalloc: all slots allocated")

// Allocate picks the lowest free slot, marks it taken, and returns
// the corresponding LeaseAddr. O(words) worst case where words is
// the bitmap size in 64-bit chunks (256 for /16 → /30); in practice
// the nextHint hint puts us at O(1) amortized.
func (a *IPAllocator) Allocate() (LeaseAddr, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for w := a.nextHint / 64; w < len(a.bitmap); w++ {
		if a.bitmap[w] == ^uint64(0) {
			continue // word fully allocated
		}
		// Find the lowest 0-bit in this word, but skip any bits
		// below nextHint within this word so we don't double-issue
		// a slot that was just freed in the SAME word above the hint.
		startBit := 0
		if w*64 < a.nextHint {
			startBit = a.nextHint - w*64
		}
		for b := startBit; b < 64; b++ {
			if a.bitmap[w]&(uint64(1)<<b) == 0 {
				slot := w*64 + b
				if slot >= a.slots {
					return LeaseAddr{}, ErrIPExhausted
				}
				a.bitmap[w] |= uint64(1) << b
				a.nextHint = slot + 1
				return a.leaseFromSlot(slot), nil
			}
		}
	}
	return LeaseAddr{}, ErrIPExhausted
}

// Free marks slot as available. No-op (and no error) if the slot was
// already free — Free is intended to be safe for retry / shutdown
// paths where double-free is hard to rule out.
func (a *IPAllocator) Free(slot int) {
	if slot < 0 || slot >= a.slots {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	w := slot / 64
	b := slot % 64
	a.bitmap[w] &^= uint64(1) << b
	if slot < a.nextHint {
		a.nextHint = slot
	}
}

// InUse returns the count of allocated slots. O(words). Used by
// tests + slice 7's expvar gauges.
func (a *IPAllocator) InUse() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, w := range a.bitmap {
		n += popcount(w)
	}
	return n
}

// Capacity returns the total slot count.
func (a *IPAllocator) Capacity() int { return a.slots }

// leaseFromSlot computes the LeaseAddr for slot. Called only with
// a.mu held (or from a non-mutating context that owns the allocator).
//
// /30 layout for slot N (offset = N << 2 IPs from the base):
//
//	offset+0 — network
//	offset+1 — gateway (host TAP side)
//	offset+2 — guest IP
//	offset+3 — broadcast
func (a *IPAllocator) leaseFromSlot(slot int) LeaseAddr {
	hostsPerSlot := 1 << (32 - a.maskLen)
	baseInt := ipv4ToUint32(a.base.IP.To4())
	netInt := baseInt + uint32(slot*hostsPerSlot)
	gw := uint32ToIPv4(netInt + 1)
	guest := uint32ToIPv4(netInt + 2)
	guestCIDR := fmt.Sprintf("%s/%d", guest.String(), a.maskLen)
	return LeaseAddr{
		Slot:    slot,
		IP:      guestCIDR,
		Gateway: gw.String(),
		MAC:     macFromIP(guest),
	}
}

// macFromIP derives a locally-administered unicast MAC from an IPv4.
// Format: 02:00:<a>:<b>:<c>:<d> where a-d are the IP octets. Local
// admin bit is set (0x02), unicast (low bit of first byte = 0).
// Deterministic so the same guest IP always gets the same MAC,
// which keeps the guest's ARP cache valid across pause/resume/restore.
func macFromIP(ip net.IP) string {
	v4 := ip.To4()
	return fmt.Sprintf("02:00:%02x:%02x:%02x:%02x", v4[0], v4[1], v4[2], v4[3])
}

func ipv4ToUint32(ip net.IP) uint32 {
	v4 := ip.To4()
	return uint32(v4[0])<<24 | uint32(v4[1])<<16 | uint32(v4[2])<<8 | uint32(v4[3])
}

func uint32ToIPv4(n uint32) net.IP {
	return net.IPv4(byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
}

// popcount returns the number of set bits in v. Standard SWAR
// trick — Go's math/bits.OnesCount64 would do, kept inline so the
// allocator has zero std-lib coupling beyond net.
func popcount(v uint64) int {
	v = v - ((v >> 1) & 0x5555555555555555)
	v = (v & 0x3333333333333333) + ((v >> 2) & 0x3333333333333333)
	v = (v + (v >> 4)) & 0x0F0F0F0F0F0F0F0F
	return int((v * 0x0101010101010101) >> 56)
}
