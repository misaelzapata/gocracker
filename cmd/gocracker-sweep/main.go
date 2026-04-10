package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	internalapi "github.com/gocracker/gocracker/internal/api"
	"github.com/gocracker/gocracker/internal/compose"
)

type manifestCase struct {
	ID          string
	Kind        string
	URL         string
	Ref         string
	Path        string
	Stack       string
	Mode        string
	ProbeType   string
	ProbeTarget string
	ProbeExpect string
	MemMB       int
	DiskMB      int
	Notes       string
}

type ttyProbe struct {
	ID      string
	Service string
	Command string
	Expect  string
}

type runnerConfig struct {
	ManifestPath   string
	KernelPath     string
	GocrackerBin   string
	VMMBinary      string
	JailerBinary   string
	LogDir         string
	CloneCacheDir  string
	CacheRoot      string
	IDsFile        string
	ExcludeIDsFile string
	TTYManifest    string
	ID             string
	Kind           string
	Filter         string
	ListOnly       bool
	Refresh        bool
	Privileged     bool
	Limit          int
	BootTimeout    time.Duration
	ServiceWindow  time.Duration
	ShardIndex     int
	ShardTotal     int
}

type caseResult struct {
	ID       string
	Kind     string
	Status   string
	Duration time.Duration
	Message  string
	LogPath  string
}

func main() {
	cfg := parseFlags()
	if err := run(cfg); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "gocracker-sweep: %v\n", err)
		os.Exit(1)
	}
}

func parseFlags() runnerConfig {
	var cfg runnerConfig
	flag.StringVar(&cfg.ManifestPath, "manifest", "tests/external-repos/manifest.tsv", "external repo manifest path")
	flag.StringVar(&cfg.KernelPath, "kernel", strings.TrimSpace(os.Getenv("GOCRACKER_KERNEL")), "guest kernel path")
	flag.StringVar(&cfg.GocrackerBin, "bin", defaultEnv("GC_BIN", "./gocracker"), "gocracker binary path")
	flag.StringVar(&cfg.VMMBinary, "vmm-binary", defaultEnv("GC_VMM_BIN", "./gocracker-vmm"), "gocracker-vmm binary path")
	flag.StringVar(&cfg.JailerBinary, "jailer-binary", defaultEnv("GC_JAILER_BIN", "./gocracker-jailer"), "gocracker-jailer binary path")
	flag.StringVar(&cfg.LogDir, "log-dir", defaultEnv("EXT_REPO_LOG_DIR", filepath.Join(os.TempDir(), "gocracker-external-repos", time.Now().Format("20060102-150405"))), "results and logs directory")
	flag.StringVar(&cfg.CloneCacheDir, "clone-cache-dir", defaultEnv("EXT_REPO_CACHE_DIR", filepath.Join(os.TempDir(), "gocracker-external-repos", "cache", "clones")), "git mirror cache directory")
	flag.StringVar(&cfg.CacheRoot, "cache-root", defaultEnv("EXT_REPO_VM_CACHE_DIR", filepath.Join(os.TempDir(), "gocracker-external-repos", "cache", "vm")), "per-case VM/build cache root")
	flag.StringVar(&cfg.IDsFile, "ids-file", "", "newline-delimited ids file")
	flag.StringVar(&cfg.ExcludeIDsFile, "exclude-ids-file", "", "newline-delimited ids file to exclude from the selected set")
	flag.StringVar(&cfg.TTYManifest, "tty-manifest", "", "TTY probe manifest TSV")
	flag.StringVar(&cfg.ID, "id", "", "single manifest id")
	flag.StringVar(&cfg.Kind, "kind", "", "restrict to dockerfile or compose")
	flag.StringVar(&cfg.Filter, "filter", defaultEnv("EXT_REPO_FILTER", ""), "substring filter applied to id/url/path")
	flag.BoolVar(&cfg.ListOnly, "list", false, "list selected ids and exit")
	flag.BoolVar(&cfg.Refresh, "refresh", defaultEnv("EXT_REPO_REFRESH", "") == "1", "refresh cached git mirrors before running")
	flag.BoolVar(&cfg.Privileged, "sudo", defaultEnv("EXT_REPO_SUDO", "") == "1", "prefix gocracker commands with sudo -n")
	flag.IntVar(&cfg.Limit, "limit", parseEnvInt("EXT_REPO_LIMIT", 0), "limit selected cases after filtering")
	flag.DurationVar(&cfg.BootTimeout, "boot-timeout", parseEnvDuration("EXT_REPO_BOOT_TIMEOUT", 90*time.Second), "time to wait for a case to boot")
	flag.DurationVar(&cfg.ServiceWindow, "service-window", parseEnvDuration("EXT_REPO_SERVICE_WINDOW", 10*time.Second), "time to keep a successfully started service alive before passing")
	flag.IntVar(&cfg.ShardIndex, "shard-index", parseEnvInt("EXT_REPO_SHARD_INDEX", 0), "0-based shard index")
	flag.IntVar(&cfg.ShardTotal, "shard-total", parseEnvInt("EXT_REPO_SHARD_TOTAL", 1), "total number of shards")
	flag.Parse()
	return cfg
}

func run(cfg runnerConfig) error {
	if err := os.MkdirAll(cfg.LogDir, 0755); err != nil {
		return err
	}
	manifest, err := loadManifest(cfg.ManifestPath)
	if err != nil {
		return err
	}
	ids, err := loadSelectedIDs(cfg.ID, cfg.IDsFile, os.Getenv("EXT_REPO_IDS"))
	if err != nil {
		return err
	}
	excluded, err := loadIDSet("", cfg.ExcludeIDsFile, os.Getenv("EXT_REPO_EXCLUDE_IDS"))
	if err != nil {
		return err
	}
	ttyByID, err := loadTTYManifest(cfg.TTYManifest)
	if err != nil {
		return err
	}

	selected := selectCases(manifest, ids, excluded, cfg.Filter, cfg.Kind, cfg.Limit, cfg.ShardIndex, cfg.ShardTotal, ttyByID)
	if cfg.ListOnly {
		for _, tc := range selected {
			fmt.Println(tc.ID)
		}
		return nil
	}
	if len(selected) == 0 {
		return fmt.Errorf("no cases selected")
	}
	if strings.TrimSpace(cfg.KernelPath) == "" {
		return fmt.Errorf("--kernel or GOCRACKER_KERNEL is required")
	}
	for _, path := range []string{cfg.GocrackerBin, cfg.KernelPath} {
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("required path %s: %w", path, err)
		}
	}
	if runtime.GOOS == "darwin" {
		for _, check := range []struct {
			path           string
			args           []string
			requireSuccess bool
		}{
			{path: cfg.GocrackerBin, args: []string{"version"}, requireSuccess: true},
			{path: cfg.VMMBinary},
			{path: cfg.JailerBinary},
		} {
			if err := verifyDarwinBinary(check.path, check.requireSuccess, check.args...); err != nil {
				return err
			}
		}
	}
	if err := os.MkdirAll(cfg.CloneCacheDir, 0755); err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.CacheRoot, 0755); err != nil {
		return err
	}

	results := make([]caseResult, 0, len(selected))
	for _, tc := range selected {
		result := runOne(cfg, tc, ttyByID[tc.ID])
		results = append(results, result)
		statusLine := fmt.Sprintf("%s %s", result.Status, result.ID)
		if strings.TrimSpace(result.Message) != "" {
			statusLine += " - " + result.Message
		}
		fmt.Println(statusLine)
	}
	if err := writeResults(cfg.LogDir, results); err != nil {
		return err
	}
	for _, result := range results {
		if result.Status != "PASS" {
			return fmt.Errorf("one or more cases failed; see %s", cfg.LogDir)
		}
	}
	return nil
}

func runOne(cfg runnerConfig, tc manifestCase, tty *ttyProbe) caseResult {
	started := time.Now()
	caseDir := filepath.Join(cfg.LogDir, "cases", tc.ID)
	logPath := filepath.Join(caseDir, tc.ID+".log")
	result := caseResult{ID: tc.ID, Kind: tc.Kind, Status: "FAIL", LogPath: logPath}
	if err := os.MkdirAll(caseDir, 0755); err != nil {
		result.Message = err.Error()
		return result
	}
	logFile, err := os.Create(logPath)
	if err != nil {
		result.Message = err.Error()
		return result
	}
	defer logFile.Close()

	workDir, err := prepareRepoCheckout(cfg, tc, caseDir, logFile)
	if err != nil {
		result.Message = err.Error()
		return finalizeResult(result, started)
	}

	server, err := startCaseServer(cfg, caseDir, logFile)
	if err != nil {
		result.Message = err.Error()
		return finalizeResult(result, started)
	}
	defer server.Close()

	if err := runCaseLifecycle(cfg, tc, tty, workDir, server, logFile); err != nil {
		result.Message = err.Error()
		return finalizeResult(result, started)
	}

	result.Status = "PASS"
	result.Message = "booted and cleaned up"
	return finalizeResult(result, started)
}

func finalizeResult(result caseResult, started time.Time) caseResult {
	result.Duration = time.Since(started).Round(time.Millisecond)
	return result
}

type caseServer struct {
	url string
	cmd *exec.Cmd
}

func (s *caseServer) Close() {
	if s == nil || s.cmd == nil || s.cmd.Process == nil {
		return
	}
	_ = s.cmd.Process.Signal(os.Interrupt)
	done := make(chan struct{})
	go func() {
		_ = s.cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = s.cmd.Process.Kill()
		<-done
	}
}

func startCaseServer(cfg runnerConfig, caseDir string, logFile *os.File) (*caseServer, error) {
	addr, err := freeLocalAddr()
	if err != nil {
		return nil, err
	}
	args := []string{
		"serve",
		"--addr", addr,
		"--cache-dir", filepath.Join(cfg.CacheRoot, filepath.Base(caseDir)),
		"--state-dir", filepath.Join(caseDir, "state"),
		"--vmm-binary", cfg.VMMBinary,
		"--jailer-binary", cfg.JailerBinary,
	}
	cmd := commandWithPrivilege(cfg, cfg.GocrackerBin, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	url := "http://" + addr
	if err := waitForAPI(url, 45*time.Second); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, err
	}
	return &caseServer{url: url, cmd: cmd}, nil
}

func runCaseLifecycle(cfg runnerConfig, tc manifestCase, tty *ttyProbe, workDir string, server *caseServer, logFile *os.File) error {
	switch tc.Kind {
	case "dockerfile":
		vmID, err := startDockerfileCase(cfg, tc, workDir, server.url, logFile)
		if err != nil {
			return err
		}
		client := internalapi.NewClient(server.url)
		defer stopAllVMs(client, logFile)
		if tty != nil {
			return runTTYProbe(client, vmID, *tty, logFile)
		}
		time.Sleep(cfg.ServiceWindow)
		return nil
	case "compose":
		stackName, err := startComposeCase(cfg, tc, workDir, server.url, logFile)
		if err != nil {
			return err
		}
		defer stopComposeStack(cfg, tc, workDir, server.url, logFile)
		client := internalapi.NewClient(server.url)
		if tty != nil {
			var info internalapi.VMInfo
			if strings.TrimSpace(tty.Service) == "" {
				vms, err := waitForComposeStack(client, stackName, cfg.BootTimeout)
				if err != nil {
					return err
				}
				info = vms[0]
			} else {
				info, err = waitForComposeVM(client, stackName, tty.Service, cfg.BootTimeout)
				if err != nil {
					return err
				}
			}
			return runTTYProbe(client, info.ID, *tty, logFile)
		}
		time.Sleep(cfg.ServiceWindow)
		return nil
	default:
		return fmt.Errorf("unsupported kind %q", tc.Kind)
	}
}

func startDockerfileCase(cfg runnerConfig, tc manifestCase, workDir, serverURL string, logFile *os.File) (string, error) {
	fullPath := filepath.Join(workDir, tc.Path)
	var args []string
	if info, err := os.Stat(fullPath); err == nil && !info.IsDir() && looksLikeDockerfile(fullPath) {
		args = []string{
			"run",
			"--server", serverURL,
			"--dockerfile", fullPath,
			"--context", filepath.Dir(fullPath),
			"--kernel", cfg.KernelPath,
			"--mem", strconv.Itoa(defaultInt(tc.MemMB, 256)),
			"--disk", strconv.Itoa(defaultInt(tc.DiskMB, 4096)),
			"--tty", "off",
		}
	} else {
		args = []string{
			"repo",
			"--server", serverURL,
			"--url", workDir,
			"--subdir", tc.Path,
			"--kernel", cfg.KernelPath,
			"--mem", strconv.Itoa(defaultInt(tc.MemMB, 256)),
			"--tty", "off",
		}
	}
	output, err := runLoggedCommand(cfg, logFile, args...)
	if err != nil {
		return "", err
	}
	vmID := parsePrefixedID(output, "vm started: ")
	if strings.TrimSpace(vmID) == "" {
		client := internalapi.NewClient(serverURL)
		info, err := waitForSingleVM(client, cfg.BootTimeout)
		if err != nil {
			return "", err
		}
		return info.ID, nil
	}
	client := internalapi.NewClient(serverURL)
	if _, err := waitForVMByID(client, vmID, cfg.BootTimeout); err != nil {
		return "", err
	}
	return vmID, nil
}

func startComposeCase(cfg runnerConfig, tc manifestCase, workDir, serverURL string, logFile *os.File) (string, error) {
	composePath := filepath.Join(workDir, tc.Path)
	args := []string{
		"compose",
		"--server", serverURL,
		"--file", composePath,
		"--kernel", cfg.KernelPath,
		"--mem", strconv.Itoa(defaultInt(tc.MemMB, 256)),
		"--disk", strconv.Itoa(defaultInt(tc.DiskMB, 4096)),
	}
	if _, err := runLoggedCommand(cfg, logFile, args...); err != nil {
		return "", err
	}
	stackName := compose.StackNameForComposePath(composePath)
	client := internalapi.NewClient(serverURL)
	if _, err := waitForComposeStack(client, stackName, cfg.BootTimeout); err != nil {
		return "", err
	}
	return stackName, nil
}

func stopComposeStack(cfg runnerConfig, tc manifestCase, workDir, serverURL string, logFile *os.File) {
	composePath := filepath.Join(workDir, tc.Path)
	_, _ = runLoggedCommand(cfg, logFile, "compose", "down", "--server", serverURL, "--file", composePath)
}

func stopAllVMs(client *internalapi.Client, logFile *os.File) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	vms, err := client.ListVMs(ctx, nil)
	if err != nil {
		_, _ = fmt.Fprintf(logFile, "list vms for cleanup: %v\n", err)
		return
	}
	for _, vm := range vms {
		if err := client.StopVM(ctx, vm.ID); err != nil {
			_, _ = fmt.Fprintf(logFile, "stop vm %s: %v\n", vm.ID, err)
		}
	}
}

func runTTYProbe(client *internalapi.Client, vmID string, probe ttyProbe, logFile *os.File) error {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	conn, err := client.ExecVMStream(ctx, vmID, internalapi.ExecRequest{
		Command: []string{"/bin/sh", "-l"},
		Columns: 120,
		Rows:    40,
	})
	if err != nil {
		return err
	}
	defer conn.Close()
	script := strings.TrimSpace(probe.Command) + "\nexit\n"
	if _, err := io.WriteString(conn, script); err != nil {
		return err
	}
	closeConnWrite(conn)
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	data, err := io.ReadAll(conn)
	if err != nil {
		return err
	}
	_, _ = logFile.Write(data)
	transcript := string(data)
	for _, bad := range []string{"\x1b[1;1R", "\x1b[?2004h", "\x1b[?2004l"} {
		if strings.Contains(transcript, bad) {
			return fmt.Errorf("TTY transcript contained terminal noise %q", bad)
		}
	}
	if !strings.Contains(transcript, probe.Expect) {
		return fmt.Errorf("TTY transcript did not contain %q", probe.Expect)
	}
	return nil
}

func prepareRepoCheckout(cfg runnerConfig, tc manifestCase, caseDir string, logFile *os.File) (string, error) {
	mirrorDir := filepath.Join(cfg.CloneCacheDir, tc.ID+".git")
	if cfg.Refresh {
		_ = os.RemoveAll(mirrorDir)
	}
	if _, err := os.Stat(mirrorDir); errors.Is(err, os.ErrNotExist) {
		if _, err := runCommand(logFile, "git", "clone", "--mirror", tc.URL, mirrorDir); err != nil {
			return "", err
		}
	} else {
		if _, err := runCommand(logFile, "git", "-C", mirrorDir, "fetch", "--all", "--tags", "--prune"); err != nil {
			return "", err
		}
	}
	workDir := filepath.Join(caseDir, "repo")
	if _, err := runCommand(logFile, "git", "clone", mirrorDir, workDir); err != nil {
		return "", err
	}
	ref := strings.TrimSpace(tc.Ref)
	if ref != "" {
		if _, err := runCommand(logFile, "git", "-C", workDir, "checkout", "--quiet", ref); err != nil {
			return "", err
		}
	}
	return workDir, nil
}

func runLoggedCommand(cfg runnerConfig, logFile *os.File, args ...string) (string, error) {
	cmd := commandWithPrivilege(cfg, cfg.GocrackerBin, args...)
	var output bytes.Buffer
	cmd.Stdout = io.MultiWriter(logFile, &output)
	cmd.Stderr = io.MultiWriter(logFile, &output)
	err := cmd.Run()
	return output.String(), err
}

func runCommand(logFile *os.File, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	var output bytes.Buffer
	cmd.Stdout = io.MultiWriter(logFile, &output)
	cmd.Stderr = io.MultiWriter(logFile, &output)
	err := cmd.Run()
	return output.String(), err
}

func commandWithPrivilege(cfg runnerConfig, bin string, args ...string) *exec.Cmd {
	if cfg.Privileged {
		fullArgs := append([]string{"-n", bin}, args...)
		return exec.Command("sudo", fullArgs...)
	}
	return exec.Command(bin, args...)
}

func waitForAPI(serverURL string, timeout time.Duration) error {
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := client.Get(serverURL + "/vms")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("API server %s did not become ready in %s", serverURL, timeout)
}

func waitForSingleVM(client *internalapi.Client, timeout time.Duration) (internalapi.VMInfo, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		vms, err := client.ListVMs(ctx, nil)
		cancel()
		if err == nil && len(vms) == 1 {
			return vms[0], nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return internalapi.VMInfo{}, fmt.Errorf("expected one VM within %s", timeout)
}

func waitForVMByID(client *internalapi.Client, id string, timeout time.Duration) (internalapi.VMInfo, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		info, err := client.GetVM(ctx, id)
		cancel()
		if err == nil && info.ID == id {
			return info, nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return internalapi.VMInfo{}, fmt.Errorf("VM %s did not appear within %s", id, timeout)
}

func waitForComposeStack(client *internalapi.Client, stackName string, timeout time.Duration) ([]internalapi.VMInfo, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		vms, err := client.ListVMs(ctx, map[string]string{
			"orchestrator": "compose",
			"stack":        stackName,
		})
		cancel()
		if err == nil && len(vms) > 0 {
			return vms, nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return nil, fmt.Errorf("compose stack %s did not appear within %s", stackName, timeout)
}

func waitForComposeVM(client *internalapi.Client, stackName, serviceName string, timeout time.Duration) (internalapi.VMInfo, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		vms, err := client.ListVMs(ctx, map[string]string{
			"orchestrator": "compose",
			"stack":        stackName,
			"service":      serviceName,
		})
		cancel()
		if err == nil && len(vms) > 0 {
			return vms[0], nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return internalapi.VMInfo{}, fmt.Errorf("compose service %s/%s did not appear within %s", stackName, serviceName, timeout)
}

func writeResults(logDir string, results []caseResult) error {
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return err
	}
	if err := writeResultsTSV(filepath.Join(logDir, "results.tsv"), results); err != nil {
		return err
	}
	return writeSummary(filepath.Join(logDir, "summary.md"), results)
}

func writeResultsTSV(path string, results []caseResult) error {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	w.Comma = '\t'
	if err := w.Write([]string{"id", "kind", "status", "duration", "message", "log_path"}); err != nil {
		return err
	}
	for _, result := range results {
		if err := w.Write([]string{
			result.ID,
			result.Kind,
			result.Status,
			result.Duration.String(),
			result.Message,
			result.LogPath,
		}); err != nil {
			return err
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0644)
}

func writeSummary(path string, results []caseResult) error {
	var pass, fail int
	for _, result := range results {
		if result.Status == "PASS" {
			pass++
		} else {
			fail++
		}
	}
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "# External Repo Sweep Summary\n\n")
	fmt.Fprintf(&buf, "- PASS: %d\n", pass)
	fmt.Fprintf(&buf, "- FAIL: %d\n\n", fail)
	fmt.Fprintf(&buf, "| ID | Kind | Status | Duration | Message |\n")
	fmt.Fprintf(&buf, "| --- | --- | --- | --- | --- |\n")
	for _, result := range results {
		fmt.Fprintf(&buf, "| `%s` | `%s` | `%s` | `%s` | %s |\n",
			result.ID,
			result.Kind,
			result.Status,
			result.Duration,
			strings.ReplaceAll(result.Message, "|", "/"),
		)
	}
	return os.WriteFile(path, buf.Bytes(), 0644)
}

func loadManifest(path string) ([]manifestCase, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	reader := csv.NewReader(file)
	reader.Comma = '\t'
	reader.FieldsPerRecord = -1

	var cases []manifestCase
	for {
		record, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if len(record) == 0 {
			continue
		}
		record[0] = strings.TrimSpace(strings.TrimPrefix(record[0], "#"))
		if strings.EqualFold(record[0], "id") {
			continue
		}
		if len(record) < 13 {
			return nil, fmt.Errorf("manifest row has %d columns, want at least 13", len(record))
		}
		cases = append(cases, manifestCase{
			ID:          record[0],
			Kind:        record[1],
			URL:         record[2],
			Ref:         record[3],
			Path:        record[4],
			Stack:       record[5],
			Mode:        record[6],
			ProbeType:   record[7],
			ProbeTarget: record[8],
			ProbeExpect: record[9],
			MemMB:       parseInt(record[10], 0),
			DiskMB:      parseInt(record[11], 0),
			Notes:       valueAt(record, 12),
		})
	}
	sort.Slice(cases, func(i, j int) bool { return cases[i].ID < cases[j].ID })
	return cases, nil
}

func loadTTYManifest(path string) (map[string]*ttyProbe, error) {
	if strings.TrimSpace(path) == "" {
		return map[string]*ttyProbe{}, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	reader := csv.NewReader(file)
	reader.Comma = '\t'
	reader.FieldsPerRecord = -1

	probes := map[string]*ttyProbe{}
	for {
		record, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if len(record) == 0 {
			continue
		}
		record[0] = strings.TrimSpace(strings.TrimPrefix(record[0], "#"))
		if strings.EqualFold(record[0], "id") {
			continue
		}
		probe := &ttyProbe{
			ID:      valueAt(record, 0),
			Service: valueAt(record, 1),
			Command: valueAt(record, 2),
			Expect:  valueAt(record, 3),
		}
		if probe.Service == "" && len(record) == 3 {
			probe.Command = valueAt(record, 1)
			probe.Expect = valueAt(record, 2)
		}
		if probe.ID == "" {
			continue
		}
		probes[probe.ID] = probe
	}
	return probes, nil
}

func loadSelectedIDs(singleID, idsFile, envIDs string) (map[string]struct{}, error) {
	return loadIDSet(singleID, idsFile, envIDs)
}

func loadIDSet(singleID, idsFile, envIDs string) (map[string]struct{}, error) {
	ids := map[string]struct{}{}
	addID := func(id string) {
		id = strings.TrimSpace(id)
		if id != "" {
			ids[id] = struct{}{}
		}
	}
	addMany := func(raw string) {
		for _, id := range strings.Split(raw, ",") {
			addID(id)
		}
	}
	addID(singleID)
	addMany(envIDs)
	if strings.TrimSpace(idsFile) != "" {
		data, err := os.ReadFile(idsFile)
		if err != nil {
			return nil, err
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			addID(line)
		}
	}
	return ids, nil
}

func selectCases(all []manifestCase, ids, excluded map[string]struct{}, filter, kind string, limit, shardIndex, shardTotal int, tty map[string]*ttyProbe) []manifestCase {
	var selected []manifestCase
	ttyOnly := len(ids) == 0 && len(tty) > 0
	for _, tc := range all {
		if _, skip := excluded[tc.ID]; skip {
			continue
		}
		if ttyOnly {
			if _, ok := tty[tc.ID]; !ok {
				continue
			}
		}
		if len(ids) > 0 {
			if _, ok := ids[tc.ID]; !ok {
				if _, ok := tty[tc.ID]; !ok {
					continue
				}
			}
		}
		if strings.TrimSpace(kind) != "" && tc.Kind != kind {
			continue
		}
		if filter != "" {
			haystack := tc.ID + " " + tc.URL + " " + tc.Path
			if !strings.Contains(strings.ToLower(haystack), strings.ToLower(filter)) {
				continue
			}
		}
		selected = append(selected, tc)
	}
	if shardTotal <= 0 {
		shardTotal = 1
	}
	if shardIndex < 0 {
		shardIndex = 0
	}
	if shardTotal > 1 {
		sharded := make([]manifestCase, 0, len(selected))
		for i, tc := range selected {
			if i%shardTotal == shardIndex {
				sharded = append(sharded, tc)
			}
		}
		selected = sharded
	}
	if limit > 0 && len(selected) > limit {
		selected = selected[:limit]
	}
	return selected
}

func verifyDarwinBinary(bin string, requireSuccess bool, args ...string) error {
	cmd := exec.Command("codesign", "-d", "--entitlements", ":-", bin)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("inspect Darwin entitlements for %s: %v\n%s", bin, err, output)
	}
	text := string(output)
	for _, want := range []string{"com.apple.security.virtualization", "com.apple.vm.networking"} {
		if !strings.Contains(text, want) {
			return fmt.Errorf("%s is missing entitlement %s", bin, want)
		}
	}
	run := exec.Command(bin, args...)
	runOutput, runErr := run.CombinedOutput()
	if runErr == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) && !requireSuccess && exitErr.ExitCode() >= 0 {
		return nil
	}
	return fmt.Errorf("%s failed to launch after signing: %v\n%s\nthis host likely rejects ad-hoc com.apple.vm.networking binaries; rebuild with a real signing identity", bin, runErr, bytes.TrimSpace(runOutput))
}

func freeLocalAddr() (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer ln.Close()
	return ln.Addr().String(), nil
}

func closeConnWrite(conn net.Conn) {
	type closeWriter interface {
		CloseWrite() error
	}
	if cw, ok := conn.(closeWriter); ok {
		_ = cw.CloseWrite()
	}
}

func parsePrefixedID(output, prefix string) string {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(line, prefix))
		}
	}
	return ""
}

func looksLikeDockerfile(path string) bool {
	base := filepath.Base(path)
	return base == "Dockerfile" || strings.HasPrefix(base, "Dockerfile.") || strings.EqualFold(base, "dockerfile")
}

func parseEnvInt(key string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return value
}

func parseEnvDuration(key string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	if seconds, err := strconv.Atoi(raw); err == nil {
		return time.Duration(seconds) * time.Second
	}
	value, err := time.ParseDuration(raw)
	if err != nil {
		return fallback
	}
	return value
}

func defaultEnv(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func defaultInt(value, fallback int) int {
	if value > 0 {
		return value
	}
	return fallback
}

func parseInt(raw string, fallback int) int {
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return fallback
	}
	return value
}

func valueAt(record []string, idx int) string {
	if idx >= len(record) {
		return ""
	}
	return strings.TrimSpace(record[idx])
}
