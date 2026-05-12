//go:build windows

package vmm

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"

	"github.com/gocracker/gocracker/internal/loader"
)

// WHPBootConfig is the minimum configuration to boot a Linux kernel on
// Windows via the WHP backend. Phase 2e — the first end-to-end path
// from `gocracker.exe` to a Linux kernel running on Hyper-V.
//
// This bypasses the legacy KVM-coupled run loop in vmm.go entirely; it
// uses the Hypervisor / HVVM / HVVCPU abstraction directly so the same
// integration applies to any future backend (HVF on macOS, etc.).
type WHPBootConfig struct {
	// KernelPath points to a bzImage or ELF vmlinux. The loader
	// auto-detects the format and handles bzImage decompression.
	KernelPath string

	// Cmdline is the kernel command line (e.g.
	// "console=ttyS0 reboot=k panic=1 nomodule"). Empty defaults to
	// "console=ttyS0".
	Cmdline string

	// MemoryBytes is the guest RAM size. Must be at least 64 MiB for
	// any real Linux kernel to boot. Default 128 MiB.
	MemoryBytes uint64

	// VCPUs is the number of vCPUs. v1 supports 1; multi-vCPU needs
	// per-cpu APIC + IPI which lands in a follow-up.
	VCPUs int

	// InitrdPath optionally points to an initramfs to load alongside
	// the kernel. Empty disables initrd.
	InitrdPath string

	// OnUARTOutput is called for each byte the guest writes to the
	// COM1 data port (0x3F8). Typically wired to os.Stdout for a
	// serial console.
	OnUARTOutput func(byte)
}

// WHPBootSession is the handle returned by BootLinuxOnWHP. Run() drives
// the vCPU; Close() releases the partition + RAM.
type WHPBootSession struct {
	cfg      WHPBootConfig
	hv       Hypervisor
	vm       HVVM
	vcpu     HVVCPU
	memBytes []byte
	stop     chan struct{}
	pit      *pit8254 // 8254 PIT — satisfies Linux's TSC calibration loop
}

// BootLinuxOnWHP prepares a Linux kernel boot via WHP — allocates the
// partition, loads the kernel into guest RAM, sets up long-mode boot
// state, and creates the vCPU. The vCPU does NOT start executing until
// Run() is called.
//
// Returns a session handle the caller drives via Run/Close.
func BootLinuxOnWHP(ctx context.Context, cfg WHPBootConfig) (*WHPBootSession, error) {
	if cfg.KernelPath == "" {
		return nil, fmt.Errorf("WHPBootConfig.KernelPath is required")
	}
	if cfg.MemoryBytes == 0 {
		cfg.MemoryBytes = 128 * 1024 * 1024
	}
	if cfg.MemoryBytes < 64*1024*1024 {
		return nil, fmt.Errorf("WHPBootConfig.MemoryBytes must be at least 64 MiB; got %d", cfg.MemoryBytes)
	}
	if cfg.VCPUs == 0 {
		cfg.VCPUs = 1
	}
	if cfg.VCPUs != 1 {
		return nil, fmt.Errorf("WHPBootConfig.VCPUs must be 1 in Phase 2e (multi-vCPU lands in a follow-up)")
	}
	if cfg.Cmdline == "" {
		cfg.Cmdline = "console=ttyS0"
	}
	if cfg.OnUARTOutput == nil {
		cfg.OnUARTOutput = func(b byte) {} // /dev/null
	}

	hv, err := NewWHPHypervisor()
	if err != nil {
		return nil, fmt.Errorf("NewWHPHypervisor: %w", err)
	}
	cleanupHV := func() { _ = hv.Close() }

	vm, err := hv.CreateVM(HVVMConfig{NumVCPUs: cfg.VCPUs, MemoryBytes: cfg.MemoryBytes})
	if err != nil {
		cleanupHV()
		return nil, fmt.Errorf("CreateVM: %w", err)
	}
	cleanupVM := func() { _ = vm.Close(); cleanupHV() }

	ram, err := vm.AllocateGuestRAM(cfg.MemoryBytes)
	if err != nil {
		cleanupVM()
		return nil, fmt.Errorf("AllocateGuestRAM: %w", err)
	}

	// Load the kernel image into guest RAM and write boot params.
	info, err := loader.LoadKernel(ram, cfg.KernelPath, BootParamsAddr)
	if err != nil {
		cleanupVM()
		return nil, fmt.Errorf("loader.LoadKernel: %w", err)
	}

	// Cmdline immediately after kernel.
	cmdlineBytes := []byte(cfg.Cmdline)
	if len(cmdlineBytes)+1 > 4096 {
		cleanupVM()
		return nil, fmt.Errorf("kernel cmdline too long: %d bytes (limit 4095)", len(cmdlineBytes))
	}
	copy(ram[CmdlineAddr:], cmdlineBytes)
	ram[CmdlineAddr+len(cmdlineBytes)] = 0 // NUL-terminated

	// Optional initrd.
	var initrdAddr, initrdSize uint64
	if cfg.InitrdPath != "" {
		size, err := loadFileIntoRAM(ram, cfg.InitrdPath, InitrdAddr, uint64(len(ram)))
		if err != nil {
			cleanupVM()
			return nil, fmt.Errorf("load initrd: %w", err)
		}
		initrdAddr = InitrdAddr
		initrdSize = size
	}

	loader.WriteBootParams(ram, info, loader.BootConfig{
		MemBytes:   cfg.MemoryBytes,
		Cmdline:    cfg.Cmdline,
		InitrdAddr: initrdAddr,
		InitrdSize: initrdSize,
	})

	// Boot header fields the kernel sniffs (Firecracker-style: type_of_loader
	// = 0xFF "unknown bootloader", LOADED_HIGH flag, magic + boot_flag).
	binary.LittleEndian.PutUint16(ram[BootParamsAddr+0x1FE:], 0xAA55)
	binary.LittleEndian.PutUint32(ram[BootParamsAddr+0x202:], 0x53726448) // "HdrS"
	ram[BootParamsAddr+0x210] = 0xFF
	ram[BootParamsAddr+0x211] |= 0x01
	binary.LittleEndian.PutUint32(ram[BootParamsAddr+0x228:], uint32(CmdlineAddr))
	binary.LittleEndian.PutUint32(ram[BootParamsAddr+0x238:], uint32(len(cmdlineBytes)))

	// Long-mode page tables + GDT/IDT in guest RAM.
	BuildBootPageTables(ram, PageTableBase)
	WriteBootGDT(ram)

	// Map RAM into the partition with full RWX.
	if err := vm.MapMemory(0, ram, MemRWX); err != nil {
		cleanupVM()
		return nil, fmt.Errorf("MapMemory: %w", err)
	}

	// Create the vCPU and put it in 64-bit long mode.
	vcpu, err := vm.CreateVCPU(0)
	if err != nil {
		cleanupVM()
		return nil, fmt.Errorf("CreateVCPU(0): %w", err)
	}

	if err := vcpu.SetSegmentRegisters(LongModeBootSegments(PageTableBase)); err != nil {
		_ = vcpu.Close()
		cleanupVM()
		return nil, fmt.Errorf("SetSegmentRegisters: %w", err)
	}
	if err := vcpu.SetRegisters(LongModeBootRegisters(info.EntryPoint, BootParamsAddr)); err != nil {
		_ = vcpu.Close()
		cleanupVM()
		return nil, fmt.Errorf("SetRegisters: %w", err)
	}

	return &WHPBootSession{
		cfg:      cfg,
		hv:       hv,
		vm:       vm,
		vcpu:     vcpu,
		memBytes: ram,
		stop:     make(chan struct{}),
		pit:      newPIT8254(),
	}, nil
}

// Run drives the vCPU until the guest halts, the context is cancelled,
// or an unrecoverable exit fires. Returns nil on a clean halt.
//
// Exit dispatch:
//   - ExitReasonIOPort   → UART (port 0x3F8) → OnUARTOutput; otherwise
//                          drop (default 0xFF on IN). RIP advances by
//                          InstructionLength.
//   - ExitReasonMMIO     → not yet wired (Phase 2e+ adds virtio-mmio).
//                          For now we log and continue past the access.
//   - ExitReasonHalt     → return nil (clean shutdown).
//   - ExitReasonCancelled → return nil (caller asked Stop).
//   - ExitReasonInternal/FailEntry → return error.
//   - Other              → log and continue (best-effort).
func (s *WHPBootSession) Run(ctx context.Context) error {
	// Watchdog: another goroutine cancels the vCPU when ctx is done or
	// Close is called, so Run() returns promptly.
	go func() {
		select {
		case <-ctx.Done():
			_ = s.vcpu.Cancel()
		case <-s.stop:
			_ = s.vcpu.Cancel()
		}
	}()

	for {
		exitCtx, err := s.vcpu.Run()
		if err != nil {
			return fmt.Errorf("vcpu.Run: %w", err)
		}
		switch exitCtx.Reason {
		case ExitReasonIOPort:
			s.handleIOPortExit(exitCtx)
		case ExitReasonMMIO:
			// Best-effort: log and skip past the instruction. Phase 6
			// (virtio-fs / sharedfs) and Phase 9 (compose networking)
			// add real device emulation here.
			if err := s.advanceRIP(exitCtx.InstructionLength); err != nil {
				return err
			}
		case ExitReasonHalt:
			return nil
		case ExitReasonCancelled:
			return nil
		case ExitReasonInternal:
			return fmt.Errorf("hypervisor internal error: %s", exitCtx.FailureMsg)
		case ExitReasonFailEntry:
			return fmt.Errorf("hypervisor fail entry (bad guest state): %s", exitCtx.FailureMsg)
		case ExitReasonIRQWindowOpen, ExitReasonSystemEvent, ExitReasonCPUID, ExitReasonUnknown:
			// Tolerated — caller can wire more dispatchers later.
			if err := s.advanceRIP(exitCtx.InstructionLength); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unhandled exit reason %v", exitCtx.Reason)
		}
	}
}

// handleIOPortExit dispatches a port I/O exit to the right emulator
// (UART or 8254 PIT) or drops it, then advances RIP past the trapped
// instruction.
func (s *WHPBootSession) handleIOPortExit(exit ExitContext) {
	port := exit.IOPort.Port
	isWrite := exit.IOPort.Direction == IOPortOut
	switch {
	case port >= 0x3F8 && port < 0x400:
		// COM1 (Firecracker uses 0x3F8 base, 8 ports).
		if isWrite {
			r, _ := s.vcpu.GetRegisters()
			if port == 0x3F8 {
				s.cfg.OnUARTOutput(byte(r.RAX & 0xFF))
			}
		} else {
			// IN — return TX-empty (LSR bit 5) on port+5 so the kernel
			// keeps writing without blocking. Other registers read 0.
			var ret byte
			if port == 0x3F8+5 {
				ret = 0x20
			}
			r, _ := s.vcpu.GetRegisters()
			r.RAX = (r.RAX &^ 0xFF) | uint64(ret)
			_ = s.vcpu.SetRegisters(r)
		}
	case s.pit.handles(port):
		// 8254 PIT (0x40-0x43) + NMI/speaker (0x61). The kernel uses
		// these for TSC calibration.
		if isWrite {
			r, _ := s.vcpu.GetRegisters()
			s.pit.writePort(port, byte(r.RAX&0xFF))
		} else {
			val := s.pit.readPort(port)
			r, _ := s.vcpu.GetRegisters()
			r.RAX = (r.RAX &^ 0xFF) | uint64(val)
			_ = s.vcpu.SetRegisters(r)
		}
	default:
		// Unhandled IN: return 0xFF (no device). Unhandled OUT: drop.
		if !isWrite {
			r, _ := s.vcpu.GetRegisters()
			r.RAX = (r.RAX &^ 0xFF) | 0xFF
			_ = s.vcpu.SetRegisters(r)
		}
	}
	_ = s.advanceRIP(exit.InstructionLength)
}

// advanceRIP moves RIP forward by `length` bytes — required after every
// emulated exit on WHP (it does not auto-advance). length=0 is treated
// as 1 (most port/MMIO instructions are 1–3 bytes; 1 is the safe minimum
// for "I have no idea, try the next byte" — better than infinite-looping
// on the same instruction).
func (s *WHPBootSession) advanceRIP(length uint8) error {
	if length == 0 {
		length = 1
	}
	r, err := s.vcpu.GetRegisters()
	if err != nil {
		return fmt.Errorf("GetRegisters for RIP advance: %w", err)
	}
	r.RIP += uint64(length)
	if err := s.vcpu.SetRegisters(r); err != nil {
		return fmt.Errorf("SetRegisters for RIP advance: %w", err)
	}
	return nil
}

// Close releases all WHP resources held by the session.
func (s *WHPBootSession) Close() error {
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
	var firstErr error
	if s.vcpu != nil {
		if err := s.vcpu.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if s.vm != nil {
		if err := s.vm.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if s.hv != nil {
		if err := s.hv.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// loadFileIntoRAM reads path into mem at offset gpa. Returns the file
// size. Caller is responsible for ensuring gpa+size fits.
func loadFileIntoRAM(mem []byte, path string, gpa, memSize uint64) (uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return 0, err
	}
	size := uint64(fi.Size())
	if gpa+size > memSize {
		return 0, fmt.Errorf("file %s (%d bytes) at GPA %#x exceeds guest RAM (%d bytes)", path, size, gpa, memSize)
	}
	if _, err := io.ReadFull(f, mem[gpa:gpa+size]); err != nil {
		return 0, fmt.Errorf("read %s: %w", path, err)
	}
	return size, nil
}
