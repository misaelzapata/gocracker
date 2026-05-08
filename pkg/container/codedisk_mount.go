package container

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/gocracker/gocracker/internal/toolbox/agent"
	"github.com/gocracker/gocracker/internal/toolbox/client"
	"github.com/gocracker/gocracker/pkg/vmm"
)

// waitForAgentHealthy polls the toolbox /healthz endpoint until it
// returns success, the parent ctx is canceled, or the local timeout
// elapses. Used by MountAdditionalCodeDisks so the first per-disk
// exec frame doesn't race the in-guest supervisor coming back up
// after a snapshot restore. The poll interval is short (50 ms)
// because Health is sub-millisecond once the agent is bound.
func waitForAgentHealthy(parent context.Context, cli *client.Client, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		probeCtx, probeCancel := context.WithTimeout(ctx, 1*time.Second)
		_, err := cli.Health(probeCtx)
		probeCancel()
		if err == nil {
			return nil
		}
		lastErr = err
		select {
		case <-time.After(50 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if lastErr != nil {
		return lastErr
	}
	return fmt.Errorf("toolbox not ready within %s", timeout)
}

// codeDisksAsDriveConfigs converts the public CodeDisk slice into the
// vmm.DriveConfig shape that pkg/vmm consumes for AdditionalDrives at
// restore time. Drive IDs are minted as "code0", "code1", … to match the
// cold-boot ordering produced by runtimeDrives, so the device names are
// stable between Phase 1 (cold) and Phase 2 (restore).
func codeDisksAsDriveConfigs(disks []CodeDisk) []vmm.DriveConfig {
	if len(disks) == 0 {
		return nil
	}
	out := make([]vmm.DriveConfig, 0, len(disks))
	for i, cd := range disks {
		out = append(out, vmm.DriveConfig{
			ID:       fmt.Sprintf("code%d", i),
			Path:     cd.HostPath,
			Root:     false,
			ReadOnly: cd.ReadOnly,
		})
	}
	return out
}

// vsockUDSForCodeDiskMount returns the host-visible UDS path the toolbox
// agent listens on for this VM, or "" if the VM has no Vsock device. We
// pull it from the VM's effective config (post-OverrideVsockUDSPath) so
// callers don't have to thread the path through their own bookkeeping.
// Callers must pass a non-nil handle (the contract is satisfied by
// every real *vmm.VM / worker remote handle).
func vsockUDSForCodeDiskMount(vm interface{ VMConfig() vmm.Config }) string {
	cfg := vm.VMConfig()
	if cfg.Vsock == nil {
		return ""
	}
	return cfg.Vsock.UDSPath
}

// nonRootDriveCount returns how many entries in the live VM's drive list
// are non-root. MountAdditionalCodeDisks subtracts the count of just-
// injected AdditionalDrives from this to land at the device-name offset
// of the first new drive (so /dev/vd[b+offset] = first AdditionalDrive).
func nonRootDriveCount(vm interface{ VMConfig() vmm.Config }) int {
	cfg := vm.VMConfig()
	n := 0
	for _, d := range cfg.Drives {
		if !d.Root {
			n++
		}
	}
	return n
}

// MountAdditionalCodeDisks runs `mkdir -p MOUNT && mount -t FS DEV MOUNT`
// inside the guest, once per code-disk. It is the host-side hook for
// Phase 2 of code-disk-attach: when a snapshot is restored with extra
// drives the snapshot did not include, the guest's init has already
// finished, so it cannot self-mount the new disks. The host invokes
// this helper after Resume to land each disk at its intended path.
//
// baseDeviceIndex is the offset (0 = /dev/vdb) of the FIRST AdditionalDrive
// in the merged drive list — the same convention buildCmdlineWithPlan
// uses for the cold-boot gc.code_disk= cmdline. Pass len(snapshotDrives)-1
// (subtracting the snapshot's root) when the snapshot already had
// additional non-root drives, otherwise 0.
//
// Errors are aggregated: the function tries every disk and returns the
// first failure so the caller can choose whether to fail the lease or
// log and continue. udsPath empty or codeDisks empty is a no-op.
func MountAdditionalCodeDisks(ctx context.Context, udsPath string, codeDisks []CodeDisk, baseDeviceIndex int, perDiskTimeout time.Duration) error {
	if udsPath == "" || len(codeDisks) == 0 {
		return nil
	}
	if perDiskTimeout <= 0 {
		perDiskTimeout = 5 * time.Second
	}
	cli := client.New(udsPath)
	// On a fresh restore the in-guest toolbox supervisor takes a tick
	// to re-bind its listener before it answers our exec frames.
	// Probe Health first so the per-disk Wait below isn't burning its
	// budget on "agent not yet ready" instead of the actual mount.
	if err := waitForAgentHealthy(ctx, cli, 10*time.Second); err != nil {
		return fmt.Errorf("toolbox agent at %s not ready: %w", udsPath, err)
	}
	var firstErr error
	for i, cd := range codeDisks {
		dev := fmt.Sprintf("/dev/vd%c", 'b'+byte(baseDeviceIndex+i))
		fs := cd.FSType
		if fs == "" {
			fs = "ext4"
		}
		mode := "rw"
		if cd.ReadOnly {
			mode = "ro"
		}
		// Single sh -c so mkdir + mount run together with one exec
		// frame (one toolbox round-trip per disk). Quoting via %q
		// guards against paths with spaces; the device + fs are
		// constants we already validated in mustParseCodeDisks.
		// On a fresh restore the kernel takes a tick to publish the
		// new virtio-blk device under /dev/vdb; we retry the mount
		// up to ~1 s with a 50 ms backoff to ride out that race
		// (Agent C finding #4 for the code-disk path).
		script := fmt.Sprintf(
			"mkdir -p %q; "+
				"for _ in $(seq 1 20); do "+
				"  if [ -b %s ]; then break; fi; "+
				"  sleep 0.05; "+
				"done; "+
				"mount -t %s -o %s %s %q",
			cd.Mount, dev, fs, mode, dev, cd.Mount)
		var stdout, stderr bytes.Buffer
		execCtx, cancel := context.WithTimeout(ctx, perDiskTimeout)
		res, err := cli.Exec(execCtx, agent.ExecRequest{Cmd: []string{"sh", "-c", script}}, nil, &stdout, &stderr)
		cancel()
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("mount code-disk %s on %s: %w", dev, cd.Mount, err)
			}
			continue
		}
		if res.ExitCode != 0 {
			if firstErr == nil {
				firstErr = fmt.Errorf("mount code-disk %s on %s: exit %d: %s",
					dev, cd.Mount, res.ExitCode, stderr.String())
			}
			continue
		}
	}
	return firstErr
}
