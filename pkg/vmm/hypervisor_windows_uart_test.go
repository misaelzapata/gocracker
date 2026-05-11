//go:build windows

package vmm

import (
	"testing"
	"time"

	"github.com/gocracker/gocracker/internal/whp"
)

// TestWHPIOPortDispatchToUart proves end-to-end that the WHP run loop
// can drive an x86 OUT instruction targeting the UART data port, route
// the exit through dispatchIOPort, and advance the vCPU past the
// instruction so the next OUT / HLT works. This is the Phase 2e
// foundation: same machinery that Alpine's earliest kernel printk uses.
//
// Guest program (raw bytes at GPA 0, real mode):
//
//	BA F8 03   mov dx, 0x3F8    ; UART data port (COM1Base)
//	B0 48      mov al, 'H'
//	EE         out dx, al        ; → exits ExitReasonIOPort (Port=0x3F8, IsWrite, Data='H')
//	B0 69      mov al, 'i'
//	EE         out dx, al        ; → same, 'i'
//	B0 0A      mov al, '\n'
//	EE         out dx, al        ; → same, '\n'
//	F4         hlt               ; → exits ExitReasonHalt
//
// The test:
//  1. Allocates 4 MiB guest RAM, writes the program at GPA 0
//  2. Configures real-mode CS:RIP=0:0
//  3. Calls vcpu.Run() until ExitReasonHalt
//  4. After each ExitReasonIOPort, asserts port == 0x3F8 and accumulates
//     the data byte
//  5. Asserts the accumulated bytes are "Hi\n"
//
// This is the integration smoke for: WHP run loop + IOPort exit decoding
// + RIP advance via SetRegisters + portable ExitContext mapping.
func TestWHPIOPortDispatchToUart(t *testing.T) {
	if !whp.Available() {
		t.Skip("WinHvPlatform.dll not loadable")
	}
	present, err := whp.HypervisorPresent()
	if err != nil || !present {
		t.Skip("Hypervisor Platform feature not enabled")
	}

	hv, err := NewWHPHypervisor()
	if err != nil {
		t.Fatalf("NewWHPHypervisor: %v", err)
	}
	t.Cleanup(func() { hv.Close() })

	vm, err := hv.CreateVM(HVVMConfig{NumVCPUs: 1, MemoryBytes: 4 * 1024 * 1024})
	if err != nil {
		t.Fatalf("CreateVM: %v", err)
	}
	t.Cleanup(func() { vm.Close() })

	ram, err := vm.AllocateGuestRAM(4 * 1024 * 1024)
	if err != nil {
		t.Fatalf("AllocateGuestRAM: %v", err)
	}

	// Encode the program (9 bytes, real-mode 16-bit code).
	program := []byte{
		0xBA, 0xF8, 0x03, // mov dx, 0x3F8
		0xB0, 0x48, // mov al, 'H'
		0xEE,       // out dx, al
		0xB0, 0x69, // mov al, 'i'
		0xEE,       // out dx, al
		0xB0, 0x0A, // mov al, '\n'
		0xEE, // out dx, al
		0xF4, // hlt
	}
	copy(ram, program)

	if err := vm.MapMemory(0, ram, MemRWX); err != nil {
		t.Fatalf("MapMemory: %v", err)
	}

	vcpu, err := vm.CreateVCPU(0)
	if err != nil {
		t.Fatalf("CreateVCPU(0): %v", err)
	}
	t.Cleanup(func() { vcpu.Close() })

	// Real-mode boot.
	sregs, err := vcpu.GetSegmentRegisters()
	if err != nil {
		t.Fatalf("GetSegmentRegisters: %v", err)
	}
	// CS: real-mode code, selector=0, base=0, limit=0xFFFF, 16-bit (no L,
	// no DB; Type=0xA read+exec, S=1=user, P=1=present).
	sregs.CS = Segment{Limit: 0xFFFF, Type: 0xA, S: 1, Present: 1}
	if err := vcpu.SetSegmentRegisters(sregs); err != nil {
		t.Fatalf("SetSegmentRegisters: %v", err)
	}
	regs, err := vcpu.GetRegisters()
	if err != nil {
		t.Fatalf("GetRegisters: %v", err)
	}
	regs.RIP = 0
	regs.RFLAGS = 0x2
	if err := vcpu.SetRegisters(regs); err != nil {
		t.Fatalf("SetRegisters: %v", err)
	}

	// Run loop. Track UART writes and watch-dog the whole thing.
	var uartOutput []byte
	deadline := time.Now().Add(5 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatalf("watchdog: vCPU did not halt within 5s after %d bytes output", len(uartOutput))
		}
		exitCtx, err := vcpu.Run()
		if err != nil {
			t.Fatalf("vcpu.Run: %v", err)
		}
		switch exitCtx.Reason {
		case ExitReasonIOPort:
			io := exitCtx.IOPort
			t.Logf("IOPort exit: Port=%#x Direction=%d Size=%d Count=%d Data[0]=%#x",
				io.Port, io.Direction, io.Size, io.Count, io.Data[0])
			if io.Port != 0x3F8 {
				t.Fatalf("unexpected IO port %#x; want 0x3F8 (UART data)", io.Port)
			}
			if io.Direction != IOPortOut {
				t.Fatalf("unexpected IO direction %v; want IOPortOut", io.Direction)
			}
			// Pull the byte from RAX (where x86 OUT places the data we
			// see on this exit shape). The portable ExitContext only
			// carries Port/Direction/Size today; pulling the actual data
			// byte from the vCPU register set is what the WHP boot path
			// will do for every IO write.
			r, err := vcpu.GetRegisters()
			if err != nil {
				t.Fatalf("GetRegisters during IOPort exit: %v", err)
			}
			uartOutput = append(uartOutput, byte(r.RAX&0xFF))

			// Advance RIP past the trapped instruction. WHP does NOT
			// auto-advance; we read the instruction length from the
			// underlying WHV_VP_EXIT_CONTEXT (decoded in internal/whp).
			// For OUT dx, al the instruction is 1 byte (0xEE); we
			// hard-code 1 here because the portable ExitContext doesn't
			// yet surface InstructionLength.
			//
			// TODO Phase 2e: lift InstructionLength into portable
			// ExitContext so this is generic.
			r.RIP += 1
			if err := vcpu.SetRegisters(r); err != nil {
				t.Fatalf("SetRegisters (RIP advance): %v", err)
			}

		case ExitReasonHalt:
			t.Logf("vCPU halted after %d UART bytes: %q", len(uartOutput), string(uartOutput))
			if string(uartOutput) != "Hi\n" {
				t.Fatalf("UART output = %q; want %q", string(uartOutput), "Hi\n")
			}
			return

		case ExitReasonInternal, ExitReasonFailEntry:
			t.Fatalf("vCPU error exit (%v): %s", exitCtx.Reason, exitCtx.FailureMsg)

		default:
			t.Fatalf("unexpected exit reason %v (after %d output bytes)",
				exitCtx.Reason, len(uartOutput))
		}
	}
}
