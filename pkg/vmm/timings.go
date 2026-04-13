package vmm

import "time"

// BootTimings is the per-phase breakdown of how long it took to bring a
// microVM to life. Every phase is measured independently so the caller
// can see exactly where time is going instead of a single misleading
// "duration" number.
//
// The four phases are:
//
//   - Orchestration: host-side work *before* the guest kernel starts
//     booting. For the in-process path (runLocal) this is effectively 0
//     because there is no IPC. For the worker path (runViaWorker) this
//     includes fork-exec of gocracker-jailer, Go runtime init of the
//     second process, chroot/mount/mknod/pivot_root, syscall.Exec into
//     gocracker-vmm, waiting for the unix socket, and the SetBootSource /
//     SetDrive / SetMachineConfig / SetNetworkInterface / SetSharedFS
//     REST PUTs that populate the remote VMM's pre-boot config.
//
//   - VMMSetup: time spent inside vmm.New() — KVM_CREATE_VM, memory
//     allocation, virtio device wiring, IRQCHIP / LAPIC setup, kernel
//     image load + decompression + copy to guest RAM, vCPU creation,
//     CPUID/MSR setup, GDT/IDT programming. This is identical work in
//     both paths; the only difference is whether it happens in-process
//     or inside a subprocess addressed via REST.
//
//   - Start: time spent inside vm.Start() — spawning the vCPU goroutines
//     and entering KVM_RUN. This is normally tiny (sub-millisecond)
//     because the guest kernel does not actually execute yet; it is
//     accounted separately so the caller can see it is negligible.
//
//   - GuestFirstOutput: wall clock from vm.Start() returning until the
//     guest first transmits a byte through the UART. This is the honest
//     "time to usable VM" number that matches what Firecracker's
//     boot-timer reports and what AWS publishes in its SLO.
//
// Total is the sum of all phases and represents end-to-end wall clock
// from the CLI picking up the boot request to the guest kernel being
// able to talk on the serial console.
type BootTimings struct {
	Orchestration    time.Duration `json:"orchestration"`
	VMMSetup         time.Duration `json:"vmm_setup"`
	Start            time.Duration `json:"start"`
	GuestFirstOutput time.Duration `json:"guest_first_output"`
	Total            time.Duration `json:"total"`
}

// Sum recomputes Total from the individual phases and returns a copy.
// Use this after populating the individual phases to keep Total in sync.
func (t BootTimings) Sum() BootTimings {
	t.Total = t.Orchestration + t.VMMSetup + t.Start + t.GuestFirstOutput
	return t
}
