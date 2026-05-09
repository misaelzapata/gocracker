//go:build linux

package vmm

import (
	"testing"

	"github.com/gocracker/gocracker/internal/kvm"
)

// TestSregsRoundTrip checks the SegmentRegisters↔kvm.Sregs converters
// preserve every field. The adapter is the boundary between the portable
// Hypervisor abstraction and KVM's UAPI shape; a single dropped field
// here would surface as a guest boot failure several layers up.
func TestSregsRoundTrip(t *testing.T) {
	original := kvm.Sregs{
		CS:  kvm.Segment{Base: 0x1000, Limit: 0xffff, Selector: 0x10, Type: 11, Present: 1, S: 1, L: 1, G: 1},
		DS:  kvm.Segment{Base: 0x2000, Limit: 0x1fff, Selector: 0x18, Type: 3, Present: 1, S: 1, DB: 1, G: 1},
		ES:  kvm.Segment{Selector: 0x18},
		FS:  kvm.Segment{Selector: 0x18},
		GS:  kvm.Segment{Selector: 0x18},
		SS:  kvm.Segment{Selector: 0x18},
		TR:  kvm.Segment{Type: 11, Present: 1, S: 0},
		LDT: kvm.Segment{Unusable: 1},
		GDT: kvm.DTTR{Base: 0x500, Limit: 31},
		IDT: kvm.DTTR{Base: 0x520, Limit: 7},
		CR0: 0x80050033,
		CR3: 0x9000,
		CR4: 0x000006a0,
		EFER: 0x500,
		ApicBase:        0xfee00900,
		InterruptBitmap: [4]uint64{0x1, 0x2, 0x3, 0x4},
	}

	round := sregsToKVM(sregsFromKVM(original))

	if round != original {
		t.Fatalf("Sregs round-trip lost data\noriginal: %#v\nround:    %#v", original, round)
	}
}

// TestRegsRoundTrip pins down the layout-equivalence between Registers
// and kvm.Regs. The conversion is a single struct cast today; the test
// makes sure that holds even if either struct gains a field. If the
// shapes diverge, this fails to compile.
func TestRegsRoundTrip(t *testing.T) {
	original := kvm.Regs{
		RAX: 0x1, RBX: 0x2, RCX: 0x3, RDX: 0x4,
		RSI: 0x5, RDI: 0x6, RSP: 0x7, RBP: 0x8,
		R8: 0x9, R9: 0xa, R10: 0xb, R11: 0xc,
		R12: 0xd, R13: 0xe, R14: 0xf, R15: 0x10,
		RIP: 0x100000, RFLAGS: 0x202,
	}

	hv := Registers(original)
	round := kvm.Regs(hv)

	if round != original {
		t.Fatalf("Regs round-trip lost data\noriginal: %#v\nround:    %#v", original, round)
	}
}

// TestKVMMemFlagsTranslation is a regression test for the bit layout
// kvmMemFlagsFromHV emits. KVM_MEM_LOG_DIRTY_PAGES = 1<<0 and
// KVM_MEM_READONLY = 1<<1; misaligned flags produce silent guest faults.
func TestKVMMemFlagsTranslation(t *testing.T) {
	cases := []struct {
		in       MemFlags
		wantBits uint32
	}{
		{MemRWX, 0},                       // RW: no log, not readonly
		{MemRead, 1 << 1},                  // RO
		{MemRead | MemTrackDirty, 1<<0 | 1<<1},
		{MemRWX | MemTrackDirty, 1 << 0},   // RW + dirty log
	}
	for _, c := range cases {
		got := kvmMemFlagsFromHV(c.in)
		if got != c.wantBits {
			t.Errorf("kvmMemFlagsFromHV(%#b) = %#b; want %#b", uint32(c.in), got, c.wantBits)
		}
	}
}
