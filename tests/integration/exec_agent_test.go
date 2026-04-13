//go:build integration

package integration

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	internalapi "github.com/gocracker/gocracker/internal/api"
	"github.com/gocracker/gocracker/internal/compose"
)

func TestAPIServeRunExec(t *testing.T) {
	requirePrivilegedExecIntegration(t)
	kernel := requireIntegrationKernel(t)
	bins := buildProjectBinaries(t)
	contextDir := buildExecFixtureContext(t)
	cacheDir := filepath.Join(t.TempDir(), "cache")

	addr := freeLocalAddr(t)
	serverURL := "http://" + addr
	serveCmd := exec.Command(
		bins.gocracker,
		"serve",
		"--addr", addr,
		"--cache-dir", cacheDir,
		"--jailer-binary", bins.jailer,
		"--vmm-binary", bins.vmm,
	)
	var serveLog lockedBuffer
	serveCmd.Stdout = &serveLog
	serveCmd.Stderr = &serveLog
	serveCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := serveCmd.Start(); err != nil {
		t.Fatalf("start serve command: %v", err)
	}
	defer stopCommand(t, serveCmd)
	waitForAPI(t, serverURL, 45*time.Second)

	client := internalapi.NewClient(serverURL)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	runResp, err := client.Run(ctx, internalapi.RunRequest{
		Dockerfile:  filepath.Join(contextDir, "Dockerfile"),
		Context:     contextDir,
		KernelPath:  kernel,
		MemMB:       256,
		DiskSizeMB:  256,
		ExecEnabled: true,
	})
	if err != nil {
		t.Fatalf("api run: %v\nserve log:\n%s", err, serveLog.String())
	}
	waitForVMByID(t, client, runResp.ID, 45*time.Second)

	resp := waitForExecResponse(t, client, runResp.ID, internalapi.ExecRequest{
		Command: []string{"/bin/sh", "-lc", "echo api-run-exec-ok"},
	}, 30*time.Second)
	if !strings.Contains(resp.Stdout, "api-run-exec-ok") {
		t.Fatalf("exec stdout = %q, want api-run-exec-ok", resp.Stdout)
	}

	streamCtx, streamCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer streamCancel()
	conn, err := client.ExecVMStream(streamCtx, runResp.ID, internalapi.ExecRequest{
		Command: []string{"/bin/sh", "-l"},
		Columns: 120,
		Rows:    40,
	})
	if err != nil {
		t.Fatalf("exec stream: %v\nserve log:\n%s", err, serveLog.String())
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, "echo api-run-stream-ok\nexit\n"); err != nil {
		t.Fatalf("write exec stream commands: %v", err)
	}
	closeNetWriterForTest(conn)
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	streamOutput, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read exec stream output: %v", err)
	}
	if !strings.Contains(string(streamOutput), "api-run-stream-ok") {
		t.Fatalf("exec stream output = %q, want api-run-stream-ok", string(streamOutput))
	}

	if err := client.StopVM(context.Background(), runResp.ID); err != nil {
		t.Fatalf("stop vm: %v", err)
	}
}

func TestCLIComposeServeExec(t *testing.T) {
	requirePrivilegedExecIntegration(t)
	kernel := requireIntegrationKernel(t)
	bins := buildProjectBinaries(t)
	fixtureDir := buildComposeExecFixture(t)
	composeFile := filepath.Join(fixtureDir, "docker-compose.yml")
	cacheDir := filepath.Join(t.TempDir(), "cache")

	addr := freeLocalAddr(t)
	serverURL := "http://" + addr
	serveCmd := exec.Command(
		bins.gocracker,
		"serve",
		"--addr", addr,
		"--cache-dir", cacheDir,
		"--jailer-binary", bins.jailer,
		"--vmm-binary", bins.vmm,
	)
	var serveLog lockedBuffer
	serveCmd.Stdout = &serveLog
	serveCmd.Stderr = &serveLog
	serveCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := serveCmd.Start(); err != nil {
		t.Fatalf("start serve command: %v", err)
	}
	defer stopCommand(t, serveCmd)
	waitForAPI(t, serverURL, 45*time.Second)

	composeUp := exec.Command(bins.gocracker,
		"compose",
		"--server", serverURL,
		"--file", composeFile,
		"--kernel", kernel,
		"--cache-dir", cacheDir,
	)
	upOutput, err := composeUp.CombinedOutput()
	if err != nil {
		t.Fatalf("compose up: %v\n%s\nserve log:\n%s", err, upOutput, serveLog.String())
	}
	defer func() {
		composeDown := exec.Command(bins.gocracker, "compose", "down", "--server", serverURL, "--file", composeFile)
		if output, err := composeDown.CombinedOutput(); err != nil {
			t.Fatalf("compose down: %v\n%s\nserve log:\n%s", err, output, serveLog.String())
		}
	}()

	waitForComposeVM(t, serverURL, compose.StackNameForComposePath(composeFile), "debug", 60*time.Second)

	// Retry exec once — under CI load the VM may need a moment after
	// reaching "running" state before the exec agent is ready.
	var execOutput []byte
	for attempt := 0; attempt < 2; attempt++ {
		composeExec := exec.Command(bins.gocracker,
			"compose", "exec",
			"--server", serverURL,
			"--file", composeFile,
			"debug", "--", "/bin/sh", "-lc", "echo compose-exec-ok",
		)
		execOutput, err = composeExec.CombinedOutput()
		if err == nil {
			break
		}
		if attempt == 0 {
			time.Sleep(2 * time.Second)
		}
	}
	if err != nil {
		t.Fatalf("compose exec: %v\n%s\nserve log:\n%s", err, execOutput, serveLog.String())
	}
	if !strings.Contains(string(execOutput), "compose-exec-ok") {
		t.Fatalf("compose exec output = %q, want compose-exec-ok", string(execOutput))
	}
}

func TestCLIRunInteractiveExec(t *testing.T) {
	requirePrivilegedExecIntegration(t)
	kernel := requireIntegrationKernel(t)
	bins := buildProjectBinaries(t)
	cacheDir := filepath.Join(t.TempDir(), "cache")

	logPath := filepath.Join(t.TempDir(), "run-interactive.log")
	runPTYScript(t, bins.ptyrun, logPath, bins.gocracker,
		"run",
		"--image", "alpine:3.20",
		"--kernel", kernel,
		"--cache-dir", cacheDir,
		"--wait",
		"--tty", "force",
	)

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read interactive log: %v", err)
	}
	logText := string(data)
	for _, want := range []string{"A_B_C", "XY", "CTRL_C_OK"} {
		if !strings.Contains(logText, want) {
			t.Fatalf("interactive run log missing %q:\n%s", want, logText)
		}
	}
	for _, bad := range []string{"\x1b[1;1R", "\x1b[?2004h", "\x1b[?2004l"} {
		if strings.Contains(logText, bad) {
			t.Fatalf("interactive run log contains terminal noise %q:\n%s", bad, logText)
		}
	}
}

func TestCLIComposeExecInteractive(t *testing.T) {
	requirePrivilegedExecIntegration(t)
	kernel := requireIntegrationKernel(t)
	bins := buildProjectBinaries(t)
	fixtureDir := buildComposeExecFixture(t)
	composeFile := filepath.Join(fixtureDir, "docker-compose.yml")
	cacheDir := filepath.Join(t.TempDir(), "cache")

	addr := freeLocalAddr(t)
	serverURL := "http://" + addr
	serveCmd := exec.Command(
		bins.gocracker,
		"serve",
		"--addr", addr,
		"--cache-dir", cacheDir,
		"--jailer-binary", bins.jailer,
		"--vmm-binary", bins.vmm,
	)
	var serveLog lockedBuffer
	serveCmd.Stdout = &serveLog
	serveCmd.Stderr = &serveLog
	serveCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := serveCmd.Start(); err != nil {
		t.Fatalf("start serve command: %v", err)
	}
	defer stopCommand(t, serveCmd)
	waitForAPI(t, serverURL, 45*time.Second)

	composeUp := exec.Command(bins.gocracker,
		"compose",
		"--server", serverURL,
		"--file", composeFile,
		"--kernel", kernel,
		"--cache-dir", cacheDir,
	)
	upOutput, err := composeUp.CombinedOutput()
	if err != nil {
		t.Fatalf("compose up: %v\n%s\nserve log:\n%s", err, upOutput, serveLog.String())
	}
	defer func() {
		composeDown := exec.Command(bins.gocracker, "compose", "down", "--server", serverURL, "--file", composeFile)
		if output, err := composeDown.CombinedOutput(); err != nil {
			t.Fatalf("compose down: %v\n%s\nserve log:\n%s", err, output, serveLog.String())
		}
	}()

	waitForComposeVM(t, serverURL, compose.StackNameForComposePath(composeFile), "debug", 30*time.Second)

	logPath := filepath.Join(t.TempDir(), "compose-interactive.log")
	runPTYScript(t, bins.ptyrun, logPath, bins.gocracker,
		"compose", "exec",
		"--server", serverURL,
		"--file", composeFile,
		"debug",
	)

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read interactive compose log: %v", err)
	}
	logText := string(data)
	for _, want := range []string{"A_B_C", "XY", "CTRL_C_OK"} {
		if !strings.Contains(logText, want) {
			t.Fatalf("interactive compose log missing %q:\n%s", want, logText)
		}
	}
	for _, bad := range []string{"\x1b[1;1R", "\x1b[?2004h", "\x1b[?2004l"} {
		if strings.Contains(logText, bad) {
			t.Fatalf("interactive compose log contains terminal noise %q:\n%s", bad, logText)
		}
	}
}

type builtBinaries struct {
	gocracker string
	jailer    string
	vmm       string
	ptyrun    string
}

func requirePrivilegedExecIntegration(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("root is required for exec integration tests")
	}
	cleanupPrivilegedRuntime(t)
	t.Cleanup(func() { cleanupPrivilegedRuntime(t) })
}

func cleanupPrivilegedRuntime(t *testing.T) {
	t.Helper()
	for _, pattern := range []string{
		"integration.test vmm --vm-id",
		"gocracker vmm --vm-id",
		"gocracker-vmm --vm-id",
	} {
		cmd := exec.Command("pkill", "-9", "-f", pattern)
		_ = cmd.Run()
	}
	for _, glob := range []string{
		"/tmp/gocracker-vmm-worker-*",
		"/tmp/gocracker-jailer-*",
	} {
		matches, err := filepath.Glob(glob)
		if err != nil {
			t.Fatalf("glob %s: %v", glob, err)
		}
		for _, match := range matches {
			_ = os.RemoveAll(match)
		}
	}
}

func buildProjectBinaries(t *testing.T) builtBinaries {
	t.Helper()
	dir := t.TempDir()
	buildOne := func(name, pkg string) string {
		outPath := filepath.Join(dir, name)
		cmd := exec.Command("go", "build", "-o", outPath, pkg)
		cmd.Dir = repoRoot(t)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("build %s: %v\n%s", name, err, output)
		}
		return outPath
	}
	return builtBinaries{
		gocracker: buildOne("gocracker", "./cmd/gocracker"),
		jailer:    buildOne("gocracker-jailer", "./cmd/gocracker-jailer"),
		vmm:       buildOne("gocracker-vmm", "./cmd/gocracker-vmm"),
		ptyrun:    buildOne("ptyrun", "./tests/manual-smoke/cmd/ptyrun"),
	}
}

func runPTYScript(t *testing.T, ptyrunPath, logPath string, command ...string) {
	t.Helper()
	inputPath := filepath.Join(t.TempDir(), "input.txt")
	script := "echo A_B_C\recho XZ\x7fY\rsleep 20\r\x03echo CTRL_C_OK\rexit\r"
	if err := os.WriteFile(inputPath, []byte(script), 0644); err != nil {
		t.Fatalf("write PTY script: %v", err)
	}
	args := []string{
		"--log", logPath,
		"--input", inputPath,
		"--ready-timeout", "90s",
		"--exit-timeout", "90s",
		"--",
	}
	args = append(args, command...)
	cmd := exec.Command(ptyrunPath, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		transcript, _ := os.ReadFile(logPath)
		t.Fatalf("ptyrun: %v\n%s\ntranscript:\n%s", err, output, transcript)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}
	return root
}

func buildExecFixtureContext(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	shellPath := buildGuestProgram(t, miniShellSource)
	copyFileIntoContext(t, shellPath, filepath.Join(dir, "sh"))
	dockerfile := "FROM scratch\nCOPY sh /bin/sh\nCMD [\"/bin/sh\", \"-lc\", \"sleep infinity\"]\n"
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0644); err != nil {
		t.Fatalf("write Dockerfile: %v", err)
	}
	return dir
}

func buildComposeExecFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	contextDir := filepath.Join(dir, "debug")
	if err := os.MkdirAll(contextDir, 0755); err != nil {
		t.Fatalf("mkdir context: %v", err)
	}
	shellPath := buildGuestProgram(t, miniShellSource)
	copyFileIntoContext(t, shellPath, filepath.Join(contextDir, "sh"))
	dockerfile := "FROM scratch\nCOPY sh /bin/sh\nCMD [\"/bin/sh\", \"-lc\", \"sleep infinity\"]\n"
	if err := os.WriteFile(filepath.Join(contextDir, "Dockerfile"), []byte(dockerfile), 0644); err != nil {
		t.Fatalf("write Dockerfile: %v", err)
	}
	composeYAML := "services:\n  debug:\n    build:\n      context: ./debug\n"
	if err := os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(composeYAML), 0644); err != nil {
		t.Fatalf("write compose file: %v", err)
	}
	return dir
}

func stopCommand(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if cmd == nil || cmd.Process == nil {
		return
	}
	// Kill the entire process group (parent + children like gocracker-vmm,
	// virtiofsd) so inherited stdout pipes get closed and cmd.Wait returns.
	pgid := cmd.Process.Pid
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-time.After(5 * time.Second):
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			// Give up — don't block test cleanup forever.
		}
	case <-done:
	}
}

func freeLocalAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve local port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func closeNetWriterForTest(conn net.Conn) {
	type closeWriter interface {
		CloseWrite() error
	}
	if cw, ok := conn.(closeWriter); ok {
		_ = cw.CloseWrite()
	}
}

func waitForAPI(t *testing.T, serverURL string, timeout time.Duration) {
	t.Helper()
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := client.Get(serverURL + "/vms")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("API server %s did not become ready in %s", serverURL, timeout)
}

func waitForVMByID(t *testing.T, client *internalapi.Client, id string, timeout time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		info, err := client.GetVM(ctx, id)
		if err == nil && info.ID == id {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("vm %s not visible by API in time", id)
		case <-ticker.C:
		}
	}
}

func waitForComposeVM(t *testing.T, serverURL, stackName, serviceName string, timeout time.Duration) {
	t.Helper()
	client := internalapi.NewClient(serverURL)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		vms, err := client.ListVMs(ctx, map[string]string{
			"orchestrator": "compose",
			"stack":        stackName,
			"service":      serviceName,
		})
		if err == nil && len(vms) > 0 {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("compose VM %s/%s not visible by API in time", stackName, serviceName)
		case <-ticker.C:
		}
	}
}

func waitForExecResponse(t *testing.T, client *internalapi.Client, id string, req internalapi.ExecRequest, timeout time.Duration) internalapi.ExecResponse {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		resp, err := client.ExecVM(ctx, id, req)
		if err == nil {
			return resp
		}
		select {
		case <-ctx.Done():
			t.Fatalf("exec for vm %s did not succeed in time: %v", id, err)
		case <-ticker.C:
		}
	}
}

const miniShellSource = `
package main

import (
	"bufio"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

func main() {
	args := os.Args[1:]
	switch {
	case len(args) >= 2 && args[0] == "-lc":
		runCommand(strings.TrimSpace(args[1]))
	case len(args) >= 1 && (args[0] == "-l" || args[0] == "-i"):
		fmt.Println("mini-shell-ready")
		fmt.Print("gocracker:/# ")
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			runCommand(strings.TrimSpace(scanner.Text()))
			fmt.Print("gocracker:/# ")
		}
	default:
		runCommand(strings.TrimSpace(strings.Join(args, " ")))
	}
}

func runCommand(cmd string) {
	switch {
	case cmd == "", cmd == "true":
		return
	case cmd == "sleep infinity", strings.HasPrefix(cmd, "sleep "):
		sigs := make(chan os.Signal, 2)
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
		<-sigs
	case strings.HasPrefix(cmd, "echo "):
		fmt.Println(strings.TrimPrefix(cmd, "echo "))
	case cmd == "uname -a":
		fmt.Println("Linux gocracker-test 0.0.0 #1 SMP")
	case cmd == "exit":
		os.Exit(0)
	default:
		fmt.Printf("unsupported command: %s\n", cmd)
	}
}
`
