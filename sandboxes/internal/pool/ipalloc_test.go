package pool

import (
	"errors"
	"net"
	"sync"
	"testing"
)

func TestNewIPAllocator_DefaultRange(t *testing.T) {
	a, err := NewIPAllocator("198.19.0.0/16", 30)
	if err != nil {
		t.Fatalf("NewIPAllocator: %v", err)
	}
	if got := a.Capacity(); got != 16384 {
		t.Errorf("Capacity = %d, want 16384 (/16 → /30)", got)
	}
	if got := a.InUse(); got != 0 {
		t.Errorf("fresh allocator InUse = %d, want 0", got)
	}
}

func TestNewIPAllocator_RejectsBadInput(t *testing.T) {
	cases := []struct {
		name    string
		base    string
		maskLen int
	}{
		{"nonsense CIDR", "not-a-cidr", 30},
		{"IPv6 base", "fd00::/16", 30},
		{"maskLen too small", "198.19.0.0/16", 16},
		{"maskLen below base", "198.19.0.0/24", 20},
		{"maskLen too large", "198.19.0.0/16", 33},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewIPAllocator(tc.base, tc.maskLen); err == nil {
				t.Fatalf("expected error for base=%q mask=%d", tc.base, tc.maskLen)
			}
		})
	}
}

func TestAllocate_FirstSlotShape(t *testing.T) {
	a, _ := NewIPAllocator("198.19.0.0/16", 30)
	la, err := a.Allocate()
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if la.Slot != 0 {
		t.Errorf("Slot = %d, want 0", la.Slot)
	}
	if la.IP != "198.19.0.2/30" {
		t.Errorf("IP = %q, want 198.19.0.2/30", la.IP)
	}
	if la.Gateway != "198.19.0.1" {
		t.Errorf("Gateway = %q, want 198.19.0.1", la.Gateway)
	}
	if la.MAC != "02:00:c6:13:00:02" {
		t.Errorf("MAC = %q, want 02:00:c6:13:00:02", la.MAC)
	}
}

func TestAllocate_SecondSlotIsAdjacent(t *testing.T) {
	a, _ := NewIPAllocator("198.19.0.0/16", 30)
	_, _ = a.Allocate() // slot 0: 198.19.0.0/30 → guest .2
	la, err := a.Allocate()
	if err != nil {
		t.Fatalf("Allocate #2: %v", err)
	}
	if la.Slot != 1 {
		t.Errorf("Slot = %d, want 1", la.Slot)
	}
	// Slot 1 = 198.19.0.4/30 → gw .5, guest .6
	if la.IP != "198.19.0.6/30" {
		t.Errorf("IP = %q, want 198.19.0.6/30", la.IP)
	}
	if la.Gateway != "198.19.0.5" {
		t.Errorf("Gateway = %q, want 198.19.0.5", la.Gateway)
	}
}

func TestAllocate_CarriesOverOctet(t *testing.T) {
	a, _ := NewIPAllocator("198.19.0.0/16", 30)
	// Slot 64 = offset 256 = 198.19.1.0/30 → gw .1, guest .2 in the next /24.
	for i := 0; i < 64; i++ {
		_, _ = a.Allocate()
	}
	la, _ := a.Allocate()
	if la.Slot != 64 {
		t.Errorf("Slot = %d, want 64", la.Slot)
	}
	if la.IP != "198.19.1.2/30" {
		t.Errorf("IP = %q, want 198.19.1.2/30 (octet carry)", la.IP)
	}
}

func TestFreeAndReallocate_ReusesSlot(t *testing.T) {
	a, _ := NewIPAllocator("198.19.0.0/16", 30)
	la1, _ := a.Allocate()
	la2, _ := a.Allocate()
	a.Free(la1.Slot)
	la3, _ := a.Allocate()
	if la3.Slot != la1.Slot {
		t.Errorf("Free+Allocate slot=%d, want reused %d", la3.Slot, la1.Slot)
	}
	if la3.IP != la1.IP {
		t.Errorf("Reused slot IP=%q, want %q", la3.IP, la1.IP)
	}
	// la2 should still be allocated.
	if got := a.InUse(); got != 2 {
		t.Errorf("InUse = %d, want 2 (la2 still held + la3 reissued)", got)
	}
	_ = la2
}

func TestFree_OutOfRange_Noop(t *testing.T) {
	a, _ := NewIPAllocator("198.19.0.0/16", 30)
	a.Free(-1)         // should not panic
	a.Free(99999)      // should not panic
	if a.InUse() != 0 {
		t.Errorf("Free out-of-range bumped InUse to %d", a.InUse())
	}
}

func TestFree_DoubleFree_Noop(t *testing.T) {
	a, _ := NewIPAllocator("198.19.0.0/16", 30)
	la, _ := a.Allocate()
	a.Free(la.Slot)
	a.Free(la.Slot) // second free must not break the bitmap
	if a.InUse() != 0 {
		t.Errorf("InUse after double-free = %d, want 0", a.InUse())
	}
	la2, _ := a.Allocate()
	if la2.Slot != la.Slot {
		t.Errorf("post double-free Allocate slot=%d, want reused %d", la2.Slot, la.Slot)
	}
}

func TestAllocate_Exhaustion(t *testing.T) {
	// Tiny range: /28 → /30 = 4 slots.
	a, err := NewIPAllocator("10.0.0.0/28", 30)
	if err != nil {
		t.Fatalf("NewIPAllocator: %v", err)
	}
	for i := 0; i < 4; i++ {
		if _, err := a.Allocate(); err != nil {
			t.Fatalf("Allocate #%d: %v", i, err)
		}
	}
	_, err = a.Allocate()
	if !errors.Is(err, ErrIPExhausted) {
		t.Fatalf("post-exhaustion err = %v, want ErrIPExhausted", err)
	}
}

func TestAllocate_AllSlotsHaveDistinctIPs(t *testing.T) {
	a, _ := NewIPAllocator("10.0.0.0/24", 30) // 64 slots
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		la, err := a.Allocate()
		if err != nil {
			t.Fatalf("Allocate #%d: %v", i, err)
		}
		if seen[la.IP] {
			t.Fatalf("duplicate IP %q at slot %d", la.IP, i)
		}
		seen[la.IP] = true
	}
}

// TestConcurrentAllocate verifies the allocator under bus contention:
// 100 goroutines each grab 10 slots. Total = 1000 distinct slots out
// of a /20 → /30 range (1024 slots). No duplicates allowed.
func TestConcurrentAllocate(t *testing.T) {
	a, err := NewIPAllocator("10.0.0.0/20", 30)
	if err != nil {
		t.Fatalf("NewIPAllocator: %v", err)
	}
	const goroutines = 100
	const perGoroutine = 10
	type result struct {
		ips []string
		err error
	}
	results := make(chan result, goroutines)
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := result{}
			for i := 0; i < perGoroutine; i++ {
				la, err := a.Allocate()
				if err != nil {
					r.err = err
					results <- r
					return
				}
				r.ips = append(r.ips, la.IP)
			}
			results <- r
		}()
	}
	wg.Wait()
	close(results)

	all := map[string]int{}
	for r := range results {
		if r.err != nil {
			t.Errorf("goroutine errored: %v", r.err)
			continue
		}
		for _, ip := range r.ips {
			all[ip]++
		}
	}
	if len(all) != goroutines*perGoroutine {
		t.Errorf("got %d distinct IPs, want %d", len(all), goroutines*perGoroutine)
	}
	for ip, n := range all {
		if n != 1 {
			t.Errorf("IP %q allocated %d times, want exactly 1", ip, n)
		}
	}
}

// TestReuseAfterFree_HintCorrect: allocate 100, free middle 10, next
// 10 Allocates should refill the freed range — exercises the
// nextHint reset on Free path.
func TestReuseAfterFree_HintCorrect(t *testing.T) {
	a, _ := NewIPAllocator("10.0.0.0/24", 30) // 64 slots
	all := make([]LeaseAddr, 30)
	for i := range all {
		la, _ := a.Allocate()
		all[i] = la
	}
	// Free slots 5..14 (10 slots).
	for i := 5; i < 15; i++ {
		a.Free(all[i].Slot)
	}
	if got := a.InUse(); got != 20 {
		t.Errorf("post-free InUse = %d, want 20", got)
	}
	// Next 10 Allocates should reuse 5..14.
	for i := 5; i < 15; i++ {
		la, err := a.Allocate()
		if err != nil {
			t.Fatalf("Allocate after free: %v", err)
		}
		if la.Slot != i {
			t.Errorf("Allocate after free slot=%d, want %d", la.Slot, i)
		}
	}
}

func TestMacFromIP_FormatStable(t *testing.T) {
	cases := map[string]string{
		"198.19.0.2":     "02:00:c6:13:00:02",
		"198.19.255.254": "02:00:c6:13:ff:fe",
		"10.0.0.1":       "02:00:0a:00:00:01",
	}
	for ipStr, want := range cases {
		t.Run(ipStr, func(t *testing.T) {
			ip := net.ParseIP(ipStr)
			if ip == nil {
				t.Fatalf("net.ParseIP(%q) returned nil", ipStr)
			}
			if got := macFromIP(ip); got != want {
				t.Errorf("macFromIP(%s) = %q, want %q", ipStr, got, want)
			}
		})
	}
}
