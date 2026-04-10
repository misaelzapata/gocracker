package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	internalapi "github.com/gocracker/gocracker/internal/api"
)

type Binaries struct {
	Gocracker string
	Jailer    string
	VMM       string
	PTYRun    string
}

type Harness struct {
	Root     string
	Kernel   string
	Binaries Binaries
}

type Case struct {
	t           *testing.T
	Harness     *Harness
	RootDir     string
	CacheDir    string
	SnapshotDir string
	LogDir      string
}

type Server struct {
	t       *testing.T
	URL     string
	cmd     *exec.Cmd
	logPath string
}

var (
	buildOnce     sync.Once
	builtHarness  *Harness
	buildErr      error
	buildErrMutex sync.Mutex
)

func RequireDarwinE2E(t *testing.T) *Harness {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("Darwin e2e suite only runs on macOS")
	}
	if os.Getenv("GOCRACKER_E2E") == "" {
		t.Skip("set GOCRACKER_E2E=1 to enable Darwin e2e tests")
	}
	buildOnce.Do(func() {
		builtHarness, buildErr = buildHarness()
	})
	buildErrMutex.Lock()
	defer buildErrMutex.Unlock()
	if buildErr != nil {
		t.Fatalf("prepare Darwin e2e harness: %v", buildErr)
	}
	return builtHarness
}

func (h *Harness) NewCase(t *testing.T, name string) *Case {
	t.Helper()
	base := filepath.Join(os.TempDir(), "gocracker-e2e", sanitizeName(name), fmt.Sprintf("%d", time.Now().UnixNano()))
	if err := os.MkdirAll(base, 0755); err != nil {
		t.Fatalf("create case dir %s: %v", base, err)
	}
	c := &Case{
		t:           t,
		Harness:     h,
		RootDir:     base,
		CacheDir:    filepath.Join(base, "cache"),
		SnapshotDir: filepath.Join(base, "snapshots"),
		LogDir:      filepath.Join(base, "logs"),
	}
	for _, dir := range []string{c.CacheDir, c.SnapshotDir, c.LogDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("create case path %s: %v", dir, err)
		}
	}
	return c
}

func (c *Case) SupervisorURL() string {
	return "unix://" + filepath.Join(c.CacheDir, "supervisor.sock")
}

func (c *Case) Client() *internalapi.Client {
	return internalapi.NewClient(c.SupervisorURL())
}

func (c *Case) RunCLI(args ...string) string {
	c.t.Helper()
	cmd := exec.Command(c.Harness.Binaries.Gocracker, args...)
	cmd.Dir = c.Harness.Root
	output, err := cmd.CombinedOutput()
	if err != nil {
		c.t.Fatalf("run %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return string(output)
}

func (c *Case) RunCLIWithEnv(env map[string]string, args ...string) string {
	c.t.Helper()
	cmd := exec.Command(c.Harness.Binaries.Gocracker, args...)
	cmd.Dir = c.Harness.Root
	cmd.Env = append(os.Environ(), encodeEnv(env)...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		c.t.Fatalf("run %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return string(output)
}

func (c *Case) RunPTY(logName string, args ...string) string {
	c.t.Helper()
	logPath := filepath.Join(c.LogDir, logName)
	inputPath := filepath.Join(c.RootDir, logName+".input")
	script := "echo A_B_C\recho XZ\x7fY\rsleep 20\r\x03echo CTRL_C_OK\rexit\r"
	if err := os.WriteFile(inputPath, []byte(script), 0644); err != nil {
		c.t.Fatalf("write PTY input: %v", err)
	}
	cmdArgs := []string{
		"--log", logPath,
		"--input", inputPath,
		"--ready-timeout", "90s",
		"--exit-timeout", "90s",
		"--",
	}
	cmdArgs = append(cmdArgs, c.Harness.Binaries.Gocracker)
	cmdArgs = append(cmdArgs, args...)
	cmd := exec.Command(c.Harness.Binaries.PTYRun, cmdArgs...)
	cmd.Dir = c.Harness.Root
	output, err := cmd.CombinedOutput()
	if err != nil {
		transcript, _ := os.ReadFile(logPath)
		c.t.Fatalf("ptyrun %s: %v\n%s\ntranscript:\n%s", strings.Join(args, " "), err, output, transcript)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		c.t.Fatalf("read PTY log %s: %v", logPath, err)
	}
	return string(data)
}

func (c *Case) WaitForSingleVM(timeout time.Duration) internalapi.VMInfo {
	c.t.Helper()
	deadline := time.Now().Add(timeout)
	client := c.Client()
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		vms, err := client.ListVMs(ctx, nil)
		cancel()
		if err == nil && len(vms) == 1 {
			return vms[0]
		}
		time.Sleep(250 * time.Millisecond)
	}
	c.t.Fatalf("expected one VM via %s within %s", c.SupervisorURL(), timeout)
	return internalapi.VMInfo{}
}

func (c *Case) WaitForServiceVM(serverURL, stack, service string, timeout time.Duration) internalapi.VMInfo {
	c.t.Helper()
	client := internalapi.NewClient(serverURL)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		vms, err := client.ListVMs(ctx, map[string]string{
			"orchestrator": "compose",
			"stack":        stack,
			"service":      service,
		})
		cancel()
		if err == nil && len(vms) > 0 {
			return vms[0]
		}
		time.Sleep(250 * time.Millisecond)
	}
	c.t.Fatalf("compose service %s/%s did not appear within %s", stack, service, timeout)
	return internalapi.VMInfo{}
}

func (c *Case) ExecVM(serverURL, id string, command ...string) internalapi.ExecResponse {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	resp, err := internalapi.NewClient(serverURL).ExecVM(ctx, id, internalapi.ExecRequest{Command: command})
	if err != nil {
		c.t.Fatalf("exec vm %s %v: %v", id, command, err)
	}
	return resp
}

func (c *Case) ExecVMStream(serverURL, id string, script string) string {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	conn, err := internalapi.NewClient(serverURL).ExecVMStream(ctx, id, internalapi.ExecRequest{
		Command: []string{"/bin/sh", "-l"},
		Columns: 120,
		Rows:    40,
	})
	if err != nil {
		c.t.Fatalf("exec stream %s: %v", id, err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, script); err != nil {
		c.t.Fatalf("write exec stream script: %v", err)
	}
	closeConnWrite(conn)
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	data, err := io.ReadAll(conn)
	if err != nil {
		c.t.Fatalf("read exec stream output: %v", err)
	}
	return string(data)
}

func (c *Case) StartServer() *Server {
	c.t.Helper()
	addr := freeLocalAddr(c.t)
	url := "http://" + addr
	logPath := filepath.Join(c.LogDir, "serve.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		c.t.Fatalf("create serve log: %v", err)
	}
	cmd := exec.Command(
		c.Harness.Binaries.Gocracker,
		"serve",
		"--addr", addr,
		"--cache-dir", c.CacheDir,
		"--state-dir", filepath.Join(c.RootDir, "state"),
		"--jailer-binary", c.Harness.Binaries.Jailer,
		"--vmm-binary", c.Harness.Binaries.VMM,
	)
	cmd.Dir = c.Harness.Root
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		c.t.Fatalf("start serve: %v", err)
	}
	c.t.Cleanup(func() {
		stopProcess(cmd)
		_ = logFile.Close()
	})
	waitForAPI(c.t, url, 45*time.Second)
	return &Server{t: c.t, URL: url, cmd: cmd, logPath: logPath}
}

func (c *Case) WaitHTTPContains(url, want string, timeout time.Duration) {
	c.t.Helper()
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			data, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if readErr == nil && strings.Contains(string(data), want) {
				return
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	c.t.Fatalf("url %s did not contain %q within %s", url, want, timeout)
}

func (c *Case) WaitJSONField(url string, timeout time.Duration, field, want string) {
	c.t.Helper()
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			var payload map[string]any
			decodeErr := json.NewDecoder(resp.Body).Decode(&payload)
			_ = resp.Body.Close()
			if decodeErr == nil && fmt.Sprint(payload[field]) == want {
				return
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	c.t.Fatalf("url %s did not report %s=%q within %s", url, field, want, timeout)
}

func (c *Case) PostJSON(url string, body string) string {
	c.t.Helper()
	resp, err := http.Post(url, "application/json", bytes.NewBufferString(body))
	if err != nil {
		c.t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		c.t.Fatalf("read POST %s response: %v", url, err)
	}
	if resp.StatusCode >= http.StatusBadRequest {
		c.t.Fatalf("POST %s returned %s: %s", url, resp.Status, data)
	}
	return string(data)
}

func (c *Case) Get(url string) string {
	c.t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		c.t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		c.t.Fatalf("read GET %s response: %v", url, err)
	}
	if resp.StatusCode >= http.StatusBadRequest {
		c.t.Fatalf("GET %s returned %s: %s", url, resp.Status, data)
	}
	return string(data)
}

func (c *Case) CopyFixture(src string) string {
	c.t.Helper()
	dst := filepath.Join(c.RootDir, "fixtures", filepath.Base(src))
	if err := copyTree(src, dst); err != nil {
		c.t.Fatalf("copy fixture %s -> %s: %v", src, dst, err)
	}
	return dst
}

func buildHarness() (*Harness, error) {
	root, err := repoRoot()
	if err != nil {
		return nil, err
	}
	kernel := filepath.Join(root, "artifacts", "kernels", "gocracker-guest-standard-arm64-Image")
	if _, err := os.Stat(kernel); err != nil {
		return nil, fmt.Errorf("Darwin kernel not found at %s", kernel)
	}
	binDir := strings.TrimSpace(os.Getenv("GOCRACKER_E2E_BIN_DIR"))
	if binDir == "" {
		binDir = root
	}
	buildDir := filepath.Join(os.TempDir(), "gocracker-e2e-build")
	if err := os.MkdirAll(buildDir, 0755); err != nil {
		return nil, err
	}
	bins := Binaries{
		Gocracker: filepath.Join(binDir, "gocracker"),
		Jailer:    filepath.Join(binDir, "gocracker-jailer"),
		VMM:       filepath.Join(binDir, "gocracker-vmm"),
		PTYRun:    filepath.Join(buildDir, "ptyrun"),
	}
	for out, pkg := range map[string]string{
		bins.PTYRun: "./tests/manual-smoke/cmd/ptyrun",
	} {
		cmd := exec.Command("go", "build", "-o", out, pkg)
		cmd.Dir = root
		output, err := cmd.CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("build %s: %v\n%s", pkg, err, output)
		}
	}
	for _, bin := range []string{bins.Gocracker, bins.Jailer, bins.VMM} {
		if _, err := os.Stat(bin); err != nil {
			return nil, fmt.Errorf("required signed Darwin binary %s is missing; run `make build-darwin-e2e` or set GOCRACKER_E2E_BIN_DIR", bin)
		}
		if err := verifyEntitlements(bin); err != nil {
			return nil, err
		}
	}
	if err := verifyBinaryLaunchable(bins.Gocracker, "version"); err != nil {
		identityMsg := "no valid codesigning identities found"
		if count, countErr := signingIdentityCount(); countErr == nil && count > 0 {
			identityMsg = fmt.Sprintf("%d valid codesigning identities found", count)
		}
		return nil, fmt.Errorf("signed gocracker binary failed to launch: %w\nhost signing state: %s\nthis host likely rejects ad-hoc com.apple.vm.networking binaries; use a real signing identity and rebuild with `make build-darwin-e2e`", err, identityMsg)
	}
	for _, check := range []struct {
		path string
		args []string
	}{
		{path: bins.Jailer, args: nil},
		{path: bins.VMM, args: nil},
	} {
		if err := verifyBinaryStarts(check.path, check.args...); err != nil {
			return nil, fmt.Errorf("signed helper binary %s failed to launch: %w", filepath.Base(check.path), err)
		}
	}
	return &Harness{
		Root:     root,
		Kernel:   kernel,
		Binaries: bins,
	}, nil
}

func verifyEntitlements(bin string) error {
	cmd := exec.Command("codesign", "-d", "--entitlements", ":-", bin)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("inspect entitlements for %s: %v\n%s", filepath.Base(bin), err, output)
	}
	text := string(output)
	for _, want := range []string{
		"com.apple.security.virtualization",
		"com.apple.vm.networking",
	} {
		if !strings.Contains(text, want) {
			return fmt.Errorf("%s is missing entitlement %s", filepath.Base(bin), want)
		}
	}
	return nil
}

func verifyBinaryLaunchable(bin string, args ...string) error {
	cmd := exec.Command(bin, args...)
	output, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return fmt.Errorf("%s\n%s", err, bytes.TrimSpace(output))
	}
	return err
}

func verifyBinaryStarts(bin string, args ...string) error {
	cmd := exec.Command(bin, args...)
	output, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if exitErr.ExitCode() >= 0 {
			return nil
		}
		return fmt.Errorf("%s\n%s", err, bytes.TrimSpace(output))
	}
	return err
}

func signingIdentityCount() (int, error) {
	cmd := exec.Command("security", "find-identity", "-v", "-p", "codesigning")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return 0, err
	}
	text := string(output)
	var count int
	if _, scanErr := fmt.Sscanf(strings.TrimSpace(text), "%d valid identities found", &count); scanErr == nil {
		return count, nil
	}
	lines := strings.Split(text, "\n")
	if len(lines) > 0 {
		last := strings.TrimSpace(lines[len(lines)-1])
		if _, scanErr := fmt.Sscanf(last, "%d valid identities found", &count); scanErr == nil {
			return count, nil
		}
	}
	return 0, nil
}

func encodeEnv(values map[string]string) []string {
	if len(values) == 0 {
		return nil
	}
	env := make([]string, 0, len(values))
	for key, value := range values {
		env = append(env, key+"="+value)
	}
	return env
}

func repoRoot() (string, error) {
	return filepath.Abs(filepath.Join("..", "..", ".."))
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

func freeLocalAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve local addr: %v", err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

func stopProcess(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(os.Interrupt)
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		<-done
	}
}

func closeConnWrite(conn net.Conn) {
	type closeWriter interface {
		CloseWrite() error
	}
	if cw, ok := conn.(closeWriter); ok {
		_ = cw.CloseWrite()
	}
}

func sanitizeName(value string) string {
	replacer := strings.NewReplacer("/", "-", " ", "-", ":", "-", ".", "-")
	return replacer.Replace(strings.TrimSpace(value))
}

func copyTree(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return copyFile(src, dst, info.Mode())
	}
	if err := os.MkdirAll(dst, info.Mode()); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := copyTree(filepath.Join(src, entry.Name()), filepath.Join(dst, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
