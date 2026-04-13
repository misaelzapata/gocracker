//go:build integration

package integration

import (
	"context"
	"fmt"
	"io"
	"net"
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

func TestCLIComposeServeHealthcheckExecBinary(t *testing.T) {
	requirePrivilegedExecIntegration(t)
	kernel := requireIntegrationKernel(t)
	bins := buildProjectBinaries(t)
	fixtureDir := buildComposeHealthExecFixture(t)
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

	composeUp := exec.Command(
		bins.gocracker,
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

	stackName := compose.StackNameForComposePath(composeFile)
	waitForComposeVM(t, serverURL, stackName, "app", 30*time.Second)
	waitForComposeVM(t, serverURL, stackName, "dependent", 30*time.Second)
}

func TestComposeStackIsolationAndCleanup(t *testing.T) {
	requirePrivilegedExecIntegration(t)
	kernel := requireIntegrationKernel(t)
	bins := buildProjectBinaries(t)
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

	portA := freeLocalPort(t)
	portB := freeLocalPort(t)
	fixtureA := buildComposeIsolationFixture(t, "stack-a", portA)
	fixtureB := buildComposeIsolationFixture(t, "stack-b", portB)
	composeFileA := filepath.Join(fixtureA, "docker-compose.yml")
	composeFileB := filepath.Join(fixtureB, "docker-compose.yml")

	runComposeUp := func(composeFile string) {
		t.Helper()
		cmd := exec.Command(
			bins.gocracker,
			"compose",
			"--server", serverURL,
			"--file", composeFile,
			"--kernel", kernel,
			"--cache-dir", cacheDir,
		)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("compose up %s: %v\n%s\nserve log:\n%s", composeFile, err, output, serveLog.String())
		}
	}
	runComposeDown := func(composeFile string) {
		t.Helper()
		cmd := exec.Command(bins.gocracker, "compose", "down", "--server", serverURL, "--file", composeFile)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("compose down %s: %v\n%s\nserve log:\n%s", composeFile, err, output, serveLog.String())
		}
	}

	runComposeUp(composeFileA)
	stackAUp := true
	defer func() {
		if stackAUp {
			runComposeDown(composeFileA)
		}
	}()
	runComposeUp(composeFileB)
	stackBUp := true
	defer func() {
		if stackBUp {
			runComposeDown(composeFileB)
		}
	}()

	client := internalapi.NewClient(serverURL)
	infoA := waitForComposeVMInfo(t, client, compose.StackNameForComposePath(composeFileA), "app", 30*time.Second)
	infoB := waitForComposeVMInfo(t, client, compose.StackNameForComposePath(composeFileB), "app", 30*time.Second)
	ipA := strings.TrimSpace(infoA.Metadata["guest_ip"])
	ipB := strings.TrimSpace(infoB.Metadata["guest_ip"])
	if ipA == "" || ipB == "" {
		t.Fatalf("missing guest IPs: A=%q B=%q", ipA, ipB)
	}

	waitForProbeSuccess(t, client, infoA.ID, "127.0.0.1:8080")
	waitForProbeSuccess(t, client, infoB.ID, "127.0.0.1:8080")

	assertHostPortToken(t, portA, "stack-a")
	assertHostPortToken(t, portB, "stack-b")

	assertProbeFails(t, client, infoA.ID, net.JoinHostPort(ipB, "8080"))
	assertProbeFails(t, client, infoB.ID, net.JoinHostPort(ipA, "8080"))

	runResp, err := client.Run(context.Background(), internalapi.RunRequest{
		Dockerfile:  filepath.Join(fixtureA, "app", "Dockerfile"),
		Context:     filepath.Join(fixtureA, "app"),
		KernelPath:  kernel,
		MemMB:       256,
		DiskSizeMB:  512,
		CacheDir:    cacheDir,
		ExecEnabled: true,
	})
	if err != nil {
		t.Fatalf("api run isolated vm: %v\nserve log:\n%s", err, serveLog.String())
	}
	defer func() {
		_ = client.StopVM(context.Background(), runResp.ID)
	}()
	waitForVMByID(t, client, runResp.ID, 45*time.Second)
	assertProbeFails(t, client, runResp.ID, net.JoinHostPort(ipA, "8080"))

	runComposeDown(composeFileB)
	stackBUp = false
	runComposeDown(composeFileA)
	stackAUp = false
	waitForNetnsGone(t, "gcns-"+compose.StackNameForComposePath(composeFileA), 15*time.Second)
	waitForNetnsGone(t, "gcns-"+compose.StackNameForComposePath(composeFileB), 15*time.Second)
}

func waitForComposeVMInfo(t *testing.T, client *internalapi.Client, stackName, serviceName string, timeout time.Duration) internalapi.VMInfo {
	t.Helper()
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
			return vms[0]
		}
		select {
		case <-ctx.Done():
			t.Fatalf("compose VM %s/%s not visible by API in time", stackName, serviceName)
		case <-ticker.C:
		}
	}
}

func freeLocalPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve local port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

func assertHostPortToken(t *testing.T, port int, want string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	target := net.JoinHostPort("127.0.0.1", fmt.Sprintf("%d", port))
	var lastErr error
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", target, 2*time.Second)
		if err != nil {
			lastErr = err
			time.Sleep(250 * time.Millisecond)
			continue
		}
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		data, err := io.ReadAll(conn)
		_ = conn.Close()
		if err != nil {
			lastErr = err
			time.Sleep(250 * time.Millisecond)
			continue
		}
		if strings.Contains(string(data), want) {
			return
		}
		lastErr = fmt.Errorf("published port %d returned %q, want %q", port, data, want)
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("published port %d did not become ready: %v", port, lastErr)
}

func assertProbeFails(t *testing.T, client *internalapi.Client, vmID, target string) {
	t.Helper()
	resp := waitForExecResponse(t, client, vmID, internalapi.ExecRequest{
		Command: []string{"/bin/netprobe", "probe", target},
	}, 30*time.Second)
	if resp.ExitCode == 0 {
		t.Fatalf("probe from vm %s unexpectedly reached %s: stdout=%q stderr=%q", vmID, target, resp.Stdout, resp.Stderr)
	}
}

func waitForProbeSuccess(t *testing.T, client *internalapi.Client, vmID, target string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var lastResp internalapi.ExecResponse
	for time.Now().Before(deadline) {
		lastResp = waitForExecResponse(t, client, vmID, internalapi.ExecRequest{
			Command: []string{"/bin/netprobe", "probe", target},
		}, 30*time.Second)
		if lastResp.ExitCode == 0 {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("probe from vm %s to %s did not succeed in time: stdout=%q stderr=%q exit=%d", vmID, target, lastResp.Stdout, lastResp.Stderr, lastResp.ExitCode)
}

func waitForNetnsGone(t *testing.T, name string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := exec.Command("ip", "netns", "list").CombinedOutput()
		if err == nil && !strings.Contains(string(out), name) {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	out, _ := exec.Command("ip", "netns", "list").CombinedOutput()
	t.Fatalf("network namespace %s still present:\n%s", name, out)
}

func buildComposeHealthExecFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	appDir := filepath.Join(dir, "app")
	dependentDir := filepath.Join(dir, "dependent")
	if err := os.MkdirAll(appDir, 0755); err != nil {
		t.Fatalf("mkdir app fixture: %v", err)
	}
	if err := os.MkdirAll(dependentDir, 0755); err != nil {
		t.Fatalf("mkdir dependent fixture: %v", err)
	}
	netprobePath := buildGuestProgram(t, netprobeSource)
	shellPath := buildGuestProgram(t, execShellSource)
	copyFileIntoContext(t, netprobePath, filepath.Join(appDir, "netprobe"))
	copyFileIntoContext(t, shellPath, filepath.Join(appDir, "sh"))
	copyFileIntoContext(t, netprobePath, filepath.Join(dependentDir, "netprobe"))

	appDockerfile := "FROM scratch\nCOPY netprobe /bin/netprobe\nCOPY sh /bin/sh\nCMD [\"/bin/netprobe\", \"serve\", \"8080\", \"health-app\"]\n"
	if err := os.WriteFile(filepath.Join(appDir, "Dockerfile"), []byte(appDockerfile), 0644); err != nil {
		t.Fatalf("write app Dockerfile: %v", err)
	}
	dependentDockerfile := "FROM scratch\nCOPY netprobe /bin/netprobe\nCMD [\"/bin/netprobe\", \"serve\", \"9090\", \"dependent\"]\n"
	if err := os.WriteFile(filepath.Join(dependentDir, "Dockerfile"), []byte(dependentDockerfile), 0644); err != nil {
		t.Fatalf("write dependent Dockerfile: %v", err)
	}

	composeYAML := strings.Join([]string{
		"services:",
		"  app:",
		"    build:",
		"      context: ./app",
		"    healthcheck:",
		"      test: [\"CMD-SHELL\", \"/bin/netprobe probe 127.0.0.1:8080\"]",
		"      interval: 1s",
		"      timeout: 2s",
		"      retries: 20",
		"  dependent:",
		"    build:",
		"      context: ./dependent",
		"    depends_on:",
		"      app:",
		"        condition: service_healthy",
		"    command: [\"/bin/netprobe\", \"serve\", \"9090\", \"dependent\"]",
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(composeYAML), 0644); err != nil {
		t.Fatalf("write compose file: %v", err)
	}
	return dir
}

func buildComposeIsolationFixture(t *testing.T, token string, hostPort int) string {
	t.Helper()
	dir := t.TempDir()
	appDir := filepath.Join(dir, "app")
	if err := os.MkdirAll(appDir, 0755); err != nil {
		t.Fatalf("mkdir app fixture: %v", err)
	}
	netprobePath := buildGuestProgram(t, netprobeSource)
	copyFileIntoContext(t, netprobePath, filepath.Join(appDir, "netprobe"))
	dockerfile := fmt.Sprintf("FROM scratch\nCOPY netprobe /bin/netprobe\nCMD [\"/bin/netprobe\", \"serve\", \"8080\", %q]\n", token)
	if err := os.WriteFile(filepath.Join(appDir, "Dockerfile"), []byte(dockerfile), 0644); err != nil {
		t.Fatalf("write Dockerfile: %v", err)
	}
	composeYAML := fmt.Sprintf("services:\n  app:\n    build:\n      context: ./app\n    ports:\n      - \"%d:8080\"\n", hostPort)
	if err := os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(composeYAML), 0644); err != nil {
		t.Fatalf("write compose file: %v", err)
	}
	return dir
}

const execShellSource = `
package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

func main() {
	args := os.Args[1:]
	if len(args) >= 2 && (args[0] == "-lc" || args[0] == "-c") {
		run(strings.TrimSpace(args[1]))
		return
	}
	if len(args) == 0 {
		return
	}
	run(strings.Join(args, " "))
}

func run(line string) {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return
	}
	cmd := exec.Command(fields[0], fields[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			os.Exit(exitErr.ExitCode())
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
`

const netprobeSource = `
package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: netprobe <serve|probe>")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		if len(os.Args) != 4 {
			fmt.Fprintln(os.Stderr, "usage: netprobe serve <port> <token>")
			os.Exit(2)
		}
		serve(os.Args[2], os.Args[3])
	case "probe":
		if len(os.Args) != 3 {
			fmt.Fprintln(os.Stderr, "usage: netprobe probe <host:port>")
			os.Exit(2)
		}
		probe(os.Args[2])
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		os.Exit(2)
	}
}

func serve(port, token string) {
	ln, err := net.Listen("tcp", ":"+port)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for {
		conn, err := ln.Accept()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		_, _ = io.WriteString(conn, token)
		_ = conn.Close()
	}
}

func probe(target string) {
	conn, err := net.DialTimeout("tcp", target, 2*time.Second)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	data, err := io.ReadAll(conn)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Print(string(data))
}
`
