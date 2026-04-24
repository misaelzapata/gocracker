package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/gocracker/gocracker/internal/api"
	internalapi "github.com/gocracker/gocracker/internal/api"
	"github.com/gocracker/gocracker/internal/buildinfo"
	"github.com/gocracker/gocracker/internal/buildserver"
	"github.com/gocracker/gocracker/internal/compose"
	"github.com/gocracker/gocracker/internal/console"
	"github.com/gocracker/gocracker/internal/guestexec"
	"github.com/gocracker/gocracker/internal/hostguard"
	"github.com/gocracker/gocracker/internal/jailer"
	"github.com/gocracker/gocracker/internal/oci"
	"github.com/gocracker/gocracker/internal/runtimecfg"
	"github.com/gocracker/gocracker/internal/tempprune"
	"github.com/gocracker/gocracker/internal/vmmserver"
	"github.com/gocracker/gocracker/internal/worker"
	"github.com/gocracker/gocracker/pkg/container"
	"github.com/gocracker/gocracker/pkg/vmm"
	mobyterm "github.com/moby/term"
)

const usage = `gocracker — lightweight microVM runtime (Firecracker-compatible)

Usage:
  gocracker <command> [flags]

Commands:
  run        Build and boot a microVM (image, Dockerfile, or local path)
  repo       Clone a git repo and boot its Dockerfile
  compose    Boot a docker-compose.yml stack as microVMs
  build      Build a disk image without booting
  snapshot   Take a snapshot of a running VM via the API
  restore    Restore and boot a VM from a snapshot
  migrate    Live-migrate a VM between gocracker API servers
  serve      Start the REST API server (Firecracker-compatible + extended)
  vmm        Start the single-VM Firecracker-compatible API worker
  build-worker Start the jailed build worker
  jailer     Start a Firecracker-style jailer for a worker/VMM
  version    Print build version, commit, date, and go runtime

Examples:
  # From OCI image
  gocracker run --image ubuntu:22.04 --kernel ./artifacts/kernels/gocracker-guest-standard-vmlinux

  # From local Dockerfile
  gocracker run --dockerfile ./Dockerfile --kernel ./artifacts/kernels/gocracker-guest-standard-vmlinux --mem 512

  # From git repo (auto-detects Dockerfile)
  gocracker repo --url https://github.com/user/myapp --kernel ./artifacts/kernels/gocracker-guest-standard-vmlinux

  # From local repo path
  gocracker repo --url ./myapp --kernel ./artifacts/kernels/gocracker-guest-standard-vmlinux

  # Boot a docker-compose.yml stack
  gocracker compose --file ./docker-compose.yml --kernel ./artifacts/kernels/gocracker-guest-standard-vmlinux

  # API server
  gocracker serve --addr :8080

  # Live migration between API servers
  gocracker migrate --source http://localhost:8080 --id vm-123 --dest http://host-b:8080
`

type interactiveMode struct {
	enabled bool
}

var (
	terminalGetFdInfo             = mobyterm.GetFdInfo
	terminalSetRaw                = mobyterm.SetRawTerminal
	terminalRestore               = mobyterm.RestoreTerminal
	terminalOutput      io.Writer = os.Stdout
	terminalInputReader           = os.Stdin
)

var ioCopyBufPool = sync.Pool{
	New: func() any {
		buf := make([]byte, 4096)
		return &buf
	},
}

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(1)
	}
	switch os.Args[1] {
	case "run":
		cmdRun(os.Args[2:])
	case "repo":
		cmdRepo(os.Args[2:])
	case "compose", "up":
		cmdCompose(os.Args[2:])
	case "build":
		cmdBuild(os.Args[2:])
	case "restore":
		cmdRestore(os.Args[2:])
	case "migrate":
		cmdMigrate(os.Args[2:])
	case "serve", "server":
		cmdServe(os.Args[2:])
	case "vmm":
		cmdVMM(os.Args[2:])
	case "build-worker":
		cmdBuildWorker(os.Args[2:])
	case "jailer":
		cmdJailer(os.Args[2:])
	case "version", "-v", "--version":
		fmt.Println(buildinfo.String())
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", os.Args[1])
		fmt.Print(usage)
		os.Exit(1)
	}
}

// ---- run ----

func cmdRun(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	image := fs.String("image", "", "OCI image ref (e.g. ubuntu:22.04)")
	df := fs.String("dockerfile", "", "Path to Dockerfile")
	ctx := fs.String("context", ".", "Build context directory")
	kernel := fs.String("kernel", "", "Kernel image path [required]")
	mem := fs.Uint64("mem", 256, "RAM in MiB")
	arch := fs.String("arch", runtime.GOARCH, "Guest architecture: amd64 or arm64 (same-arch only)")
	cpus := fs.Int("cpus", 1, "vCPU count")
	balloonTargetMiB := fs.Uint64("balloon-target-mib", 0, "Balloon target in MiB")
	balloonDeflateOnOOM := fs.Bool("balloon-deflate-on-oom", false, "Allow balloon deflate on guest OOM")
	balloonStatsIntervalS := fs.Int("balloon-stats-interval-s", 0, "Balloon statistics polling interval in seconds")
	balloonAuto := fs.String("balloon-auto", string(vmm.BalloonAutoOff), "Balloon auto policy: off or conservative")
	hotplugTotalMiB := fs.Uint64("hotplug-total-mib", 0, "Hotpluggable memory region total size in MiB")
	hotplugSlotMiB := fs.Uint64("hotplug-slot-mib", 0, "Hotpluggable memory slot size in MiB")
	hotplugBlockMiB := fs.Uint64("hotplug-block-mib", 0, "Hotpluggable memory block size in MiB")
	x86Boot := fs.String("x86-boot", string(vmm.X86BootAuto), "x86 boot mode: auto, acpi, or legacy")
	netMode := fs.String("net", "none", "network mode: none or auto")
	tap := fs.String("tap", "", "TAP interface (e.g. tap0)")
	disk := fs.Int("disk", 2048, "Disk size MiB")
	snap := fs.String("snapshot", "", "Restore from snapshot dir")
	cacheDir := fs.String("cache-dir", filepath.Join(os.TempDir(), "gocracker", "cache"), "Persistent cache directory")
	envStr := fs.String("env", "", "Comma-separated KEY=VALUE env vars")
	cmdStr := fs.String("cmd", "", "Override CMD")
	entrypointStr := fs.String("entrypoint", "", "Override ENTRYPOINT")
	workdir := fs.String("workdir", "", "Override working directory")
	id := fs.String("id", "", "VM identifier")
	wait := fs.Bool("wait", false, "Block until VM stops")
	ttyMode := fs.String("tty", "auto", "Console mode: auto, off, or force")
	jailerMode := fs.String("jailer", container.JailerModeOn, "Privilege model: on or off")
	rootfsPersistent := fs.Bool("rootfs-persistent", false, "Mount rootfs read-write directly (writes survive VM stop; slower boot). Default: Docker-style tmpfs overlay.")
	warm := fs.Bool("warm", false, "Auto snapshot-cache: restore from snapshot on cache hit (~3 ms); snapshot after cold boot on miss so next run is fast.")
	vsockUDSPath := fs.String("vsock-uds-path", "", "Absolute path for the VM's Firecracker-style vsock UDS. Clients dial it and send \"CONNECT <port>\\n\" to reach a guest vsock port. Empty = no UDS (HTTP /vms/{id}/vsock/connect still works).")
	buildArgs := multiKVFlag{}
	fs.Var(&buildArgs, "build-arg", "Build arg KEY=VALUE (repeatable)")
	fs.Parse(args)

	requireKernel(*kernel)
	*kernel = resolveRequiredExistingPath("kernel", *kernel)
	if *image == "" && *df == "" {
		fatal("--image or --dockerfile required")
	}

	interactive := mustInteractiveMode(*ttyMode, *wait)
	consoleIn := io.Reader(nil)
	consoleOut := io.Writer(nil)
	if interactive.enabled {
		consoleIn = bytes.NewReader(nil)
		consoleOut = io.Discard
	}

	runOpts := container.RunOptions{
		Image: *image, Dockerfile: *df, Context: *ctx,
		KernelPath: *kernel, MemMB: *mem, Arch: *arch, CPUs: *cpus, TapName: *tap, NetworkMode: normalizeNetworkMode(*netMode), X86Boot: vmm.X86BootMode(*x86Boot),
		DiskSizeMB: *disk, SnapshotDir: *snap,
		Env: splitComma(*envStr), Cmd: splitFields(*cmdStr),
		Entrypoint:      splitFields(*entrypointStr),
		WorkDir:         *workdir,
		BuildArgs:       buildArgs.Map(),
		PID1Mode:        pid1ModeForCLIWait(*wait),
		ID:              *id,
		CacheDir:        *cacheDir,
		// --warm: boot in InteractiveExec (idle exec agent, no CMD as PID 1)
		// so the snapshot is CMD-agnostic and any subsequent CMD can reuse it.
		ExecEnabled:     interactive.enabled || *warm,
		InteractiveExec: interactive.enabled || *warm,
		JailerMode:      *jailerMode,
		ConsoleOut:      consoleOut,
		ConsoleIn:       consoleIn,
		RootfsPersistent: *rootfsPersistent,
		WarmCapture:     *warm,
		VsockUDSPath:    *vsockUDSPath,
	}
	if *balloonTargetMiB > 0 || *balloonDeflateOnOOM || *balloonStatsIntervalS > 0 || strings.TrimSpace(*balloonAuto) != "" {
		runOpts.Balloon = &vmm.BalloonConfig{
			AmountMiB:             *balloonTargetMiB,
			DeflateOnOOM:          *balloonDeflateOnOOM,
			StatsPollingIntervalS: *balloonStatsIntervalS,
			Auto:                  mustBalloonAutoMode(*balloonAuto),
		}
	}
	if *hotplugTotalMiB > 0 || *hotplugSlotMiB > 0 || *hotplugBlockMiB > 0 {
		runOpts.MemoryHotplug = &vmm.MemoryHotplugConfig{
			TotalSizeMiB: *hotplugTotalMiB,
			SlotSizeMiB:  *hotplugSlotMiB,
			BlockSizeMiB: *hotplugBlockMiB,
		}
	}
	result := mustRun(runOpts)
	defer result.Close()
	if interactive.enabled {
		// Drain warm-capture BEFORE printing result or opening the shell. The
		// capture goroutine emits vsock-quiesce log lines and injects RST; doing
		// this first keeps those internal logs grouped with the boot output and
		// lets printResult + the shell prompt appear together, uninterrupted.
		drainWarmDone(result)
		printResult(result)
		if err := runLocalInteractiveVM(result, resolveInteractiveRunCommand(result.Config, runOpts)); err != nil {
			stopVMAndWait(result.VM, 15*time.Second)
			fatal(err.Error())
		}
		stopVMAndWait(result.VM, 15*time.Second)
		// Final newline to ensure the shell prompt redraws after VM stop messages.
		fmt.Println()
		return
	}
	// --warm non-interactive path: exec CMD if provided, wait for snapshot.
	if *warm {
		drainWarmDone(result)
		cmd := effectiveCommandSlice(runOpts.Cmd, imageDefaultCmd(result.Config))
		if len(cmd) > 0 {
			if err := runWarmCmd(result.VM, cmd); err != nil {
				stopVMAndWait(result.VM, 5*time.Second)
				fatal(err.Error())
			}
		}
		stopVMAndWait(result.VM, 5*time.Second)
		return
	}
	if *wait {
		waitVM(result.VM, nil)
	}
	drainWarmDone(result)
}

// ---- repo ----

func cmdRepo(args []string) {
	fs := flag.NewFlagSet("repo", flag.ExitOnError)
	url := fs.String("url", "", "Git repo URL or local path [required]")
	ref := fs.String("ref", "", "Branch/tag to checkout")
	subdir := fs.String("subdir", "", "Subdir inside repo")
	dockerfileFlag := fs.String("dockerfile", "", "Explicit Dockerfile path relative to --subdir (rescues non-canonical names like Dockerfile-envoy)")
	kernel := fs.String("kernel", "", "Kernel image path [required]")
	mem := fs.Uint64("mem", 256, "RAM in MiB")
	arch := fs.String("arch", runtime.GOARCH, "Guest architecture: amd64 or arm64 (same-arch only)")
	cpus := fs.Int("cpus", 1, "vCPU count")
	balloonTargetMiB := fs.Uint64("balloon-target-mib", 0, "Balloon target in MiB")
	balloonDeflateOnOOM := fs.Bool("balloon-deflate-on-oom", false, "Allow balloon deflate on guest OOM")
	balloonStatsIntervalS := fs.Int("balloon-stats-interval-s", 0, "Balloon statistics polling interval in seconds")
	balloonAuto := fs.String("balloon-auto", string(vmm.BalloonAutoOff), "Balloon auto policy: off or conservative")
	hotplugTotalMiB := fs.Uint64("hotplug-total-mib", 0, "Hotpluggable memory region total size in MiB")
	hotplugSlotMiB := fs.Uint64("hotplug-slot-mib", 0, "Hotpluggable memory slot size in MiB")
	hotplugBlockMiB := fs.Uint64("hotplug-block-mib", 0, "Hotpluggable memory block size in MiB")
	x86Boot := fs.String("x86-boot", string(vmm.X86BootAuto), "x86 boot mode: auto, acpi, or legacy")
	netMode := fs.String("net", "none", "network mode: none or auto")
	tap := fs.String("tap", "", "TAP interface")
	disk := fs.Int("disk", 2048, "Disk size MiB")
	snap := fs.String("snapshot", "", "Restore from snapshot dir")
	cacheDir := fs.String("cache-dir", filepath.Join(os.TempDir(), "gocracker", "cache"), "Persistent cache directory")
	envStr := fs.String("env", "", "Comma-separated KEY=VALUE env vars")
	cmdStr := fs.String("cmd", "", "Override CMD")
	entrypointStr := fs.String("entrypoint", "", "Override ENTRYPOINT")
	workdir := fs.String("workdir", "", "Override working directory")
	wait := fs.Bool("wait", false, "Block until VM stops")
	ttyMode := fs.String("tty", "auto", "Console mode: auto, off, or force")
	jailerMode := fs.String("jailer", container.JailerModeOn, "Privilege model: on or off")
	rootfsPersistent := fs.Bool("rootfs-persistent", false, "Mount rootfs read-write directly (writes survive VM stop; slower boot). Default: Docker-style tmpfs overlay.")
	buildArgs := multiKVFlag{}
	fs.Var(&buildArgs, "build-arg", "Build arg KEY=VALUE (repeatable)")
	fs.Parse(args)

	if *url == "" {
		fatal("--url required")
	}
	requireKernel(*kernel)
	*kernel = resolveRequiredExistingPath("kernel", *kernel)

	interactive := mustInteractiveMode(*ttyMode, *wait)
	consoleIn := io.Reader(nil)
	consoleOut := io.Writer(nil)
	if interactive.enabled {
		consoleIn = bytes.NewReader(nil)
		consoleOut = io.Discard
	}

	runOpts := container.RunOptions{
		RepoURL: *url, RepoRef: *ref, RepoSubdir: *subdir, RepoDockerfile: *dockerfileFlag,
		KernelPath: *kernel, MemMB: *mem, Arch: *arch, CPUs: *cpus, TapName: *tap, NetworkMode: normalizeNetworkMode(*netMode), X86Boot: vmm.X86BootMode(*x86Boot),
		DiskSizeMB: *disk, SnapshotDir: *snap,
		Env: splitComma(*envStr), Cmd: splitFields(*cmdStr),
		Entrypoint:      splitFields(*entrypointStr),
		WorkDir:         *workdir,
		BuildArgs:       buildArgs.Map(),
		RootfsPersistent: *rootfsPersistent,
		PID1Mode:        pid1ModeForCLIWait(*wait),
		CacheDir:        *cacheDir,
		ExecEnabled:     interactive.enabled,
		InteractiveExec: interactive.enabled,
		JailerMode:      *jailerMode,
		ConsoleOut:      consoleOut,
		ConsoleIn:       consoleIn,
	}
	if *balloonTargetMiB > 0 || *balloonDeflateOnOOM || *balloonStatsIntervalS > 0 || strings.TrimSpace(*balloonAuto) != "" {
		runOpts.Balloon = &vmm.BalloonConfig{
			AmountMiB:             *balloonTargetMiB,
			DeflateOnOOM:          *balloonDeflateOnOOM,
			StatsPollingIntervalS: *balloonStatsIntervalS,
			Auto:                  mustBalloonAutoMode(*balloonAuto),
		}
	}
	if *hotplugTotalMiB > 0 || *hotplugSlotMiB > 0 || *hotplugBlockMiB > 0 {
		runOpts.MemoryHotplug = &vmm.MemoryHotplugConfig{
			TotalSizeMiB: *hotplugTotalMiB,
			SlotSizeMiB:  *hotplugSlotMiB,
			BlockSizeMiB: *hotplugBlockMiB,
		}
	}
	result := mustRun(runOpts)
	defer result.Close()
	printResult(result)
	if interactive.enabled {
		if err := runLocalInteractiveVM(result, resolveInteractiveRunCommand(result.Config, runOpts)); err != nil {
			stopVMAndWait(result.VM, 15*time.Second)
			fatal(err.Error())
		}
		stopVMAndWait(result.VM, 15*time.Second)
		// Final newline to ensure the shell prompt redraws after VM stop messages.
		fmt.Println()
		return
	}
	if *wait {
		waitVM(result.VM, nil)
	}
}

// ---- compose ----

func cmdCompose(args []string) {
	if len(args) > 0 {
		switch args[0] {
		case "down":
			cmdComposeDown(args[1:])
			return
		case "exec":
			cmdComposeExec(args[1:])
			return
		}
	}
	fs := flag.NewFlagSet("compose", flag.ExitOnError)
	file := fs.String("file", "docker-compose.yml", "Path to docker-compose.yml")
	serverURL := fs.String("server", "", "Optional gocracker API server URL for compose-managed VMs")
	kernel := fs.String("kernel", "", "Kernel image path [required]")
	cacheDir := fs.String("cache-dir", filepath.Join(os.TempDir(), "gocracker", "cache"), "Persistent cache directory")
	mem := fs.Uint64("mem", 256, "Default RAM per service (MiB)")
	arch := fs.String("arch", runtime.GOARCH, "Guest architecture for every service: amd64 or arm64 (same-arch only)")
	disk := fs.Int("disk", 4096, "Default disk size per service (MiB)")
	x86Boot := fs.String("x86-boot", string(vmm.X86BootAuto), "x86 boot mode: auto, acpi, or legacy")
	tapPfx := fs.String("tap-prefix", "gc", "TAP interface prefix")
	snapDir := fs.String("snapshot", "", "Snapshot dir to restore from / save to")
	wait := fs.Bool("wait", false, "Block until all VMs stop")
	doSnap := fs.Bool("save-snapshot", false, "Take snapshots on Ctrl-C / stop")
	jailerMode := fs.String("jailer", container.JailerModeOn, "Privilege model: on or off")
	rootfsPersistent := fs.Bool("rootfs-persistent", false, "Mount rootfs rw in each service VM (writes survive; slower boot).")
	fs.Parse(args)

	requireKernel(*kernel)
	*kernel = resolveRequiredExistingPath("kernel", *kernel)

	stack, err := compose.Up(compose.RunOptions{
		ComposePath:      *file,
		ServerURL:        *serverURL,
		CacheDir:         *cacheDir,
		KernelPath:       *kernel,
		DefaultMem:       *mem,
		Arch:             *arch,
		DefaultDisk:      *disk,
		TapPrefix:        *tapPfx,
		SnapshotDir:      *snapDir,
		X86Boot:          vmm.X86BootMode(*x86Boot),
		JailerMode:       *jailerMode,
		RootfsPersistent: *rootfsPersistent,
	})
	if err != nil {
		fatal("compose up: " + err.Error())
	}

	fmt.Println("\n✓ Stack started:")
	fmt.Printf("  %-20s %-10s %-15s %-10s %s\n", "SERVICE", "STATE", "IP", "TAP", "PORTS")
	for _, info := range stack.ServiceInfos() {
		ports := "-"
		if len(info.PublishedPorts) > 0 {
			ports = strings.Join(info.PublishedPorts, ", ")
		}
		fmt.Printf("  %-20s %-10s %-15s %-10s %s\n", info.Name, info.State, valueOrDash(info.IP), valueOrDash(info.TapName), ports)
	}
	if hints := composeAccessHints(stack.ServiceInfos()); len(hints) > 0 {
		fmt.Println("\nAccess hints:")
		for _, hint := range hints {
			fmt.Printf("  - %s\n", hint)
		}
	}

	if *wait || *doSnap {
		fmt.Println("\n(Ctrl-C to stop stack)")
		waitForInterrupt()
		if *doSnap && *snapDir != "" {
			fmt.Printf("\nTaking snapshots → %s\n", *snapDir)
			if err := stack.TakeSnapshots(*snapDir); err != nil {
				fmt.Printf("snapshot error: %v\n", err)
			}
		}
		stack.Down()
	}
}

func composeAccessHints(services []compose.ServiceInfo) []string {
	return []string{"Use the published host ports above to reach each service from the host"}
}

func valueOrDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}

func cmdComposeDown(args []string) {
	fs := flag.NewFlagSet("compose down", flag.ExitOnError)
	file := fs.String("file", "docker-compose.yml", "Path to docker-compose.yml")
	serverURL := fs.String("server", "", "gocracker API server URL")
	fs.Parse(args)
	if *serverURL == "" {
		fatal("compose down currently requires --server")
	}
	if err := compose.DownRemote(*serverURL, *file); err != nil {
		fatal("compose down: " + err.Error())
	}
	fmt.Println("✓ Stack stopped")
}

func cmdComposeExec(args []string) {
	fs := flag.NewFlagSet("compose exec", flag.ExitOnError)
	file := fs.String("file", "docker-compose.yml", "Path to docker-compose.yml")
	serverURL := fs.String("server", "http://127.0.0.1:8080", "gocracker serve API URL")
	fs.Parse(args)
	if fs.NArg() < 1 {
		fatal("compose exec requires a service name")
	}
	parsedArgs := fs.Args()
	service := parsedArgs[0]
	info, err := compose.LookupRemoteService(*serverURL, *file, service)
	if err != nil {
		fatal(err.Error())
	}
	client := internalapi.NewClient(*serverURL)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	execArgs := parsedArgs[1:]
	if len(execArgs) > 0 && execArgs[0] == "--" {
		execArgs = execArgs[1:]
	}
	if len(execArgs) == 0 {
		cols, rows := interactiveTerminalSize()
		conn, err := client.ExecVMStream(ctx, info.ID, internalapi.ExecRequest{
			Columns: cols,
			Rows:    rows,
		})
		if err != nil {
			fatal(err.Error())
		}
		if err := runInteractiveConn(conn); err != nil {
			fatal(err.Error())
		}
		return
	}
	resp, err := client.ExecVM(ctx, info.ID, internalapi.ExecRequest{Command: execArgs})
	if err != nil {
		fatal(err.Error())
	}
	if resp.Stdout != "" {
		fmt.Fprint(os.Stdout, resp.Stdout)
	}
	if resp.Stderr != "" {
		fmt.Fprint(os.Stderr, resp.Stderr)
	}
	if resp.ExitCode != 0 {
		os.Exit(resp.ExitCode)
	}
}

func runInteractiveConn(conn net.Conn) error {
	return runInteractiveConnWithIO(conn, terminalInputReader, terminalOutput)
}

func runInteractiveConnWithIO(conn net.Conn, stdin *os.File, stdout io.Writer) error {
	defer conn.Close()
	restore := prepareInteractiveTerminal(stdin, stdout)
	defer restore()
	copyInputDone := make(chan error, 1)
	go func() {
		buf := ioCopyBufPool.Get().(*[]byte)
		defer ioCopyBufPool.Put(buf)
		_, err := io.CopyBuffer(conn, stdin, *buf)
		closeNetWriter(conn)
		copyInputDone <- err
	}()
	copyOutputDone := make(chan error, 1)
	go func() {
		buf := ioCopyBufPool.Get().(*[]byte)
		defer ioCopyBufPool.Put(buf)
		_, err := io.CopyBuffer(stdout, conn, *buf)
		copyOutputDone <- err
	}()

	select {
	case inputErr := <-copyInputDone:
		// Input copy finished first. If it returned a real error (e.g.
		// write to a closed vsock pipe), the remote has already gone away
		// — force-close the conn so the output copy's Read() unblocks
		// immediately. If input ended cleanly (EOF from stdin), the
		// remote may still have data in flight, so wait for the output
		// copy to drain naturally.
		if normalizeCopyError(inputErr) != nil {
			conn.Close()
		}
		return normalizeCopyError(<-copyOutputDone)
	case outputErr := <-copyOutputDone:
		if stdin != nil && stdin == os.Stdin {
			restore() // Restore state before returning
		}
		return normalizeCopyError(outputErr)
	}
}

func prepareInteractiveTerminal(stdin *os.File, stdout io.Writer) func() {
	if stdin == nil {
		return func() {}
	}
	stdinFD, stdinTTY := terminalGetFdInfo(stdin)
	if !stdinTTY {
		return func() {}
	}
	oldState, err := terminalSetRaw(stdinFD)
	if err != nil {
		return func() {}
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			_ = terminalRestore(stdinFD, oldState)
			if stdout != nil {
				// Reset cursor visibility and print carriage return + newline
				// so the shell prompt reappears cleanly after raw mode.
				fmt.Fprint(stdout, "\033[?25h\r\n")
			}
		})
	}
}

// ---- build ----

func cmdBuild(args []string) {
	fs := flag.NewFlagSet("build", flag.ExitOnError)
	image := fs.String("image", "", "OCI image ref")
	df := fs.String("dockerfile", "", "Path to Dockerfile")
	ctx := fs.String("context", ".", "Build context dir")
	repoURL := fs.String("repo", "", "Git repo URL or local path")
	repoRef := fs.String("ref", "", "Branch/tag")
	repoSubdir := fs.String("subdir", "", "Subdir inside repo")
	output := fs.String("output", "", "Output ext4 image path [required]")
	disk := fs.Int("disk", 2048, "Disk size MiB")
	cacheDir := fs.String("cache-dir", filepath.Join(os.TempDir(), "gocracker", "cache"), "Persistent cache directory")
	jailerMode := fs.String("jailer", container.JailerModeOn, "Privilege model: on or off")
	buildArgs := multiKVFlag{}
	fs.Var(&buildArgs, "build-arg", "Build arg KEY=VALUE (repeatable)")
	fs.Parse(args)

	if *output == "" {
		fatal("--output required")
	}

	result, err := container.Build(container.BuildOptions{
		Image:      *image,
		Dockerfile: *df,
		Context:    *ctx,
		RepoURL:    *repoURL,
		RepoRef:    *repoRef,
		RepoSubdir: *repoSubdir,
		DiskSizeMB: *disk,
		BuildArgs:  buildArgs.Map(),
		OutputPath: *output,
		CacheDir:   *cacheDir,
		JailerMode: *jailerMode,
	})
	if err != nil {
		fatal(err.Error())
	}
	fmt.Printf("✓ Built disk image: %s\n", result.DiskPath)
	if result.RootfsDir != "" {
		fmt.Printf("  rootfs: %s\n", result.RootfsDir)
	}
}

// ---- restore ----

func cmdRestore(args []string) {
	fs := flag.NewFlagSet("restore", flag.ExitOnError)
	snapDir := fs.String("snapshot", "", "Snapshot directory [required]")
	wait := fs.Bool("wait", false, "Block until VM stops")
	ttyMode := fs.String("tty", "auto", "Console mode: auto, off, or force")
	cpus := fs.Int("cpus", 0, "Expected vCPU count from snapshot")
	x86Boot := fs.String("x86-boot", "", "Expected x86 boot mode from snapshot")
	jailerMode := fs.String("jailer", container.JailerModeOn, "Privilege model: on or off")
	fs.Parse(args)

	if *snapDir == "" {
		fatal("--snapshot required")
	}

	session := mustConsoleSession(*ttyMode, *wait)
	if session != nil {
		defer session.Close()
	}

	fmt.Printf("restoring from %s...\n", *snapDir)
	t0 := time.Now()
	var (
		vm  vmm.Handle
		err error
	)
	if *jailerMode == container.JailerModeOff {
		localVM, restoreErr := vmm.RestoreFromSnapshotWithOptions(*snapDir, vmm.RestoreOptions{
			ConsoleIn:       session.ConsoleIn(),
			ConsoleOut:      session.ConsoleOut(),
			OverrideVCPUs:   *cpus,
			OverrideX86Boot: vmm.X86BootMode(*x86Boot),
		})
		if restoreErr != nil {
			fatal(restoreErr.Error())
		}
		if err := localVM.Start(); err != nil {
			fatal(err.Error())
		}
		vm = localVM
	} else {
		vm, _, err = worker.LaunchRestoredVMM(*snapDir, vmm.RestoreOptions{
			ConsoleIn:       session.ConsoleIn(),
			ConsoleOut:      session.ConsoleOut(),
			OverrideVCPUs:   *cpus,
			OverrideX86Boot: vmm.X86BootMode(*x86Boot),
		}, worker.VMMOptions{UID: os.Getuid(), GID: os.Getgid()})
		if err != nil {
			fatal(err.Error())
		}
	}
	fmt.Printf("✓ VM %s restored in %s\n", vm.ID(), time.Since(t0).Round(time.Millisecond))
	if *wait {
		waitVM(vm, session)
	}
}

// ---- migrate ----

func cmdMigrate(args []string) {
	fs := flag.NewFlagSet("migrate", flag.ExitOnError)
	source := fs.String("source", "", "Source API base URL [required]")
	id := fs.String("id", "", "VM identifier on source [required]")
	dest := fs.String("dest", "", "Destination API base URL [required]")
	targetID := fs.String("target-id", "", "Override VM identifier on destination")
	targetTap := fs.String("tap", "", "Override TAP interface on destination")
	noResume := fs.Bool("no-resume", false, "Leave the VM paused on the destination")
	fs.Parse(args)

	if *source == "" {
		fatal("--source required")
	}
	if *id == "" {
		fatal("--id required")
	}
	if *dest == "" {
		fatal("--dest required")
	}

	body := fmt.Sprintf(`{"destination_url":%q,"target_vm_id":%q,"target_tap_name":%q,"resume_target":%t}`,
		*dest, *targetID, *targetTap, !*noResume)
	endpoint := strings.TrimRight(*source, "/") + "/vms/" + *id + "/migrate"
	resp, err := apiPOST(endpoint, body)
	if err != nil {
		fatal(err.Error())
	}
	fmt.Println(resp)
}

// ---- serve ----

func cmdServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", "", "TCP address (e.g. :8080)")
	sock := fs.String("sock", "/tmp/gocracker.sock", "Unix socket path")
	authToken := fs.String("auth-token", os.Getenv("GOCRACKER_API_TOKEN"), "Bearer token required for API requests when set")
	x86Boot := fs.String("x86-boot", string(vmm.X86BootAuto), "Default x86 boot mode: auto, acpi, or legacy")
	jailerMode := fs.String("jailer", container.JailerModeOn, "Privilege model for /run, root preboot, and restore/migration target: on or off")
	jailerBinary := fs.String("jailer-binary", "gocracker-jailer", "Path to the standalone gocracker-jailer binary")
	vmmBinary := fs.String("vmm-binary", "gocracker-vmm", "Path to the standalone gocracker-vmm binary")
	stateDir := fs.String("state-dir", "/tmp/gocracker-serve-state", "Persistent supervisor state directory")
	cacheDir := fs.String("cache-dir", filepath.Join(os.TempDir(), "gocracker", "cache"), "Persistent cache directory")
	chrootBaseDir := fs.String("chroot-base-dir", worker.DefaultChrootBaseDir(), "Base directory for jail roots")
	uid := fs.Int("uid", os.Getuid(), "UID for jailed workers")
	gid := fs.Int("gid", os.Getgid(), "GID for jailed workers")
	trustedKernelDirs := multiStringFlag{}
	trustedWorkDirs := multiStringFlag{}
	trustedSnapshotDirs := multiStringFlag{}
	fs.Var(&trustedKernelDirs, "trusted-kernel-dir", "Trusted kernel directory for API-supplied kernel paths (repeatable)")
	fs.Var(&trustedWorkDirs, "trusted-work-dir", "Trusted workspace directory for API-supplied dockerfile/context/initrd paths (repeatable)")
	fs.Var(&trustedSnapshotDirs, "trusted-snapshot-dir", "Trusted snapshot directory for API snapshot/restore paths (repeatable)")
	pruneMaxAge := fs.Duration("prune-stale-temp-age", 48*time.Hour, "Age threshold for pruning /tmp/gocracker-* orphans left by crashed builds; 0 disables")
	fs.Parse(args)
	if *addr != "" && !isLoopbackTCPAddr(*addr) && strings.TrimSpace(*authToken) == "" {
		fatal("--auth-token is required when --addr is not an explicit loopback address; use 127.0.0.1:PORT for local unauthenticated access")
	}

	// Sweep stale /tmp/gocracker-* dirs BEFORE accepting any HTTP request.
	// Every temp-dir site has happy-path cleanup, but when the parent process
	// gets SIGKILL'd (sweep timeouts, OOM-kill, manual Ctrl-C) the deferred
	// cleanups never run and 10s–100s of MB per orphan pile up. One gocracker
	// serve restart a day is enough to keep /tmp bounded indefinitely.
	if *pruneMaxAge > 0 {
		result := tempprune.PruneStaleTempDirs(tempprune.DefaultPrefixes, *pruneMaxAge)
		if result.Removed > 0 {
			fmt.Fprintf(os.Stderr, "[serve] pruned %d/%d stale temp dirs (%.1f MiB freed, max_age=%s)\n",
				result.Removed, result.Scanned, float64(result.BytesFree)/(1024*1024), pruneMaxAge.String())
		}
		for _, err := range result.Errors {
			fmt.Fprintf(os.Stderr, "[serve] temp prune error: %v\n", err)
		}
	}

	// One-shot capability probe for network_mode=auto. If the server cannot
	// create TAP devices (no root, no CAP_NET_ADMIN), warn the operator so
	// they catch the misconfiguration at boot instead of when the first
	// /run request returns 403.
	if !hostguard.HasNetAdmin() {
		fmt.Fprintf(os.Stderr, "[serve] WARNING: network_mode=auto unavailable — process lacks root/CAP_NET_ADMIN; /run with network_mode=auto will return 403. Run as root or `setcap cap_net_admin+ep <binary>`.\n")
	} else if os.Getuid() != 0 {
		// Running as non-root with setcap cap_net_admin+ep. The effective
		// capability is available to THIS process, but forked children (e.g.
		// `ip addr add`, `ip link set`, `iptables`) will NOT inherit it unless
		// we raise the ambient capability set. Ambient bits propagate to every
		// exec'd child automatically, making delegate network setup work without
		// requiring those utilities to have their own file capabilities.
		// Silently ignore errors (ambient not supported on kernels < 4.3, or if
		// the capability is not in the inheritable set — both fall back to the
		// root path gracefully).
		raiseAmbientNetAdmin()
	}

	kernelDirs := trustedKernelDirs.Values()
	if len(kernelDirs) == 0 {
		kernelDirs = defaultTrustedKernelDirs()
	}
	workDirs := trustedWorkDirs.Values()
	if len(workDirs) == 0 {
		workDirs = defaultTrustedWorkDirs()
	}
	snapshotDirs := trustedSnapshotDirs.Values()
	if len(snapshotDirs) == 0 {
		snapshotDirs = defaultTrustedSnapshotDirs(*stateDir)
	}

	resolvedJailer := *jailerBinary
	resolvedVMM := *vmmBinary
	if *jailerMode != container.JailerModeOff {
		var err error
		resolvedJailer, err = resolveStandaloneBinary(*jailerBinary)
		if err != nil {
			fatal(err.Error())
		}
		resolvedVMM, err = resolveStandaloneBinary(*vmmBinary)
		if err != nil {
			fatal(err.Error())
		}
	}

	srv := api.NewWithOptions(api.Options{
		DefaultX86Boot:      vmm.X86BootMode(*x86Boot),
		JailerMode:          *jailerMode,
		JailerBinary:        resolvedJailer,
		VMMBinary:           resolvedVMM,
		StateDir:            *stateDir,
		CacheDir:            *cacheDir,
		ChrootBaseDir:       *chrootBaseDir,
		UID:                 *uid,
		GID:                 *gid,
		AuthToken:           strings.TrimSpace(*authToken),
		TrustedKernelDirs:   kernelDirs,
		TrustedWorkDirs:     workDirs,
		TrustedSnapshotDirs: snapshotDirs,
	})
	if *addr != "" {
		if err := srv.ListenAndServe(*addr); err != nil {
			fatal(err.Error())
		}
	} else {
		os.Remove(*sock)
		if err := srv.ListenUnix(*sock); err != nil {
			fatal(err.Error())
		}
	}
}

func resolveStandaloneBinary(value string) (string, error) {
	if value == "" {
		return "", fmt.Errorf("binary path is required")
	}
	if filepath.IsAbs(value) {
		if _, err := os.Stat(value); err != nil {
			return "", fmt.Errorf("stat %s: %w", value, err)
		}
		return value, nil
	}
	path, err := exec.LookPath(value)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", value, err)
	}
	return path, nil
}

// ---- vmm ----

func cmdVMM(args []string) {
	fs := flag.NewFlagSet("vmm", flag.ExitOnError)
	socketPath := fs.String("socket", "/tmp/gocracker-vmm.sock", "Unix socket path to listen on")
	defaultBoot := fs.String("default-x86-boot", string(vmm.X86BootAuto), "default x86 boot mode: auto, acpi, legacy")
	vmID := fs.String("vm-id", "", "VM identifier used for worker-backed launches")
	fs.Parse(args)

	srv := vmmserver.NewWithOptions(vmmserver.Options{
		DefaultX86Boot: vmm.X86BootMode(*defaultBoot),
		VMID:           *vmID,
	})

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		srv.Close()
		os.Exit(0)
	}()

	if err := srv.ListenUnix(*socketPath); err != nil {
		fatal(err.Error())
	}
}

// ---- build-worker ----

func cmdBuildWorker(args []string) {
	fs := flag.NewFlagSet("build-worker", flag.ExitOnError)
	socketPath := fs.String("socket", "/tmp/gocracker-build.sock", "Unix socket path to listen on")
	fs.Parse(args)

	srv := buildserver.New()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		os.Exit(0)
	}()

	if err := srv.ListenUnix(*socketPath); err != nil {
		fatal(err.Error())
	}
}

// ---- jailer ----

func cmdJailer(args []string) {
	if err := jailer.RunCLI(args); err != nil {
		fatal(err.Error())
	}
}

// ---- helpers ----

func mustRun(opts container.RunOptions) *container.RunResult {
	t0 := time.Now()
	result, err := container.Run(opts)
	if err != nil {
		fatal(err.Error())
	}
	_ = t0
	return result
}

func mustConsoleSession(modeRaw string, wait bool) *console.Session {
	mode, err := console.ParseMode(modeRaw)
	if err != nil {
		fatal(err.Error())
	}
	session, err := console.NewSession(mode, wait, os.Stdin, os.Stdout)
	if err != nil {
		fatal(err.Error())
	}
	if err := session.Start(); err != nil {
		fatal(err.Error())
	}
	return session
}

func mustInteractiveMode(modeRaw string, wait bool) interactiveMode {
	mode, err := console.ParseMode(modeRaw)
	if err != nil {
		fatal(err.Error())
	}
	if !wait {
		if mode == console.ModeForce {
			fatal("--tty=force requires --wait")
		}
		return interactiveMode{}
	}
	if mode == console.ModeOff {
		return interactiveMode{}
	}
	_, stdinTTY := mobyterm.GetFdInfo(os.Stdin)
	_, stdoutTTY := mobyterm.GetFdInfo(os.Stdout)
	if stdinTTY && stdoutTTY {
		return interactiveMode{enabled: true}
	}
	if mode == console.ModeForce {
		fatal("--tty=force requires a real terminal on stdin/stdout")
	}
	return interactiveMode{}
}

func mustBalloonAutoMode(raw string) vmm.BalloonAutoMode {
	mode := vmm.BalloonAutoMode(strings.TrimSpace(raw))
	switch mode {
	case "", vmm.BalloonAutoOff:
		return vmm.BalloonAutoOff
	case vmm.BalloonAutoConservative:
		return mode
	default:
		fatal("invalid --balloon-auto: " + raw + " (want off or conservative)")
		return vmm.BalloonAutoOff
	}
}

func printResult(r *container.RunResult) {
	state := r.VM.State()
	// Worker-backed VMs report "created" before the first state poll;
	// the VM is actually running inside the worker at this point.
	if state == vmm.StateCreated && r.WorkerSocket != "" {
		state = vmm.StateRunning
	}
	bootTime := r.Timings.Total.Round(time.Millisecond)
	if bootTime == 0 {
		bootTime = r.Duration
	}
	if bootTime > 0 {
		fmt.Printf("\n✓ VM %s is %s (%s)\n", r.ID, state, bootTime)
	} else {
		fmt.Printf("\n✓ VM %s is %s\n", r.ID, state)
	}
	// Per-phase boot time breakdown. This replaces the old single
	// "duration=Xms" number that compared apples to oranges between the
	// runLocal and runViaWorker paths. See the "Boot-time benchmark"
	// section of the README for the methodology.
	t := r.Timings
	if t.Total > 0 || t.VMMSetup > 0 || t.Orchestration > 0 {
		fmt.Printf("  boot:   orchestration=%dms  vmm_setup=%dms  start=%dms  guest_first_output=%dms  total=%dms\n",
			t.Orchestration.Milliseconds(),
			t.VMMSetup.Milliseconds(),
			t.Start.Milliseconds(),
			t.GuestFirstOutput.Milliseconds(),
			t.Total.Milliseconds())
	}
	if r.DiskPath != "" {
		fmt.Printf("  disk:   %s\n", r.DiskPath)
	}
	if r.TapName != "" {
		fmt.Printf("  tap:    %s\n", r.TapName)
	}
	if r.GuestIP != "" {
		fmt.Printf("  guest:  %s\n", r.GuestIP)
	}
	if r.Gateway != "" {
		fmt.Printf("  gw:     %s\n", r.Gateway)
	}
}

func waitVM(vm vmm.Handle, session *console.Session) {
	fmt.Println("  (waiting for VM — Ctrl-C to stop)")
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()

	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigs)

	var detached <-chan struct{}
	var stopRequested <-chan struct{}
	if session != nil {
		detached = session.Detached()
		stopRequested = session.StopRequested()
	}

	for {
		if st := vm.State(); st != vmm.StateRunning && st != vmm.StateCreated {
			fmt.Printf("  VM stopped after %s\n", vm.Uptime().Round(time.Second))
			return
		}
		select {
		case <-ticker.C:
		case <-sigs:
			fmt.Printf("  Stopping VM %s...\n", vm.ID())
			vm.Stop()
		case <-stopRequested:
			fmt.Printf("  Stopping VM %s...\n", vm.ID())
			vm.Stop()
		case <-detached:
		}
	}
}

func resolveInteractiveRunCommand(imgConfig oci.ImageConfig, opts container.RunOptions) []string {
	// Consult the EFFECTIVE command — image's Entrypoint/Cmd merged with any
	// CLI overrides — before deciding whether to fall back to the guest's
	// default interactive shell. The prior early-return that only checked
	// opts.Entrypoint/opts.Cmd silently discarded the image's CMD whenever
	// the user didn't pass --cmd, so `gocracker run --dockerfile=... --wait`
	// landed in an interactive shell instead of running the image workload
	// (e.g. Dockerfile `CMD printf 'ok' > /result.txt` never executed).
	entrypoint := effectiveCommandSlice(opts.Entrypoint, imgConfig.Entrypoint)
	cmd := effectiveCommandSlice(opts.Cmd, imgConfig.Cmd)
	if len(entrypoint) == 0 && len(cmd) == 0 {
		return nil
	}
	proc := runtimecfg.ResolveProcess(entrypoint, cmd)
	if proc.IsZero() {
		return nil
	}
	command := make([]string, 0, 1+len(proc.Args))
	command = append(command, proc.Exec)
	command = append(command, proc.Args...)
	return command
}

func effectiveCommandSlice(override, base []string) []string {
	if len(override) > 0 {
		return append([]string{}, override...)
	}
	return append([]string{}, base...)
}

func runLocalInteractiveVM(result *container.RunResult, command []string) error {
	if result == nil || result.VM == nil {
		return fmt.Errorf("interactive exec requires a running VM")
	}
	cols, rows := interactiveTerminalSize()
	conn, err := openLocalExecStream(result.VM, internalapi.ExecRequest{
		Command: command,
		Columns: cols,
		Rows:    rows,
	})
	if err != nil {
		msg := fmt.Sprintf("interactive exec attach failed: %v", err)
		if tail := formatConsoleTail(result.VM.ConsoleOutput(), 40); tail != "" {
			msg += "\nserial tail:\n" + tail
		}
		return fmt.Errorf("%s", msg)
	}
	return runInteractiveConn(conn)
}

func openLocalExecStream(vm vmm.Handle, req internalapi.ExecRequest) (net.Conn, error) {
	if vm == nil {
		return nil, fmt.Errorf("VM handle is required")
	}
	if cfg := vm.VMConfig(); cfg.Exec == nil || !cfg.Exec.Enabled {
		return nil, fmt.Errorf("exec is not enabled for this VM")
	}
	dialer, ok := vm.(vmm.VsockDialer)
	if !ok {
		return nil, fmt.Errorf("virtio-vsock is not configured")
	}
	conn, err := dialer.DialVsock(execVsockPort(vm.VMConfig()))
	if err != nil {
		return nil, err
	}
	if err := guestexec.Encode(conn, guestexec.Request{
		Mode:    guestexec.ModeStream,
		Command: append([]string{}, req.Command...),
		Columns: req.Columns,
		Rows:    req.Rows,
		Env:     append([]string(nil), req.Env...),
		WorkDir: req.WorkDir,
	}); err != nil {
		_ = conn.Close()
		return nil, err
	}
	var ack guestexec.Response
	if err := guestexec.Decode(conn, &ack); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if ack.Error != "" {
		_ = conn.Close()
		return nil, fmt.Errorf("%s", ack.Error)
	}
	return conn, nil
}


// imageDefaultCmd returns the effective command from an OCI image config
// (entrypoint + cmd), or nil when the image has no default process.
func imageDefaultCmd(cfg oci.ImageConfig) []string {
	proc := runtimecfg.ResolveProcess(cfg.Entrypoint, cfg.Cmd)
	if proc.IsZero() {
		return nil
	}
	return append([]string{proc.Exec}, proc.Args...)
}

// runWarmCmd runs a one-shot exec command on a --warm VM (restored from
// snapshot) via the vsock exec agent. Streams stdout/stderr to the terminal
// and returns the guest exit code as an error when non-zero.
func runWarmCmd(vm vmm.Handle, cmd []string) error {
	if cfg := vm.VMConfig(); cfg.Exec == nil || !cfg.Exec.Enabled {
		return fmt.Errorf("exec agent not available on this VM (exec not enabled)")
	}
	dialer, ok := vm.(vmm.VsockDialer)
	if !ok {
		return fmt.Errorf("exec agent not available on this VM (no vsock)")
	}
	conn, err := dialer.DialVsock(execVsockPort(vm.VMConfig()))
	if err != nil {
		return fmt.Errorf("exec agent dial: %w", err)
	}
	defer conn.Close()
	if err := guestexec.Encode(conn, guestexec.Request{
		Mode:    guestexec.ModeExec,
		Command: cmd,
	}); err != nil {
		return fmt.Errorf("exec send: %w", err)
	}
	var resp guestexec.Response
	if err := guestexec.Decode(conn, &resp); err != nil {
		return fmt.Errorf("exec recv: %w", err)
	}
	if resp.Error != "" {
		return fmt.Errorf("%s", resp.Error)
	}
	if resp.Stdout != "" {
		fmt.Print(resp.Stdout)
	}
	if resp.Stderr != "" {
		fmt.Fprint(os.Stderr, resp.Stderr)
	}
	if resp.ExitCode != 0 {
		return fmt.Errorf("exit code %d", resp.ExitCode)
	}
	return nil
}

// drainWarmDone blocks until the background warmcache snapshot goroutine
// completes. No-op when WarmDone is nil (not a --warm run or cache hit path).
// MUST be called before any vm.Stop() to prevent the goroutine from touching
// freed VM memory.
func drainWarmDone(r *container.RunResult) {
	if r == nil || r.WarmDone == nil {
		return
	}
	<-r.WarmDone
}

func stopVMAndWait(vm vmm.Handle, timeout time.Duration) {
	if vm == nil {
		return
	}
	if vm.State() != vmm.StateStopped {
		fmt.Printf("  Stopping VM %s...\n", vm.ID())
		vm.Stop()
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_ = vm.WaitStopped(ctx)
	fmt.Printf("  VM stopped after %s\n", vm.Uptime().Round(time.Second))
}

func interactiveTerminalSize() (int, int) {
	cols, rows := 120, 40
	fd, isTerminal := mobyterm.GetFdInfo(os.Stdin)
	if !isTerminal {
		return cols, rows
	}
	if size, err := mobyterm.GetWinsize(fd); err == nil && size != nil {
		if size.Width > 0 {
			cols = int(size.Width)
		}
		if size.Height > 0 {
			rows = int(size.Height)
		}
	}
	return cols, rows
}

func execVsockPort(cfg vmm.Config) uint32 {
	if cfg.Exec != nil && cfg.Exec.Enabled && cfg.Exec.VsockPort != 0 {
		return cfg.Exec.VsockPort
	}
	return guestexec.DefaultVsockPort
}

func closeNetWriter(conn net.Conn) {
	type closeWriter interface {
		CloseWrite() error
	}
	if cw, ok := conn.(closeWriter); ok {
		_ = cw.CloseWrite()
	}
}

func normalizeCopyError(err error) error {
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) || errors.Is(err, net.ErrClosed) {
		return nil
	}
	if opErr, ok := err.(*net.OpError); ok && opErr.Err != nil {
		if errors.Is(opErr.Err, net.ErrClosed) {
			return nil
		}
	}
	return err
}

func formatConsoleTail(data []byte, maxLines int) string {
	if len(data) == 0 {
		return ""
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	filtered := lines[:0]
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		filtered = append(filtered, line)
	}
	if len(filtered) == 0 {
		return ""
	}
	if maxLines > 0 && len(filtered) > maxLines {
		filtered = filtered[len(filtered)-maxLines:]
	}
	return strings.Join(filtered, "\n")
}

func waitForInterrupt() {
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigs)
	<-sigs
}

func pid1ModeForCLIWait(wait bool) string {
	if wait {
		return runtimecfg.PID1ModeSupervised
	}
	return ""
}

func apiPOST(endpoint, body string) (string, error) {
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewBufferString(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return "", fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	return strings.TrimSpace(string(data)), nil
}

func requireKernel(k string) {
	if k == "" {
		fatal("--kernel is required (path to Linux bzImage or vmlinux)")
	}
}

func resolveRequiredExistingPath(label, raw string) string {
	if raw == "" {
		fatal("--" + label + " is required")
	}
	abs, err := filepath.Abs(raw)
	if err != nil {
		fatal("resolve --" + label + ": " + err.Error())
	}
	if _, err := os.Stat(abs); err != nil {
		fatal("read --" + label + " " + abs + ": " + err.Error())
	}
	return abs
}

// fatalFunc can be overridden in tests to avoid os.Exit.
var fatalFunc = func(msg string) {
	fmt.Fprintln(os.Stderr, "error:", msg)
	os.Exit(1)
}

func fatal(msg string) {
	fatalFunc(msg)
}

func splitComma(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

func splitFields(s string) []string {
	if s == "" {
		return nil
	}
	fields, err := runtimecfg.SplitCommandLine(s)
	if err != nil {
		fatal("parse --cmd: " + err.Error())
	}
	return fields
}

func normalizeNetworkMode(raw string) string {
	switch strings.TrimSpace(strings.ToLower(raw)) {
	case "", "none":
		return ""
	case container.NetworkModeAuto:
		return container.NetworkModeAuto
	default:
		fatal("invalid --net: " + raw + " (want none or auto)")
		return ""
	}
}

type multiStringFlag []string

func (f *multiStringFlag) String() string {
	if f == nil {
		return ""
	}
	return strings.Join(*f, ",")
}

func (f *multiStringFlag) Set(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("path cannot be empty")
	}
	*f = append(*f, value)
	return nil
}

func (f multiStringFlag) Values() []string {
	if len(f) == 0 {
		return nil
	}
	out := make([]string, 0, len(f))
	seen := map[string]struct{}{}
	for _, entry := range f {
		abs, err := filepath.Abs(strings.TrimSpace(entry))
		if err != nil {
			continue
		}
		abs = filepath.Clean(abs)
		if _, ok := seen[abs]; ok {
			continue
		}
		seen[abs] = struct{}{}
		out = append(out, abs)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

type multiKVFlag []string

func (f *multiKVFlag) String() string {
	if f == nil {
		return ""
	}
	return strings.Join(*f, ",")
}

func (f *multiKVFlag) Set(value string) error {
	if !strings.Contains(value, "=") {
		return fmt.Errorf("expected KEY=VALUE, got %q", value)
	}
	*f = append(*f, value)
	return nil
}

func (f multiKVFlag) Map() map[string]string {
	if len(f) == 0 {
		return nil
	}
	out := make(map[string]string, len(f))
	for _, entry := range f {
		parts := strings.SplitN(entry, "=", 2)
		out[parts[0]] = parts[1]
	}
	return out
}

func defaultTrustedKernelDirs() []string {
	cwd, err := os.Getwd()
	if err != nil {
		return nil
	}
	candidate := filepath.Join(cwd, "artifacts", "kernels")
	if info, err := os.Stat(candidate); err == nil && info.IsDir() {
		return []string{candidate}
	}
	return nil
}

func defaultTrustedWorkDirs() []string {
	cwd, err := os.Getwd()
	candidates := []string{}
	if err == nil {
		candidates = append(candidates, cwd)
	}
	tmp := os.TempDir()
	if strings.TrimSpace(tmp) != "" {
		candidates = append(candidates, tmp)
	}
	if len(candidates) == 0 {
		return nil
	}
	out := make([]string, 0, len(candidates))
	seen := map[string]struct{}{}
	for _, candidate := range candidates {
		abs, err := filepath.Abs(candidate)
		if err != nil {
			continue
		}
		abs = filepath.Clean(abs)
		if _, ok := seen[abs]; ok {
			continue
		}
		seen[abs] = struct{}{}
		out = append(out, abs)
	}
	return out
}

func defaultTrustedSnapshotDirs(stateDir string) []string {
	candidates := []string{
		filepath.Join(stateDir, "snapshots"),
		stateDir,
		"/tmp/gocracker-snapshots",
	}
	out := make([]string, 0, len(candidates))
	seen := map[string]struct{}{}
	for _, candidate := range candidates {
		if strings.TrimSpace(candidate) == "" {
			continue
		}
		abs, err := filepath.Abs(candidate)
		if err != nil {
			continue
		}
		if _, ok := seen[abs]; ok {
			continue
		}
		seen[abs] = struct{}{}
		out = append(out, abs)
	}
	return out
}

// raiseAmbientNetAdmin promotes CAP_NET_ADMIN to the ambient capability set so
// that every child process exec'd by this server (ip, iptables, etc.) inherits
// the capability automatically. This allows gocracker to run as a non-root user
// with `setcap cap_net_admin+ep` while still delegating network setup to those
// utilities.
//
// Steps:
//  1. Add CAP_NET_ADMIN to the inheritable set (required by Linux before a cap
//     can be raised to ambient).
//  2. Call prctl(PR_CAP_AMBIENT, PR_CAP_AMBIENT_RAISE, CAP_NET_ADMIN).
//
// Errors are silently ignored: kernels < 4.3 don't have ambient caps, and any
// failure means gocracker simply falls back to requiring root for sub-process
// network operations.
func raiseAmbientNetAdmin() {
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var data [2]unix.CapUserData
	if err := unix.Capget(&hdr, &data[0]); err != nil {
		return
	}
	// CAP_NET_ADMIN = 12; it fits in the low 32-bit word.
	data[0].Inheritable |= 1 << unix.CAP_NET_ADMIN
	if err := unix.Capset(&hdr, &data[0]); err != nil {
		return
	}
	_ = unix.Prctl(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_RAISE, unix.CAP_NET_ADMIN, 0, 0)
}

func isLoopbackTCPAddr(addr string) bool {
	host := addr
	if strings.HasPrefix(addr, ":") {
		host = ""
	} else if parsedHost, _, err := net.SplitHostPort(addr); err == nil {
		host = parsedHost
	}
	host = strings.Trim(strings.TrimSpace(host), "[]")
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
