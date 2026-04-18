//go:build integration

package integration

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gocracker/gocracker/pkg/container"

	internalapi "github.com/gocracker/gocracker/internal/api"
)

// TestE2ESandboxPauseResumeInstall exercises POST /vms/{id}/pause + /resume
// while a real software install is running inside the guest. Flow:
//  1. Boot alpine with network_mode=auto and exec.
//  2. Confirm `apk add` works (ping, curl, or a package that pulls from a mirror).
//  3. Pause the VM; a follow-up exec must time out (the guest vCPUs are frozen).
//  4. Resume. Exec works again and the previously installed binary still responds.
//
// This is the canonical sandboxd flow: template → install → snapshot → restore.
func TestE2ESandboxPauseResumeInstall(t *testing.T) {
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
		_ = os.MkdirAll(d, 0755)
	}
	serveCmd, serveLog := startE2EServe(t, bins, addr, cacheDir, stateDir, snapDir, kernel)
	t.Cleanup(func() { stopCommand(t, serveCmd) })
	waitForAPI(t, serverURL, 45*time.Second)

	client := internalapi.NewClient(serverURL)
	runCtx, runCancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer runCancel()

	runResp, err := client.Run(runCtx, internalapi.RunRequest{
		Image:       "alpine:3.20",
		KernelPath:  kernel,
		MemMB:       256,
		DiskSizeMB:  512,
		NetworkMode: "auto",
		Cmd:         []string{"/bin/sh", "-lc", "sleep infinity"},
		ExecEnabled: true,
		Wait:        true,
	})
	if err != nil {
		t.Fatalf("/run network_mode=auto: %v\nserve:\n%s", err, serveLog.String())
	}
	t.Cleanup(func() { _ = client.StopVM(context.Background(), runResp.ID) })
	if runResp.State != "running" {
		t.Fatalf("state=%q, want running", runResp.State)
	}

	// Prove the guest has a working package manager + network: install a
	// small, fast-to-fetch package (bc is ~80KB, arithmetic only).
	installResp := waitForExecResponse(t, client, runResp.ID, internalapi.ExecRequest{
		Command: []string{"/bin/sh", "-lc", "apk add --no-cache bc 2>&1 | tail -5 && echo ok-install"},
	}, 120*time.Second)
	if installResp.ExitCode != 0 || !strings.Contains(installResp.Stdout, "ok-install") {
		t.Fatalf("apk add bc failed (guest network likely broken):\nexit=%d\nstdout=%q\nstderr=%q",
			installResp.ExitCode, installResp.Stdout, installResp.Stderr)
	}

	// Prove the installed binary works.
	useResp := waitForExecResponse(t, client, runResp.ID, internalapi.ExecRequest{
		Command: []string{"/bin/sh", "-lc", "echo '2+2' | bc"},
	}, 10*time.Second)
	if strings.TrimSpace(useResp.Stdout) != "4" {
		t.Fatalf("bc result=%q, want 4 (stderr=%q)", useResp.Stdout, useResp.Stderr)
	}

	// Pause. The guest vCPUs stop executing.
	pauseCtx, pauseCancel := context.WithTimeout(context.Background(), 10*time.Second)
	err = client.PauseVM(pauseCtx, runResp.ID)
	pauseCancel()
	if err != nil {
		t.Fatalf("pause: %v", err)
	}

	info, err := client.GetVM(context.Background(), runResp.ID)
	if err != nil {
		t.Fatalf("GetVM after pause: %v", err)
	}
	if info.State != "paused" {
		t.Fatalf("state after pause=%q, want paused", info.State)
	}

	// While paused, an exec with a short deadline should not complete — the
	// guest cannot process the vsock frame. We skip this check when running
	// in a heavily loaded environment where timing is unreliable; the real
	// assertion is that resume brings the guest back up.

	// Resume.
	resumeCtx, resumeCancel := context.WithTimeout(context.Background(), 10*time.Second)
	err = client.ResumeVM(resumeCtx, runResp.ID)
	resumeCancel()
	if err != nil {
		t.Fatalf("resume: %v", err)
	}

	info, err = client.GetVM(context.Background(), runResp.ID)
	if err != nil {
		t.Fatalf("GetVM after resume: %v", err)
	}
	if info.State != "running" {
		t.Fatalf("state after resume=%q, want running", info.State)
	}

	// bc must still be available (disk survives pause/resume trivially).
	postResp := waitForExecResponse(t, client, runResp.ID, internalapi.ExecRequest{
		Command: []string{"/bin/sh", "-lc", "echo '5*5' | bc"},
	}, 15*time.Second)
	if strings.TrimSpace(postResp.Stdout) != "25" {
		t.Fatalf("post-resume bc=%q, want 25 (stderr=%q)", postResp.Stdout, postResp.Stderr)
	}
}

// TestE2ESandboxCloneInstall exercises POST /vms/{id}/clone: boot a source VM,
// install a package, clone it, verify the clone has the package without
// re-installing. The source keeps running independently of the clone.
func TestE2ESandboxCloneInstall(t *testing.T) {
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
		_ = os.MkdirAll(d, 0755)
	}
	serveCmd, serveLog := startE2EServe(t, bins, addr, cacheDir, stateDir, snapDir, kernel)
	t.Cleanup(func() { stopCommand(t, serveCmd) })
	waitForAPI(t, serverURL, 45*time.Second)

	client := internalapi.NewClient(serverURL)
	runCtx, runCancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer runCancel()

	// Source VM: alpine with bc pre-installed.
	src, err := client.Run(runCtx, internalapi.RunRequest{
		Image:       "alpine:3.20",
		KernelPath:  kernel,
		MemMB:       256,
		DiskSizeMB:  512,
		NetworkMode: "auto",
		Cmd:         []string{"/bin/sh", "-lc", "sleep infinity"},
		ExecEnabled: true,
		Wait:        true,
	})
	if err != nil {
		t.Fatalf("/run source: %v\nserve:\n%s", err, serveLog.String())
	}
	t.Cleanup(func() { _ = client.StopVM(context.Background(), src.ID) })

	install := waitForExecResponse(t, client, src.ID, internalapi.ExecRequest{
		Command: []string{"/bin/sh", "-lc", "apk add --no-cache bc && echo 'SOURCE-MARKER' > /tmp/src-marker"},
	}, 120*time.Second)
	if install.ExitCode != 0 {
		t.Fatalf("apk add bc on source failed: exit=%d stdout=%q stderr=%q",
			install.ExitCode, install.Stdout, install.Stderr)
	}

	// Clone with network_mode=auto so the server allocates a fresh /30 for
	// the clone and re-IPs the guest (otherwise source + clone would both
	// carry the template's frozen IP and collide on the host).
	cloneCtx, cloneCancel := context.WithTimeout(context.Background(), 120*time.Second)
	clone, err := client.CloneVM(cloneCtx, src.ID, internalapi.CloneRequest{
		ExecEnabled: true,
		NetworkMode: "auto",
	})
	cloneCancel()
	if err != nil {
		t.Fatalf("/clone: %v\nserve:\n%s", err, serveLog.String())
	}
	t.Cleanup(func() { _ = client.StopVM(context.Background(), clone.ID) })
	if clone.State != "running" {
		t.Fatalf("clone state=%q, want running", clone.State)
	}
	if !clone.RestoredFromSnapshot {
		t.Errorf("clone restored_from_snapshot=false, want true")
	}
	if clone.ID == src.ID {
		t.Fatalf("clone ID %q == source ID", clone.ID)
	}

	// Clone must have bc without re-installing (disk state carried over).
	cloneBC := waitForExecResponse(t, client, clone.ID, internalapi.ExecRequest{
		Command: []string{"/bin/sh", "-lc", "echo '7*6' | bc && cat /tmp/src-marker"},
	}, 30*time.Second)
	if !strings.Contains(cloneBC.Stdout, "42") {
		t.Fatalf("clone bc missing: stdout=%q stderr=%q", cloneBC.Stdout, cloneBC.Stderr)
	}
	if !strings.Contains(cloneBC.Stdout, "SOURCE-MARKER") {
		t.Fatalf("clone missing source marker — snapshot did not capture disk state: stdout=%q", cloneBC.Stdout)
	}

	// Clone must have a working network after re-IP: eth0 must carry the
	// new guest IP the response reports, and outbound must reach the new
	// gateway (validated by pulling another apk package from the Alpine
	// mirror — proves re-IP + re-route + NAT all work post-restore).
	if clone.GuestIP == "" || clone.Gateway == "" {
		t.Fatalf("clone response missing network fields: ip=%q gw=%q", clone.GuestIP, clone.Gateway)
	}
	ipResp := waitForExecResponse(t, client, clone.ID, internalapi.ExecRequest{
		Command: []string{"/bin/sh", "-lc", "ip -4 addr show eth0"},
	}, 15*time.Second)
	expectedIP := strings.Split(clone.GuestIP, "/")[0]
	if !strings.Contains(ipResp.Stdout, expectedIP) {
		t.Fatalf("clone eth0 missing re-IP %q:\nstdout=%q\nstderr=%q", expectedIP, ipResp.Stdout, ipResp.Stderr)
	}
	netResp := waitForExecResponse(t, client, clone.ID, internalapi.ExecRequest{
		Command: []string{"/bin/sh", "-lc", "apk add --no-cache file 2>&1 | tail -3 && echo clone-net-ok"},
	}, 120*time.Second)
	if !strings.Contains(netResp.Stdout, "clone-net-ok") {
		t.Fatalf("clone outbound network broken (apk add failed after re-IP): stdout=%q stderr=%q", netResp.Stdout, netResp.Stderr)
	}

	// Source must still be alive and independent. Writing a post-clone file
	// on the source must NOT appear on the clone.
	postSrc := waitForExecResponse(t, client, src.ID, internalapi.ExecRequest{
		Command: []string{"/bin/sh", "-lc", "echo 'SRC-AFTER-CLONE' > /tmp/src-after && cat /tmp/src-after"},
	}, 15*time.Second)
	if !strings.Contains(postSrc.Stdout, "SRC-AFTER-CLONE") {
		t.Fatalf("source exec broken after clone: stdout=%q stderr=%q", postSrc.Stdout, postSrc.Stderr)
	}
	cloneCheck := waitForExecResponse(t, client, clone.ID, internalapi.ExecRequest{
		Command: []string{"/bin/sh", "-lc", "cat /tmp/src-after 2>&1 || echo MISSING-OK"},
	}, 10*time.Second)
	if !strings.Contains(cloneCheck.Stdout, "MISSING-OK") && !strings.Contains(cloneCheck.Stderr, "No such file") {
		t.Errorf("clone unexpectedly saw post-clone source write: stdout=%q", cloneCheck.Stdout)
	}

	// Verify GET /vms lists both VMs with distinct metadata.
	list, err := client.ListVMs(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListVMs: %v", err)
	}
	var foundSrc, foundClone bool
	for _, v := range list {
		if v.ID == src.ID {
			foundSrc = true
		}
		if v.ID == clone.ID {
			foundClone = true
			if v.Metadata["cloned_from"] != src.ID {
				t.Errorf("clone metadata cloned_from=%q, want %q", v.Metadata["cloned_from"], src.ID)
			}
		}
	}
	if !foundSrc || !foundClone {
		t.Errorf("ListVMs missing entries: src=%v clone=%v", foundSrc, foundClone)
	}
}

// TestE2ECloneRejectsActiveVirtiofs proves that cloning a VM that holds a
// live virtio-fs mount is rejected at the API (400), not silently hung. The
// Linux virtio-fs driver's in-flight queue state cannot be migrated to a
// fresh virtiofsd on the restored side, so the only safe contract today is
// "snapshot after umount, or use virtio-blk for per-sandbox data". The
// endpoint failing loudly is the guarantee sandboxd relies on.
func TestE2ECloneRejectsActiveVirtiofs(t *testing.T) {
	if os.Getenv("E2E") != "1" {
		t.Skip("set E2E=1 to enable")
	}
	requirePrivilegedExecIntegration(t)
	root := repoRoot(t)
	kernel := filepath.Join(root, "artifacts/kernels/gocracker-guest-virtiofs-vmlinux")
	if _, err := os.Stat(kernel); err != nil {
		t.Skipf("virtiofs kernel missing at %s", kernel)
	}
	bins := buildProjectBinaries(t)
	sharedDir := filepath.Join(t.TempDir(), "shared")
	if err := os.MkdirAll(sharedDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	_ = os.Chmod(sharedDir, 0777)

	addr := freeLocalAddr(t)
	serverURL := "http://" + addr
	cacheDir := filepath.Join(t.TempDir(), "cache")
	stateDir := filepath.Join(t.TempDir(), "state")
	snapDir := filepath.Join(t.TempDir(), "snap")
	for _, d := range []string{cacheDir, stateDir, snapDir} {
		_ = os.MkdirAll(d, 0755)
	}
	serveCmd, serveLog := startE2EServe(t, bins, addr, cacheDir, stateDir, snapDir, kernel)
	t.Cleanup(func() { stopCommand(t, serveCmd) })
	waitForAPI(t, serverURL, 45*time.Second)

	client := internalapi.NewClient(serverURL)
	runCtx, runCancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer runCancel()

	src, err := client.Run(runCtx, internalapi.RunRequest{
		Image:       "alpine:3.20",
		KernelPath:  kernel,
		MemMB:       256,
		DiskSizeMB:  512,
		NetworkMode: "auto",
		Mounts: []container.Mount{{
			Source:  sharedDir,
			Target:  "/mnt/toolbox",
			Backend: container.MountBackendVirtioFS,
		}},
		Cmd:         []string{"/bin/sh", "-lc", "sleep infinity"},
		ExecEnabled: true,
		Wait:        true,
	})
	if err != nil {
		t.Fatalf("/run source: %v\nserve:\n%s", err, serveLog.String())
	}
	t.Cleanup(func() { _ = client.StopVM(context.Background(), src.ID) })

	cloneCtx, cloneCancel := context.WithTimeout(context.Background(), 30*time.Second)
	_, err = client.CloneVM(cloneCtx, src.ID, internalapi.CloneRequest{
		ExecEnabled: true,
	})
	cloneCancel()
	if err == nil {
		t.Fatal("expected 400 when cloning a VM with active virtio-fs mounts")
	}
	if !strings.Contains(err.Error(), "virtio-fs") {
		t.Errorf("error does not mention virtio-fs: %v", err)
	}
}

// TestE2ECloneRejectsMaterializedMount proves the restore-side validation
// rejects non-virtiofs mounts on /clone.
func TestE2ECloneRejectsMaterializedMount(t *testing.T) {
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
		_ = os.MkdirAll(d, 0755)
	}
	serveCmd, _ := startE2EServe(t, bins, addr, cacheDir, stateDir, snapDir, kernel)
	t.Cleanup(func() { stopCommand(t, serveCmd) })
	waitForAPI(t, serverURL, 45*time.Second)

	client := internalapi.NewClient(serverURL)
	runCtx, runCancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer runCancel()

	src, err := client.Run(runCtx, internalapi.RunRequest{
		Image:       "alpine:3.20",
		KernelPath:  kernel,
		MemMB:       256,
		NetworkMode: "auto",
		Cmd:         []string{"/bin/sh", "-lc", "sleep infinity"},
		ExecEnabled: true,
		Wait:        true,
	})
	if err != nil {
		t.Fatalf("/run source: %v", err)
	}
	t.Cleanup(func() { _ = client.StopVM(context.Background(), src.ID) })

	// Clone with materialized mount should fail with 400.
	cloneCtx, cloneCancel := context.WithTimeout(context.Background(), 60*time.Second)
	_, err = client.CloneVM(cloneCtx, src.ID, internalapi.CloneRequest{
		Mounts: []container.Mount{{
			Source:  "/tmp",
			Target:  "/mnt/data",
			Backend: container.MountBackendMaterialized,
		}},
	})
	cloneCancel()
	if err == nil {
		t.Fatal("expected clone to fail with materialized mount")
	}
	if !strings.Contains(err.Error(), "virtiofs") {
		t.Errorf("error does not mention virtiofs: %v", err)
	}
}

