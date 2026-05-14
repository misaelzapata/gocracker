//go:build windows

package vmm

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/gocracker/gocracker/internal/loader"
	"github.com/gocracker/gocracker/internal/whp"
)

// whpRunner is the Windows-only interface that *whpVCPU satisfies — it
// gives us the raw whp.ExitContext alongside the portable one. We type-
// assert on this to keep the public HVVCPU surface unchanged.
type whpRunner interface {
	RunRaw() (whp.ExitContext, ExitContext, error)
}

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

	// RootfsPath optionally points to an ext4 image to attach as the
	// guest's root block device (virtio-blk-mmio). Empty disables;
	// useful for early kernel-only smoke tests. When set, the kernel
	// command line is appended with
	//   virtio_mmio.device=4K@0xD0000000:5 root=/dev/vda rw rootfstype=ext4
	// so Linux's virtio_mmio driver probes the device at boot.
	RootfsPath string

	// RootfsReadOnly mounts the rootfs read-only (sets VIRTIO_BLK_F_RO).
	// Combine with `ro` in the cmdline if you also want the kernel to
	// avoid issuing writes.
	RootfsReadOnly bool
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
	pit      *pit8254        // 8254 PIT — real mode-3, drives IRQ 0
	pic      *pic8259        // 8259 master/slave PICs — legacy IRQ delivery
	cmos     *cmosRTC        // MC146818 RTC — seeds the kernel's wall clock
	uart     *UART16550      // 16550A COM1 (output + RX FIFO + IRQ4)
	pci      *pciConfigDummy // 0xCF8/0xCFC PCI config sentinel
	hndl     whp.PartitionHandle // raw partition handle for IRQ injection

	// MMIO emulator + dispatch table for virtio-mmio devices. Each
	// device entry handles a small GPA window; the emulator decodes
	// the trapped instruction and routes the read/write through us.
	emulator *whp.Emulator
	blkDev   *VirtioBlk // virtio-blk-mmio (rootfs) — nil if no rootfs configured
	rngDev   *VirtioRng // virtio-rng-mmio (entropy)

	// IRQ lines wired through the 8259 PIC. Each must match the value
	// in the `virtio_mmio.device=…:N` cmdline parameter.
	blkIRQ uint8
	rngIRQ uint8
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
	// If the caller supplied a rootfs, extend the cmdline so Linux probes
	// the virtio-mmio device and mounts /dev/vda. IRQ 5/6 are legacy ISA
	// lines free from common PIC reservations; the PIC binds them once
	// the kernel runs the ICW sequence. Always expose virtio-rng — most
	// initramfs and userspace blocks on /dev/urandom seeding otherwise.
	const blkMMIOBase uint64 = 0xD0000000
	const rngMMIOBase uint64 = 0xD0001000
	const blkIRQ uint8 = 5
	const rngIRQ uint8 = 6
	cfg.Cmdline += fmt.Sprintf(" virtio_mmio.device=4K@0x%X:%d", rngMMIOBase, rngIRQ)
	if cfg.RootfsPath != "" {
		cfg.Cmdline += fmt.Sprintf(" virtio_mmio.device=4K@0x%X:%d root=/dev/vda rw rootfstype=ext4", blkMMIOBase, blkIRQ)
	}

	hv, err := NewWHPHypervisor()
	if err != nil {
		return nil, fmt.Errorf("NewWHPHypervisor: %w", err)
	}
	cleanupHV := func() { _ = hv.Close() }

	vm, err := hv.CreateVM(HVVMConfig{
		NumVCPUs:    cfg.VCPUs,
		MemoryBytes: cfg.MemoryBytes,
		EnableXAPIC: true,
	})
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

	// Optional initrd. The default InitrdAddr (16 MiB) collides with a
	// 40+ MiB vmlinux that gets loaded at 1 MiB, so we place the initrd
	// near the top of RAM instead — leaves plenty of room for kernel
	// + early page tables + boot params.
	var initrdAddr, initrdSize uint64
	if cfg.InitrdPath != "" {
		fi, statErr := os.Stat(cfg.InitrdPath)
		if statErr != nil {
			cleanupVM()
			return nil, fmt.Errorf("stat initrd: %w", statErr)
		}
		// Align down to 4 KiB so the kernel maps cleanly.
		addr := (cfg.MemoryBytes - uint64(fi.Size())) &^ 0xFFF
		size, err := loadFileIntoRAM(ram, cfg.InitrdPath, addr, uint64(len(ram)))
		if err != nil {
			cleanupVM()
			return nil, fmt.Errorf("load initrd: %w", err)
		}
		initrdAddr = addr
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
	// REGION:ACPI-WIRE — Batch 2 integrator replaces with acpi.WriteTables(ram).
	// (ACPI tables expose the LAPIC/IOAPIC topology and PIT/COM interrupt
	// overrides so the kernel doesn't fall back to ad-hoc defaults.)

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

	// Stash the underlying WHP partition handle so the IRQ goroutine
	// can call whp.RequestFixedInterrupt directly. Type-asserting on
	// *whpVM is safe because we only get here via NewWHPHypervisor.
	whpvm, ok := vm.(*whpVM)
	if !ok {
		_ = vcpu.Close()
		cleanupVM()
		return nil, fmt.Errorf("internal: HVVM is %T, want *whpVM", vm)
	}

	session := &WHPBootSession{
		cfg:      cfg,
		hv:       hv,
		vm:       vm,
		vcpu:     vcpu,
		memBytes: ram,
		stop:     make(chan struct{}),
		pit:      newPIT8254(),
		pic:      newPIC8259(),
		cmos:     newCMOS(),
		hndl:     whpvm.handle,
		blkIRQ:   blkIRQ,
		rngIRQ:   rngIRQ,
	}

	// Spin up the WHP MMIO emulator and the virtio devices. Done after
	// the partition is fully set up because WHvEmulatorCreateEmulator
	// validates against the loaded DLL.
	em, err := whp.CreateEmulator()
	if err != nil {
		_ = vcpu.Close()
		cleanupVM()
		return nil, fmt.Errorf("whp.CreateEmulator: %w", err)
	}
	session.emulator = em
	session.rngDev = NewVirtioRng(rngMMIOBase, ram, func() { session.raiseIRQ(rngIRQ) })

	if cfg.RootfsPath != "" {
		blk, err := NewVirtioBlk(blkMMIOBase, ram, cfg.RootfsPath, cfg.RootfsReadOnly,
			func() { session.raiseIRQ(blkIRQ) })
		if err != nil {
			_ = em.Destroy()
			_ = vcpu.Close()
			cleanupVM()
			return nil, fmt.Errorf("NewVirtioBlk: %w", err)
		}
		session.blkDev = blk
	}

	session.uart = NewUART16550(
		func() { session.raiseIRQ(4) },
		cfg.OnUARTOutput,
	)
	session.pci = newPCIConfigDummy()
	session.pit.SetIRQ0Callback(func() { session.raiseIRQ(0) })

	return session, nil
}

// PushUARTInput feeds a byte into the guest's COM1 RX path. The
// caller (typically a stdin reader goroutine) writes one keystroke at
// a time; the UART raises IRQ4 if the kernel's 8250 driver has
// unmasked it.
func (s *WHPBootSession) PushUARTInput(b byte) {
	if s.uart != nil {
		s.uart.PushRX(b)
	}
}

// raiseIRQ delivers a virtio device IRQ to the guest via the 8259
// PIC. No-op until the kernel has finished the ICW sequence and
// unmasked the line.
func (s *WHPBootSession) raiseIRQ(irq uint8) {
	if !s.pic.initialized() || !s.pic.irqUnmasked(irq) {
		return
	}
	_ = whp.RequestFixedInterrupt(s.hndl, s.pic.vectorForIRQ(irq))
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

	// Timer-IRQ ticker: 100 Hz IRQ 0 deliveries via the 8259 PIC. The
	// Linux kernel needs these to advance jiffies and finish its TSC
	// calibration loop. We fire only once the kernel has finished the
	// 8259 init sequence (PIC.initialized()) and unmasked IRQ 0.
	go func() {
		t := time.NewTicker(10 * time.Millisecond) // 100 Hz
		defer t.Stop()
		for {
			select {
			case <-s.stop:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				if !s.pic.initialized() {
					continue
				}
				if !s.pic.irqUnmasked(0) {
					continue
				}
				vector := s.pic.vectorForIRQ(0)
				_ = whp.RequestFixedInterrupt(s.hndl, vector)
			}
		}
	}()

	// We need the raw exit context for MMIO emulation. *whpVCPU exposes
	// it via RunRaw — fall back to plain Run() for backends that don't.
	raw, ok := s.vcpu.(whpRunner)
	if !ok {
		return fmt.Errorf("internal: vcpu %T does not implement whpRunner", s.vcpu)
	}

	for {
		rawCtx, exitCtx, err := raw.RunRaw()
		if err != nil {
			return fmt.Errorf("vcpu.RunRaw: %w", err)
		}
		switch exitCtx.Reason {
		case ExitReasonIOPort:
			s.handleIOPortExit(exitCtx)
		case ExitReasonMMIO:
			// MMIO: route through the WHP emulator, which decodes the
			// trapped instruction and calls back into our device tree.
			// If the emulator isn't set up (no rootfs configured) or the
			// access isn't claimed by any device, fall back to advancing
			// RIP past the instruction so the kernel doesn't loop.
			if s.emulator != nil && s.mmioAddrHandled(exitCtx.MMIO.Address) {
				if err := s.dispatchMMIOExit(&rawCtx); err != nil {
					return fmt.Errorf("MMIO emulation at gpa=%#x: %w", exitCtx.MMIO.Address, err)
				}
				continue // emulator updated RIP via SetRegistersCallback
			}
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

// mmioAddrHandled reports whether the given guest-physical address
// falls inside one of the registered virtio-mmio devices' windows.
func (s *WHPBootSession) mmioAddrHandled(addr uint64) bool {
	if s.blkDev != nil && s.blkDev.HandlesAddr(addr) {
		return true
	}
	if s.rngDev != nil && s.rngDev.HandlesAddr(addr) {
		return true
	}
	return false
}

// dispatchMMIOExit hands the trapped MMIO instruction to the WHP
// emulator. The emulator decodes the instruction, invokes our memory
// callback for the side effects, updates RIP and the destination
// register, and returns. The raw exit context must remain alive for
// the duration of the call (the emulator dereferences pointers into
// its embedded sub-structs).
func (s *WHPBootSession) dispatchMMIOExit(rawCtx *whp.ExitContext) error {
	ev := &whp.EmulatorVCPU{
		Partition: s.hndl,
		VCPUIndex: 0,
		Mem:       s.memBytes,
		MMIORead: func(addr uint64, length uint8) []byte {
			var v uint32
			handled := false
			if s.blkDev != nil && s.blkDev.HandlesAddr(addr) {
				v = s.blkDev.ReadMMIO(addr, uint32(length))
				handled = true
			} else if s.rngDev != nil && s.rngDev.HandlesAddr(addr) {
				v = s.rngDev.ReadMMIO(addr, uint32(length))
				handled = true
			}
			if !handled {
				return nil
			}
			out := make([]byte, length)
			for i := uint8(0); i < length; i++ {
				out[i] = byte((v >> (8 * i)) & 0xFF)
			}
			return out
		},
		MMIOWrite: func(addr uint64, length uint8, data []byte) {
			var v uint32
			for i := 0; i < int(length) && i < len(data); i++ {
				v |= uint32(data[i]) << (8 * i)
			}
			if s.blkDev != nil && s.blkDev.HandlesAddr(addr) {
				s.blkDev.WriteMMIO(addr, uint32(length), v)
			} else if s.rngDev != nil && s.rngDev.HandlesAddr(addr) {
				s.rngDev.WriteMMIO(addr, uint32(length), v)
			}
		},
	}
	return s.emulator.TryMmioEmulation(ev, rawCtx.VpContextPtr(), rawCtx.MemoryAccessPtr())
}

// handleIOPortExit dispatches a port I/O exit to the right emulator
// (UART or 8254 PIT) or drops it, then advances RIP past the trapped
// instruction.
func (s *WHPBootSession) handleIOPortExit(exit ExitContext) {
	port := exit.IOPort.Port
	isWrite := exit.IOPort.Direction == IOPortOut
	switch {
	case s.uart != nil && s.uart.Handles(port):
		// 16550A COM1 — full device. DLAB-gated divisor latches, IER,
		// MCR loopback, RX FIFO, IRQ4 on RBR/THRE transitions.
		if isWrite {
			r, _ := s.vcpu.GetRegisters()
			s.uart.WritePort(port, byte(r.RAX&0xFF))
		} else {
			val := s.uart.ReadPort(port)
			r, _ := s.vcpu.GetRegisters()
			r.RAX = (r.RAX &^ 0xFF) | uint64(val)
			_ = s.vcpu.SetRegisters(r)
		}
	case s.pci != nil && s.pci.handles(port):
		// PCI config-space sentinel — every probe returns 0xFFFFFFFF
		// (no device) so Linux's bus enumeration exits cleanly.
		if isWrite {
			r, _ := s.vcpu.GetRegisters()
			s.pci.writePort(port, byte(r.RAX&0xFF))
		} else {
			val := s.pci.readPort(port)
			r, _ := s.vcpu.GetRegisters()
			r.RAX = (r.RAX &^ 0xFF) | uint64(val)
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
	case s.pic.handles(port):
		// 8259 master/slave PIC (0x20/0x21/0xA0/0xA1). Once the kernel
		// runs the ICW1-ICW4 init sequence and unmasks IRQ 0, the
		// IRQ-injection goroutine starts firing 100 Hz timer ticks.
		if isWrite {
			r, _ := s.vcpu.GetRegisters()
			s.pic.writePort(port, byte(r.RAX&0xFF))
		} else {
			val := s.pic.readPort(port)
			r, _ := s.vcpu.GetRegisters()
			r.RAX = (r.RAX &^ 0xFF) | uint64(val)
			_ = s.vcpu.SetRegisters(r)
		}
	case s.cmos != nil && s.cmos.handles(port):
		// MC146818 RTC (0x70/0x71). Reads return host wall clock as
		// BCD; writes to control regs only.
		if isWrite {
			r, _ := s.vcpu.GetRegisters()
			s.cmos.writePort(port, byte(r.RAX&0xFF))
		} else {
			val := s.cmos.readPort(port)
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
	if s.blkDev != nil {
		if err := s.blkDev.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if s.emulator != nil {
		if err := s.emulator.Destroy(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
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
