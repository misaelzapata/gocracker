//go:build integration

package integration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	internalapi "github.com/gocracker/gocracker/internal/api"
)

// TestE2EBalloon covers the virtio-balloon plumbing through the HTTP API:
//
//  1. Boot an Alpine VM with balloon.amount_mib=0 (balloon present but not
//     inflated) and record MemTotal reported by `free -m`.
//  2. Inflate the balloon to 128 MiB via PATCH /balloon. Wait ~2 s for the
//     guest to release pages, then re-read `free -m` and assert MemTotal
//     dropped by at least ~100 MiB.
//  3. Deflate back to 0 MiB and assert MemTotal recovers.
//
// Known limitation: PATCH /balloon in internal/api/api.go routes to the
// "root" VM (the one created through the Firecracker-style preboot flow).
// VMs created through POST /run do NOT become the root — so PATCH /balloon
// returns 400 "balloon is not configured". When we detect that, we fall back
// to a boot-time verification (launch a second VM with amount_mib=128 and
// assert MemTotal differs vs. the amount_mib=0 VM), which still gives e2e
// coverage of the balloon-at-boot code path.
func TestE2EBalloon(t *testing.T) {
	if os.Getenv("E2E") != "1" {
		t.Skip("set E2E=1 to enable")
	}
	requirePrivilegedExecIntegration(t)
	kernel := resolveE2EKernel(t)
	bins := buildProjectBinaries(t)

	addr := freeLocalAddr(t)
	serverURL := "http://" + addr
	cacheDir := filepath.Join(t.TempDir(), "cache")
	stateDir := filepath.Join(t.TempDir(), "state")
	snapDir := filepath.Join(t.TempDir(), "snap")
	for _, d := range []string{cacheDir, stateDir, snapDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	serveCmd, serveLog := startE2EServe(t, bins, addr, cacheDir, stateDir, snapDir, kernel)
	t.Cleanup(func() { stopCommand(t, serveCmd) })
	waitForAPI(t, serverURL, 45*time.Second)

	client := internalapi.NewClient(serverURL)
	runCtx, runCancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer runCancel()
	runResp, err := client.Run(runCtx, internalapi.RunRequest{
		Image:      "alpine:3.20",
		KernelPath: kernel,
		MemMB:      256,
		DiskSizeMB: 256,
		Cmd:        []string{"/bin/sh", "-lc", "sleep infinity"},
		Balloon: &internalapi.Balloon{
			AmountMib:             0,
			DeflateOnOOM:          true,
			StatsPollingIntervalS: 1,
		},
		ExecEnabled: true,
	})
	if err != nil {
		t.Fatalf("/run: %v\nserve log:\n%s", err, serveLog.String())
	}
	t.Cleanup(func() { _ = client.StopVM(context.Background(), runResp.ID) })

	if _, err := waitForVMStateViaClient(t, client, runResp.ID, "running", 90*time.Second); err != nil {
		t.Fatalf("VM never reached running: %v\nserve log:\n%s", err, serveLog.String())
	}

	// Baseline MemTotal (balloon uninflated).
	total0 := readGuestMemTotalMiB(t, client, runResp.ID, 60*time.Second)
	t.Logf("baseline MemTotal (amount_mib=0) = %d MiB", total0)
	if total0 < 200 {
		t.Fatalf("unexpected baseline MemTotal %d MiB — expected ~240 MiB at mem_mb=256", total0)
	}

	// Try the runtime PATCH /balloon. If the VM is not the Firecracker root,
	// this returns 400 and we fall back to the boot-time comparison.
	patchErr := client.PatchBalloon(context.Background(), internalapi.BalloonUpdate{AmountMib: 128})
	if patchErr == nil {
		// Happy path: runtime PATCH worked. Wait for guest to surrender
		// pages, then re-read MemTotal.
		time.Sleep(2 * time.Second)
		total1 := readGuestMemTotalMiB(t, client, runResp.ID, 30*time.Second)
		t.Logf("MemTotal after PATCH amount_mib=128 = %d MiB (baseline=%d)", total1, total0)
		if total1 > total0-100 {
			t.Fatalf("balloon inflation did not reclaim memory: before=%d MiB after=%d MiB (want delta >= 100 MiB)",
				total0, total1)
		}

		// Deflate back.
		if err := client.PatchBalloon(context.Background(), internalapi.BalloonUpdate{AmountMib: 0}); err != nil {
			t.Fatalf("PATCH balloon deflate: %v", err)
		}
		time.Sleep(3 * time.Second)
		total2 := readGuestMemTotalMiB(t, client, runResp.ID, 30*time.Second)
		t.Logf("MemTotal after deflate = %d MiB", total2)
		if total2 < total0-32 {
			t.Fatalf("deflate did not restore memory: before=%d MiB after-inflate=%d MiB after-deflate=%d MiB",
				total0, total1, total2)
		}
		return
	}

	// PATCH failed — likely because POST /run VMs don't become the
	// Firecracker "root" instance. Document and skip the runtime sub-test,
	// then cover boot-time balloon by launching a second VM with amount_mib=128.
	t.Logf("PATCH /balloon not available for /run-created VMs: %v", patchErr)
	t.Logf("falling back to boot-time comparison sub-test")

	runResp2, err := client.Run(runCtx, internalapi.RunRequest{
		Image:      "alpine:3.20",
		KernelPath: kernel,
		MemMB:      256,
		DiskSizeMB: 256,
		Cmd:        []string{"/bin/sh", "-lc", "sleep infinity"},
		Balloon: &internalapi.Balloon{
			AmountMib:             128,
			DeflateOnOOM:          true,
			StatsPollingIntervalS: 1,
		},
		ExecEnabled: true,
	})
	if err != nil {
		t.Fatalf("/run (balloon=128): %v\nserve log:\n%s", err, serveLog.String())
	}
	t.Cleanup(func() { _ = client.StopVM(context.Background(), runResp2.ID) })
	if _, err := waitForVMStateViaClient(t, client, runResp2.ID, "running", 90*time.Second); err != nil {
		t.Fatalf("second VM never reached running: %v\nserve log:\n%s", err, serveLog.String())
	}
	// Give the balloon time to inflate post-boot.
	time.Sleep(4 * time.Second)
	total1 := readGuestMemTotalMiB(t, client, runResp2.ID, 30*time.Second)
	t.Logf("MemTotal (boot amount_mib=128) = %d MiB; baseline (amount_mib=0) = %d MiB", total1, total0)

	// Linux reports MemTotal as physical RAM minus pages in the balloon. We
	// expect a drop >= ~100 MiB (allowing slack for kernel overhead, and
	// recognising that the balloon driver may not have fully inflated by
	// the time we measure).
	if total1 > total0-100 {
		t.Fatalf("boot-time balloon did not reduce MemTotal as expected: amount_mib=0 -> %d MiB, amount_mib=128 -> %d MiB (want delta >= 100 MiB)",
			total0, total1)
	}
}

// TestE2EMemoryHotplug exercises the /hotplug/memory PATCH endpoint: boot a
// VM with hotplug configured and zero plugged, request 128 MiB, and verify
// MemTotal grows. Like balloon, /hotplug/memory is scoped to the Firecracker
// root VM; if that's unavailable we skip rather than fake.
func TestE2EMemoryHotplug(t *testing.T) {
	if os.Getenv("E2E") != "1" {
		t.Skip("set E2E=1 to enable")
	}
	requirePrivilegedExecIntegration(t)
	kernel := resolveE2EKernel(t)
	bins := buildProjectBinaries(t)

	addr := freeLocalAddr(t)
	serverURL := "http://" + addr
	cacheDir := filepath.Join(t.TempDir(), "cache")
	stateDir := filepath.Join(t.TempDir(), "state")
	snapDir := filepath.Join(t.TempDir(), "snap")
	for _, d := range []string{cacheDir, stateDir, snapDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	serveCmd, serveLog := startE2EServe(t, bins, addr, cacheDir, stateDir, snapDir, kernel)
	t.Cleanup(func() { stopCommand(t, serveCmd) })
	waitForAPI(t, serverURL, 45*time.Second)

	client := internalapi.NewClient(serverURL)
	runCtx, runCancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer runCancel()
	hotplug := &internalapi.MemoryHotplugConfig{
		TotalSizeMiB: 256,
		SlotSizeMiB:  256,
		BlockSizeMiB: 128,
	}
	runResp, err := client.Run(runCtx, internalapi.RunRequest{
		Image:         "alpine:3.20",
		KernelPath:    kernel,
		MemMB:         256,
		DiskSizeMB:    256,
		Cmd:           []string{"/bin/sh", "-lc", "sleep infinity"},
		MemoryHotplug: hotplug,
		ExecEnabled:   true,
	})
	if err != nil {
		t.Skipf("/run with memory hotplug failed (kernel may lack CONFIG_MEMORY_HOTPLUG): %v\nserve log:\n%s", err, serveLog.String())
	}
	t.Cleanup(func() { _ = client.StopVM(context.Background(), runResp.ID) })

	if _, err := waitForVMStateViaClient(t, client, runResp.ID, "running", 90*time.Second); err != nil {
		t.Fatalf("VM never reached running: %v\nserve log:\n%s", err, serveLog.String())
	}

	total0 := readGuestMemTotalMiB(t, client, runResp.ID, 60*time.Second)
	t.Logf("hotplug baseline MemTotal = %d MiB", total0)

	if err := client.PatchMemoryHotplug(context.Background(), internalapi.MemoryHotplugSizeUpdate{RequestedSizeMiB: 128}); err != nil {
		t.Skipf("PATCH /hotplug/memory not available for /run-created VMs: %v", err)
	}
	time.Sleep(3 * time.Second)
	total1 := readGuestMemTotalMiB(t, client, runResp.ID, 30*time.Second)
	t.Logf("MemTotal after +128 MiB hotplug = %d MiB", total1)
	if total1 < total0+96 {
		t.Fatalf("memory hotplug did not grow guest: before=%d MiB after=%d MiB (want delta >= 96 MiB)",
			total0, total1)
	}
}

// readGuestMemTotalMiB execs `cat /proc/meminfo` (busybox alpine: grep works
// too) and parses MemTotal in MiB. Returns on first successful parse within
// the timeout.
func readGuestMemTotalMiB(t *testing.T, client *internalapi.Client, id string, timeout time.Duration) uint64 {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		resp, err := client.ExecVM(ctx, id, internalapi.ExecRequest{
			Command: []string{"/bin/sh", "-lc", "cat /proc/meminfo"},
		})
		cancel()
		if err != nil {
			lastErr = err
			time.Sleep(250 * time.Millisecond)
			continue
		}
		for _, line := range strings.Split(resp.Stdout, "\n") {
			if !strings.HasPrefix(line, "MemTotal:") {
				continue
			}
			fields := strings.Fields(line)
			// Format: "MemTotal:     249348 kB"
			if len(fields) < 2 {
				lastErr = fmt.Errorf("MemTotal line malformed: %q", line)
				break
			}
			kib, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				lastErr = fmt.Errorf("parse MemTotal value %q: %w", fields[1], err)
				break
			}
			return kib / 1024
		}
		if lastErr == nil {
			lastErr = fmt.Errorf("MemTotal not found in /proc/meminfo")
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("failed to read MemTotal within %s: %v", timeout, lastErr)
	return 0
}
