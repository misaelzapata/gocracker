//go:build ignore

// Guest init — PID 1 inside the gocracker VM.
// Build: CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o init ./internal/guest/init.go
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/creack/pty"
	"github.com/gocracker/gocracker/internal/guestexec"
	"github.com/gocracker/gocracker/internal/runtimecfg"
	toolboxspec "github.com/gocracker/gocracker/internal/toolbox/spec"
	"github.com/gocracker/gocracker/internal/usercfg"
	"github.com/gocracker/gocracker/internal/vsock"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

var kmsg *os.File

// activePTY is the PTY master of the most recently started exec stream session.
// A ModeResize request applies TIOCSWINSZ to this fd so the foreground process
// group receives SIGWINCH and redraws correctly. Access is protected by
// activePTYMu. Only one active PTY is tracked; this is sufficient for the
// common case of a single interactive terminal session per sandbox.
var (
	activePTYMu sync.Mutex
	activePTY   *os.File
)

func initKmsg() {
	os.MkdirAll("/dev", 0755)
	syscall.Mknod("/dev/kmsg", syscall.S_IFCHR|0600, 1<<8|11)
	kmsg, _ = os.OpenFile("/dev/kmsg", os.O_WRONLY, 0)
}

func klogf(format string, args ...interface{}) {
	if kmsg == nil {
		return
	}
	fmt.Fprintf(kmsg, "<6>gocracker-init: "+format+"\n", args...)
}

func dupTo(oldfd, newfd int) error {
	if oldfd == newfd {
		return nil
	}
	return unix.Dup3(oldfd, newfd, 0)
}

func requireGuestInitContext() {
	if os.Getpid() == 1 {
		return
	}
	fmt.Fprintf(os.Stderr, "[init] refusing to run outside the guest init context: pid=%d (expected PID 1)\n", os.Getpid())
	os.Exit(2)
}

func main() {
	requireGuestInitContext()
	for _, dir := range []string{"/proc", "/sys", "/dev", "/dev/pts", "/tmp", "/run"} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			fmt.Fprintf(os.Stderr, "[init] mkdir %s: %v\n", dir, err)
		}
	}

	mountFS("proc", "/proc", "proc", 0, "")
	mountFS("sysfs", "/sys", "sysfs", 0, "")
	mountFS("devtmpfs", "/dev", "devtmpfs", syscall.MS_NOSUID, "mode=0755")
	mountFS("tmpfs", "/tmp", "tmpfs", 0, "size=128m")
	mountFS("tmpfs", "/run", "tmpfs", 0, "size=32m")
	syscall.Mount("devpts", "/dev/pts", "devpts", 0, "newinstance,gid=5,mode=620,ptmxmode=666")
	ensureGuestDevLinks()
	initKmsg()
	applyGuestSysctls()
	setupConsole()
	klogf("init start")
	syscall.Sethostname([]byte("gocracker"))
	loadKernelModules()

	cmdline := readCmdline()
	spec, err := resolveGuestSpec(cmdline)
	if err != nil {
		klogf("decode runtime config error: %v", err)
		fmt.Fprintf(os.Stderr, "[init] decode runtime config: %v\n", err)
	}

	rootfs := mountRootDisk(cmdline)
	if rootfs == "" {
		klogf("root disk handoff failed")
		persistExitCode(1)
		syscall.Sync()
		syscall.Reboot(syscall.LINUX_REBOOT_CMD_RESTART)
	}
	if err := switchRoot(rootfs); err != nil {
		klogf("switch_root to %q failed: %v", rootfs, err)
		fmt.Fprintf(os.Stderr, "[init] switch_root %s: %v\n", rootfs, err)
		persistExitCode(1)
		syscall.Sync()
		syscall.Reboot(syscall.LINUX_REBOOT_CMD_RESTART)
	}
	if refreshedSpec, refreshErr := resolveGuestSpec(cmdline); refreshErr != nil {
		klogf("runtime config refresh after switch_root failed: %v", refreshErr)
	} else if refreshedSpec.HasStructuredFields() || !spec.HasStructuredFields() {
		spec = refreshedSpec
		klogf("runtime config refreshed after switch_root")
	}
	ensureGuestPTYSupport()

	materializeRunDirsFromTmpfiles()
	klogf("process exec=%q args=%q workdir=%q user=%q", spec.Process.Exec, spec.Process.Args, spec.WorkDir, spec.User)
	mountSharedFilesystems(spec.SharedFS)
	klogf("shared filesystem mounts completed count=%d", len(spec.SharedFS))

	configureNetwork(cmdline)
	klogf("network configuration completed")
	ensureResolvConf()
	klogf("resolv.conf ensured")
	ensureHosts(spec.Hosts)
	klogf("hosts file ensured count=%d", len(spec.Hosts))
	_ = os.Remove(guestExitCodeFile)
	startExecAgent(spec)
	startToolboxSupervisor()

	// Set working directory if specified
	if spec.WorkDir != "" {
		if err := os.MkdirAll(spec.WorkDir, 0755); err != nil {
			fmt.Fprintf(os.Stderr, "[init] mkdir workdir %s: %v\n", spec.WorkDir, err)
		}
		if err := os.Chdir(spec.WorkDir); err != nil {
			fmt.Fprintf(os.Stderr, "[init] chdir workdir %s: %v\n", spec.WorkDir, err)
		}
	}

	proc := spec.Process
	if proc.Exec == "" {
		if spec.Exec.Enabled {
			klogf("exec enabled with no primary process; entering idle supervisor")
			runExecIdleSupervisor()
			return
		}
		proc.Exec = findShell()
	}
	klogf("starting process exec=%q args=%q", proc.Exec, proc.Args)
	if cmdline["gc.wait_network"] == "1" {
		time.Sleep(500 * time.Millisecond)
	}

	if effectivePID1Mode(spec) == runtimecfg.PID1ModeSupervised {
		code := runForeground(proc.Exec, proc.Args, buildEnv(spec.Env), spec.WorkDir, spec.User)
		persistExitCode(code)
		syscall.Sync()
		if err := syscall.Reboot(syscall.LINUX_REBOOT_CMD_RESTART); err != nil {
			klogf("supervised reboot failed after exit code %d: %v", code, err)
			fmt.Fprintf(os.Stderr, "[init] supervised reboot after exit %d: %v\n", code, err)
		}
		return
	}

	if err := execInPlace(proc.Exec, proc.Args, buildEnv(spec.Env), spec.WorkDir, spec.User); err != nil {
		klogf("exec handoff failed for %q: %v", proc.Exec, err)
		fmt.Fprintf(os.Stderr, "[init] exec handoff %s: %v\n", proc.Exec, err)
		persistExitCode(127)
		syscall.Sync()
		// Firecracker expects guests to request reboot and uses reboot=k to turn
		// that into a clean VM shutdown through the legacy reset path.
		syscall.Reboot(syscall.LINUX_REBOOT_CMD_RESTART)
	}
}

func effectivePID1Mode(spec runtimecfg.GuestSpec) string {
	if spec.Exec.Enabled {
		return runtimecfg.PID1ModeSupervised
	}
	if spec.PID1Mode == runtimecfg.PID1ModeSupervised {
		return runtimecfg.PID1ModeSupervised
	}
	return runtimecfg.PID1ModeHandoff
}

// startToolboxSupervisor spawns the baked toolbox agent
// (/opt/gocracker/toolbox/toolboxguest) in the background and restarts
// it up to maxToolboxRestarts times if it crashes. The supervisor lives
// for the lifetime of the VM — there is no graceful shutdown because
// init's exit means the kernel is rebooting anyway.
//
// If the binary is missing (older snapshot or image without the agent
// baked in), this is a no-op: the legacy exec path on vsock 10022
// still works, and post-Fase-2 features that need the new agent will
// fail dial with a clear error from the host UDS bridge instead of
// silently hanging.
//
// Past failure context (PLAN_SANDBOXD §1 row 1): feat/sandboxes-v2 ran
// the equivalent install via runtime.Exec + base64 upload AFTER boot,
// causing a ~200 ms race that triggered EnsureToolbox-on-lease, version
// stamps, and event-refill workarounds. Baking + spawning from PID 1
// before the user's CMD eliminates that whole class.
const maxToolboxRestarts = 3

func startToolboxSupervisor() {
	if _, err := os.Stat(toolboxspec.BinaryPath); err != nil {
		klogf("toolbox binary absent at %s; skipping supervisor (legacy exec on 10022 still active)", toolboxspec.BinaryPath)
		return
	}
	go func() {
		for attempt := 1; attempt <= maxToolboxRestarts; attempt++ {
			cmd := exec.Command(toolboxspec.BinaryPath, "serve",
				"--vsock-port", strconv.FormatUint(uint64(toolboxspec.VsockPort), 10))
			cmd.Stdout = kmsg
			cmd.Stderr = kmsg
			// Init's own env is whatever the kernel passed (typically
			// empty). The toolbox needs PATH so the processes it
			// spawns can resolve common binaries (echo, sh, sleep,
			// etc.) via exec.LookPath. Without this every /exec call
			// fails with "executable file not found in $PATH".
			cmd.Env = append(os.Environ(),
				"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
				"HOME=/root",
			)
			klogf("toolbox supervisor: starting attempt=%d", attempt)
			err := cmd.Run()
			klogf("toolbox supervisor: exited attempt=%d err=%v", attempt, err)
			if attempt < maxToolboxRestarts {
				time.Sleep(500 * time.Millisecond)
			}
		}
		klogf("toolbox supervisor: gave up after %d attempts; vsock 10023 will not respond", maxToolboxRestarts)
	}()
}

func startExecAgent(spec runtimecfg.GuestSpec) {
	if !spec.Exec.Enabled {
		return
	}
	go func() {
		if err := serveExecAgent(spec); err != nil {
			klogf("exec agent stopped: %v", err)
			fmt.Fprintf(os.Stderr, "[init] exec agent: %v\n", err)
		}
	}()
}

func serveExecAgent(spec runtimecfg.GuestSpec) error {
	port := spec.Exec.Port()
	// Listen for connections from the host. The host dials in per exec call;
	// we accept and dispatch each on its own goroutine. On snapshot/restore
	// the listener fd is preserved in the memory image — after restore the
	// host simply re-dials; no TRANSPORT_RESET dance needed.
	for {
		ln, err := listenVsock(port)
		if err != nil {
			klogf("[exec-agent] listen port=%d failed: %v — retrying", port, err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		klogf("[exec-agent] listening port=%d", port)
		for {
			conn, err := ln.Accept()
			if err != nil {
				klogf("[exec-agent] accept failed: %v — re-listening", err)
				ln.Close()
				break
			}
			klogf("[exec-agent] accepted connection")
			go handleExecAgentConn(conn, spec)
		}
	}
}

// listenVsock binds and listens on an AF_VSOCK port for incoming host connections.
func listenVsock(port uint32) (net.Listener, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
	if err != nil {
		return nil, fmt.Errorf("socket: %w", err)
	}
	sa := &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: port}
	if err := unix.Bind(fd, sa); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("bind port %d: %w", port, err)
	}
	if err := unix.Listen(fd, 8); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("listen port %d: %w", port, err)
	}
	return &vsockListener{fd: fd}, nil
}

type vsockListener struct{ fd int }

func (l *vsockListener) Accept() (net.Conn, error) {
	nfd, peerSA, err := unix.Accept(l.fd)
	if err != nil {
		return nil, err
	}
	vmSA, ok := peerSA.(*unix.SockaddrVM)
	if !ok {
		unix.Close(nfd)
		return nil, fmt.Errorf("unexpected peer address type from accept")
	}
	return newVsockConn(nfd, vmSA)
}

func (l *vsockListener) Close() error                { return unix.Close(l.fd) }
func (l *vsockListener) Addr() net.Addr              { return vsockAddr{cid: vsock.GuestCID, port: 0} }

type vsockConn struct {
	file  *os.File
	local net.Addr
	peer  net.Addr
}

func newVsockConn(fd int, peer unix.Sockaddr) (net.Conn, error) {
	file := os.NewFile(uintptr(fd), fmt.Sprintf("vsock-conn:%d", fd))
	local, err := unix.Getsockname(fd)
	if err != nil {
		file.Close()
		return nil, err
	}
	return &vsockConn{
		file:  file,
		local: sockaddrToAddr(local),
		peer:  sockaddrToAddr(peer),
	}, nil
}

func (c *vsockConn) Read(p []byte) (int, error)  { return c.file.Read(p) }
func (c *vsockConn) Write(p []byte) (int, error) { return c.file.Write(p) }
func (c *vsockConn) Close() error                { return c.file.Close() }

// Shutdown signals the vsock socket to stop accepting reads and writes,
// waking any goroutine blocked on Read/Write and triggering the kernel's
// VIRTIO_VSOCK_OP_SHUTDOWN to the host. This MUST be called before Close
// when another goroutine may still hold a reference to the FD (e.g. a
// blocked io.Copy), because Close only releases one FD reference and the
// kernel won't send the SHUTDOWN until ALL references are dropped.
func (c *vsockConn) Shutdown() error {
	return unix.Shutdown(int(c.file.Fd()), unix.SHUT_RDWR)
}
func (c *vsockConn) LocalAddr() net.Addr  { return c.local }
func (c *vsockConn) RemoteAddr() net.Addr { return c.peer }
func (c *vsockConn) SetDeadline(t time.Time) error {
	return c.file.SetDeadline(t)
}
func (c *vsockConn) SetReadDeadline(t time.Time) error {
	return c.file.SetReadDeadline(t)
}
func (c *vsockConn) SetWriteDeadline(t time.Time) error {
	return c.file.SetWriteDeadline(t)
}

type vsockAddr struct {
	cid  uint32
	port uint32
}

func (a vsockAddr) Network() string { return "vsock" }
func (a vsockAddr) String() string  { return fmt.Sprintf("%d:%d", a.cid, a.port) }

func sockaddrToAddr(sa unix.Sockaddr) net.Addr {
	vm, ok := sa.(*unix.SockaddrVM)
	if !ok {
		return vsockAddr{}
	}
	return vsockAddr{cid: vm.CID, port: vm.Port}
}

func handleExecAgentConn(conn net.Conn, spec runtimecfg.GuestSpec) {
	defer conn.Close()
	var req guestexec.Request
	if err := guestexec.Decode(conn, &req); err != nil {
		klogf("exec agent decode failed: %v", err)
		return
	}
	if err := req.Validate(); err != nil {
		_ = guestexec.Encode(conn, guestexec.Response{Error: err.Error()})
		return
	}

	switch req.Mode {
	case guestexec.ModeExec:
		resp := runExecRequest(req, spec)
		if err := guestexec.Encode(conn, resp); err != nil {
			klogf("exec agent reply failed: %v", err)
		}
	case guestexec.ModeStream:
		if err := runExecStream(conn, req, spec); err != nil {
			klogf("exec agent stream failed: %v", err)
		}
	case guestexec.ModeMemoryStats:
		stats, err := readGuestMemoryStats()
		if err != nil {
			_ = guestexec.Encode(conn, guestexec.Response{Error: err.Error()})
			return
		}
		if err := guestexec.Encode(conn, guestexec.Response{OK: true, MemoryStats: &stats}); err != nil {
			klogf("exec agent memory stats reply failed: %v", err)
		}
	case guestexec.ModeMemoryHotplugGet:
		hotplug, err := readGuestMemoryHotplug(req)
		if err != nil {
			_ = guestexec.Encode(conn, guestexec.Response{Error: err.Error()})
			return
		}
		if err := guestexec.Encode(conn, guestexec.Response{OK: true, MemoryHotplug: &hotplug}); err != nil {
			klogf("exec agent memory hotplug get reply failed: %v", err)
		}
	case guestexec.ModeMemoryHotplugUpdate:
		hotplug, err := applyGuestMemoryHotplug(req)
		if err != nil {
			_ = guestexec.Encode(conn, guestexec.Response{Error: err.Error()})
			return
		}
		if err := guestexec.Encode(conn, guestexec.Response{OK: true, MemoryHotplug: &hotplug}); err != nil {
			klogf("exec agent memory hotplug update reply failed: %v", err)
		}
	case guestexec.ModeResize:
		activePTYMu.Lock()
		ptmx := activePTY
		activePTYMu.Unlock()
		if ptmx == nil {
			_ = guestexec.Encode(conn, guestexec.Response{Error: "no active PTY session"})
			return
		}
		ws := &unix.Winsize{Col: uint16(req.Columns), Row: uint16(req.Rows)}
		if err := unix.IoctlSetWinsize(int(ptmx.Fd()), unix.TIOCSWINSZ, ws); err != nil {
			_ = guestexec.Encode(conn, guestexec.Response{Error: fmt.Sprintf("TIOCSWINSZ: %v", err)})
			return
		}
		_ = guestexec.Encode(conn, guestexec.Response{OK: true})
	default:
		_ = guestexec.Encode(conn, guestexec.Response{Error: fmt.Sprintf("unsupported exec mode %q", req.Mode)})
	}
}

func readGuestMemoryStats() (guestexec.MemoryStats, error) {
	meminfo, err := parseProcKeyValue("/proc/meminfo")
	if err != nil {
		return guestexec.MemoryStats{}, err
	}
	vmstat, err := parseProcKeyValue("/proc/vmstat")
	if err != nil {
		return guestexec.MemoryStats{}, err
	}

	pgfault := vmstat["pgfault"]
	pgmajfault := vmstat["pgmajfault"]
	minorFaults := pgfault
	if pgfault >= pgmajfault {
		minorFaults = pgfault - pgmajfault
	}

	return guestexec.MemoryStats{
		SwapIn:          vmstat["pswpin"],
		SwapOut:         vmstat["pswpout"],
		MajorFaults:     pgmajfault,
		MinorFaults:     minorFaults,
		FreeMemory:      meminfo["MemFree"],
		TotalMemory:     meminfo["MemTotal"],
		AvailableMemory: meminfo["MemAvailable"],
		DiskCaches:      meminfo["Cached"] + meminfo["Buffers"] + meminfo["SReclaimable"],
		OOMKill:         vmstat["oom_kill"],
		AllocStall:      vmstat["allocstall"] + sumPrefixed(vmstat, "allocstall_"),
		AsyncScan:       sumPrefixed(vmstat, "pgscan_kswapd"),
		DirectScan:      sumPrefixed(vmstat, "pgscan_direct"),
		AsyncReclaim:    sumPrefixed(vmstat, "pgsteal_kswapd"),
		DirectReclaim:   sumPrefixed(vmstat, "pgsteal_direct"),
	}, nil
}

func parseProcKeyValue(path string) (map[string]uint64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := make(map[string]uint64)
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		line = strings.ReplaceAll(line, ":", " ")
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		// /proc/meminfo values are reported in KiB.
		if strings.HasPrefix(filepath.Base(path), "meminfo") {
			value <<= 10
		}
		out[fields[0]] = value
	}
	return out, nil
}

func sumPrefixed(values map[string]uint64, prefix string) uint64 {
	var total uint64
	for key, value := range values {
		if strings.HasPrefix(key, prefix) {
			total += value
		}
	}
	return total
}

const memorySysfsRoot = "/sys/devices/system/memory"

func readGuestMemoryHotplug(req guestexec.Request) (guestexec.MemoryHotplug, error) {
	return collectGuestMemoryHotplugStatus(req.MemoryHotplugBaseAddr, req.MemoryHotplugTotalBytes, req.MemoryHotplugBlockBytes, req.MemoryHotplugTargetBytes)
}

func applyGuestMemoryHotplug(req guestexec.Request) (guestexec.MemoryHotplug, error) {
	status, err := collectGuestMemoryHotplugStatus(req.MemoryHotplugBaseAddr, req.MemoryHotplugTotalBytes, req.MemoryHotplugBlockBytes, req.MemoryHotplugTargetBytes)
	if err != nil {
		return guestexec.MemoryHotplug{}, err
	}
	blockSize := status.BlockSizeBytes
	startBlock := req.MemoryHotplugBaseAddr / blockSize
	endBlock := (req.MemoryHotplugBaseAddr + req.MemoryHotplugTotalBytes) / blockSize
	targetEndBlock := (req.MemoryHotplugBaseAddr + req.MemoryHotplugTargetBytes) / blockSize

	for blockID := startBlock; blockID < targetEndBlock; blockID++ {
		addr := req.MemoryHotplugBaseAddr + (blockID-startBlock)*blockSize
		if err := ensureGuestMemoryBlockPresent(addr, blockID); err != nil {
			return guestexec.MemoryHotplug{}, err
		}
		if err := ensureGuestMemoryBlockOnline(blockID); err != nil {
			return guestexec.MemoryHotplug{}, err
		}
	}
	for blockID := endBlock; blockID > targetEndBlock; blockID-- {
		currentID := blockID - 1
		if err := ensureGuestMemoryBlockOffline(currentID); err != nil {
			return guestexec.MemoryHotplug{}, err
		}
	}

	return collectGuestMemoryHotplugStatus(req.MemoryHotplugBaseAddr, req.MemoryHotplugTotalBytes, req.MemoryHotplugBlockBytes, req.MemoryHotplugTargetBytes)
}

func collectGuestMemoryHotplugStatus(baseAddr, totalBytes, expectedBlockBytes, requestedBytes uint64) (guestexec.MemoryHotplug, error) {
	blockSize, err := readGuestMemoryBlockSizeBytes()
	if err != nil {
		return guestexec.MemoryHotplug{}, err
	}
	if expectedBlockBytes != 0 && expectedBlockBytes != blockSize {
		return guestexec.MemoryHotplug{}, fmt.Errorf("guest memory block size mismatch: guest=%d bytes requested=%d bytes", blockSize, expectedBlockBytes)
	}
	if baseAddr%blockSize != 0 {
		return guestexec.MemoryHotplug{}, fmt.Errorf("memory hotplug base %#x is not aligned to guest block size %d", baseAddr, blockSize)
	}
	if totalBytes%blockSize != 0 {
		return guestexec.MemoryHotplug{}, fmt.Errorf("memory hotplug total %d is not aligned to guest block size %d", totalBytes, blockSize)
	}
	if requestedBytes > totalBytes {
		return guestexec.MemoryHotplug{}, fmt.Errorf("memory hotplug target %d exceeds total %d", requestedBytes, totalBytes)
	}
	if requestedBytes%blockSize != 0 {
		return guestexec.MemoryHotplug{}, fmt.Errorf("memory hotplug target %d is not aligned to guest block size %d", requestedBytes, blockSize)
	}
	startBlock := baseAddr / blockSize
	endBlock := (baseAddr + totalBytes) / blockSize
	var onlineBlocks uint64
	var presentBlocks uint64
	for blockID := startBlock; blockID < endBlock; blockID++ {
		exists, state, err := readGuestMemoryBlockState(blockID)
		if err != nil {
			return guestexec.MemoryHotplug{}, err
		}
		if !exists {
			continue
		}
		presentBlocks++
		if state != "offline" {
			onlineBlocks++
		}
	}
	return guestexec.MemoryHotplug{
		BlockSizeBytes: blockSize,
		RequestedBytes: requestedBytes,
		PluggedBytes:   onlineBlocks * blockSize,
		OnlineBlocks:   onlineBlocks,
		PresentBlocks:  presentBlocks,
	}, nil
}

func readGuestMemoryBlockSizeBytes() (uint64, error) {
	data, err := os.ReadFile(filepath.Join(memorySysfsRoot, "block_size_bytes"))
	if err != nil {
		return 0, err
	}
	value := strings.TrimSpace(string(data))
	base := 0
	if value != "" && !strings.HasPrefix(value, "0x") && !strings.HasPrefix(value, "0X") {
		// The kernel commonly exposes memory block_size_bytes as hexadecimal without
		// a 0x prefix, e.g. "8000000" for 128 MiB.
		base = 16
	}
	n, err := strconv.ParseUint(value, base, 64)
	if err != nil {
		return 0, fmt.Errorf("parse memory block_size_bytes %q: %w", value, err)
	}
	if n == 0 {
		return 0, fmt.Errorf("guest memory block size is zero")
	}
	return n, nil
}

func guestMemoryBlockDir(blockID uint64) string {
	return filepath.Join(memorySysfsRoot, fmt.Sprintf("memory%d", blockID))
}

func readGuestMemoryBlockState(blockID uint64) (bool, string, error) {
	blockDir := guestMemoryBlockDir(blockID)
	info, err := os.Stat(blockDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, "", nil
		}
		return false, "", err
	}
	if !info.IsDir() {
		return false, "", fmt.Errorf("%s is not a directory", blockDir)
	}
	data, err := os.ReadFile(filepath.Join(blockDir, "state"))
	if err != nil {
		return true, "", err
	}
	return true, strings.TrimSpace(string(data)), nil
}

func ensureGuestMemoryBlockPresent(addr, blockID uint64) error {
	exists, _, err := readGuestMemoryBlockState(blockID)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	if err := os.WriteFile(filepath.Join(memorySysfsRoot, "probe"), []byte(fmt.Sprintf("%#x", addr)), 0); err != nil {
		return fmt.Errorf("probe memory block %#x: %w", addr, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		exists, _, err := readGuestMemoryBlockState(blockID)
		if err == nil && exists {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for memory block %d after probe", blockID)
}

func ensureGuestMemoryBlockOnline(blockID uint64) error {
	exists, state, err := readGuestMemoryBlockState(blockID)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("memory block %d does not exist", blockID)
	}
	if state == "online" {
		return nil
	}
	statePath := filepath.Join(guestMemoryBlockDir(blockID), "state")
	var lastErr error
	for _, value := range []string{"online_movable", "online"} {
		if err := os.WriteFile(statePath, []byte(value), 0); err != nil {
			lastErr = err
			continue
		}
		if err := waitGuestMemoryBlockState(blockID, "online", 15*time.Second); err == nil {
			return nil
		} else {
			lastErr = err
		}
	}
	return fmt.Errorf("online memory block %d: %w", blockID, lastErr)
}

func ensureGuestMemoryBlockOffline(blockID uint64) error {
	exists, state, err := readGuestMemoryBlockState(blockID)
	if err != nil {
		return err
	}
	if !exists || state == "offline" {
		return nil
	}
	statePath := filepath.Join(guestMemoryBlockDir(blockID), "state")
	if err := os.WriteFile(statePath, []byte("offline"), 0); err != nil {
		return fmt.Errorf("offline memory block %d: %w", blockID, err)
	}
	return waitGuestMemoryBlockState(blockID, "offline", 30*time.Second)
}

func waitGuestMemoryBlockState(blockID uint64, want string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		exists, state, err := readGuestMemoryBlockState(blockID)
		if err != nil {
			return err
		}
		if exists && state == want {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for memory block %d to become %s", blockID, want)
}

func runExecRequest(req guestexec.Request, spec runtimecfg.GuestSpec) guestexec.Response {
	cmd, err := prepareExecCommand(req.Command, spec)
	if err != nil {
		return guestexec.Response{Stderr: err.Error() + "\n", ExitCode: 127}
	}
	if len(req.Env) > 0 {
		cmd.Env = append(cmd.Env, req.Env...)
	}
	if req.WorkDir != "" {
		cmd.Dir = req.WorkDir
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if req.Stdin != "" {
		cmd.Stdin = strings.NewReader(req.Stdin)
	} else {
		cmd.Stdin = nil
	}
	if err := cmd.Run(); err != nil {
		exitCode := execExitCode(err)
		if stderr.Len() == 0 {
			stderr.WriteString(err.Error())
			stderr.WriteByte('\n')
		}
		return guestexec.Response{
			Stdout:   stdout.String(),
			Stderr:   stderr.String(),
			ExitCode: exitCode,
		}
	}
	return guestexec.Response{
		OK:       true,
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: 0,
	}
}

func runExecStream(conn net.Conn, req guestexec.Request, spec runtimecfg.GuestSpec) error {
	cmd, err := prepareExecCommand(req.Command, spec)
	if err != nil {
		_ = guestexec.Encode(conn, guestexec.Response{Error: err.Error()})
		return err
	}
	if len(req.Env) > 0 {
		cmd.Env = append(cmd.Env, req.Env...)
	}
	if req.WorkDir != "" {
		cmd.Dir = req.WorkDir
	}
	cols := req.Columns
	rows := req.Rows
	if cols <= 0 {
		cols = 120
	}
	if rows <= 0 {
		rows = 40
	}
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		cmd.Stdin = conn
		cmd.Stdout = conn
		cmd.Stderr = conn
		if err := guestexec.Encode(conn, guestexec.Response{OK: true}); err != nil {
			return err
		}
		return cmd.Run()
	}
	defer ptmx.Close()
	// Register this PTY master as the active one so ModeResize requests can
	// apply TIOCSWINSZ without needing a separate session-tracking mechanism.
	activePTYMu.Lock()
	activePTY = ptmx
	activePTYMu.Unlock()
	defer func() {
		activePTYMu.Lock()
		if activePTY == ptmx {
			activePTY = nil
		}
		activePTYMu.Unlock()
	}()
	// Configure the PTY slave for interactive TUI use (vim, htop, tmux).
	// pty.StartWithSize allocates the master+slave pair and starts the process
	// with the slave as its controlling terminal, but it doesn't configure
	// termios — the kernel defaults leave icanon and echo on, which breaks
	// vim's cursor-key handling and makes htop look garbled.
	//
	// We resolve the slave device path via /proc/self/fd/<masterN> (Linux only),
	// open it briefly to apply cbreak termios, then close it immediately.
	// The process already holds the slave open as its controlling terminal, so
	// closing our extra reference here doesn't affect it.
	if slaveName, serr := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", ptmx.Fd())); serr == nil {
		if slaveFd, ferr := unix.Open(slaveName, unix.O_RDWR|unix.O_NOCTTY, 0); ferr == nil {
			termios, terr := unix.IoctlGetTermios(slaveFd, unix.TCGETS)
			if terr == nil {
				// cbreak: read keys immediately, one at a time.
				termios.Lflag &^= unix.ICANON | unix.ECHO | unix.ECHOE | unix.ECHOK | unix.ECHONL
				// isig: Ctrl-C delivers SIGINT, Ctrl-Z delivers SIGTSTP.
				termios.Lflag |= unix.ISIG
				// No output post-processing so ANSI escape sequences pass through intact.
				termios.Oflag &^= unix.OPOST
				// Minimum 1 byte per read, no timer — standard cbreak.
				termios.Cc[unix.VMIN] = 1
				termios.Cc[unix.VTIME] = 0
				_ = unix.IoctlSetTermios(slaveFd, unix.TCSETS, termios)
			}
			_ = unix.Close(slaveFd)
		}
	}
	if err := guestexec.Encode(conn, guestexec.Response{OK: true}); err != nil {
		return err
	}
	stdoutDone := make(chan struct{}, 1)
	go func() {
		_, _ = io.Copy(ptmx, conn)
	}()
	go func() {
		_, _ = io.Copy(conn, ptmx)
		stdoutDone <- struct{}{}
	}()
	waitErr := cmd.Wait()
	_ = ptmx.Close()
	// Flush the guest rootfs before the host tears us down. In interactive
	// mode the init stays in runExecIdleSupervisor forever, so the outer
	// syscall.Sync() after the foreground process (see the supervised
	// branch of Main) never fires. Without an explicit sync here, anything
	// the user wrote during the session lives only in the page cache; the
	// subsequent vm.Stop() from the host hard-kills the VM and those pages
	// are lost. Cheap relative to the shell's own lifetime.
	syscall.Sync()
	// Shutdown the vsock connection to wake the blocked io.Copy(ptmx, conn)
	// goroutine and trigger the kernel's VIRTIO_VSOCK_OP_SHUTDOWN immediately.
	// Without this, conn.Close() (in the caller's defer) only decrements the
	// FD refcount while the blocked read goroutine still holds a reference,
	// so the kernel never sends the shutdown packet and the host-side reader
	// blocks indefinitely — causing the console to hang after "exit".
	type shutdowner interface{ Shutdown() error }
	if sc, ok := conn.(shutdowner); ok {
		_ = sc.Shutdown()
	}
	select {
	case <-stdoutDone:
	case <-time.After(2 * time.Second):
		klogf("exec stream stdout drain timed out after process exit")
	}
	if waitErr != nil {
		return waitErr
	}
	return nil
}

func prepareExecCommand(command []string, spec runtimecfg.GuestSpec) (*exec.Cmd, error) {
	env := buildEnv(spec.Env)
	path := findShell()
	args := []string{"-i"}
	if len(command) > 0 {
		path = command[0]
		args = append([]string{}, command[1:]...)
	}
	// Resolve spec.User the same way runForeground does so the exec'd
	// process actually honours the Dockerfile USER directive. Without this,
	// interactive `gocracker run` against an image that declares
	// `USER 1000:1000` still runs the command as root — the image's
	// metadata was read but silently discarded on the exec path.
	var credential *syscall.Credential
	defaultHome := "/root"
	if spec.User != "" {
		resolvedUser, err := usercfg.Resolve("/", spec.User)
		if err != nil {
			return nil, fmt.Errorf("resolve user %q: %w", spec.User, err)
		}
		credential = &syscall.Credential{
			Uid:    resolvedUser.UID,
			Gid:    resolvedUser.GID,
			Groups: resolvedUser.Groups,
		}
		if resolvedUser.Home != "" {
			defaultHome = resolvedUser.Home
		}
	}
	env = ensureEnvDefault(env, "HOME", defaultHome)
	resolvedPath, err := guestexec.ResolveExecutable(path, env)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(resolvedPath, args...)
	cmd.Env = env
	if spec.WorkDir != "" {
		cmd.Dir = spec.WorkDir
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Credential: credential}
	return cmd, nil
}

func execExitCode(err error) int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
			if status.Signaled() {
				return 128 + int(status.Signal())
			}
			return status.ExitStatus()
		}
	}
	return 1
}

const guestExitCodeFile = "/.gocracker-exit-code"
const guestModuleManifest = "/etc/gocracker/modules.list"

func readCmdline() map[string]string {
	data, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		klogf("read /proc/cmdline failed: %v", err)
		return map[string]string{}
	}
	return runtimecfg.ParseKernelCmdline(string(data))
}

func resolveGuestSpec(cmdline map[string]string) (runtimecfg.GuestSpec, error) {
	if data, err := os.ReadFile(runtimecfg.GuestSpecPath); err == nil {
		klogf("loaded runtime config from %s (%d bytes)", runtimecfg.GuestSpecPath, len(data))
		return runtimecfg.UnmarshalGuestSpecJSON(data)
	} else if !errors.Is(err, os.ErrNotExist) {
		return runtimecfg.GuestSpec{}, fmt.Errorf("read %s: %w", runtimecfg.GuestSpecPath, err)
	} else {
		klogf("runtime config file %s not found; falling back to kernel cmdline", runtimecfg.GuestSpecPath)
	}

	spec, ok, err := runtimecfg.DecodeGuestSpec(cmdline)
	if err != nil {
		return runtimecfg.GuestSpec{}, err
	}
	if ok {
		return spec, nil
	}
	return runtimecfg.GuestSpec{
		Process: runtimecfg.LegacyProcess(cmdline),
		Env:     runtimecfg.LegacyEnv(cmdline),
		WorkDir: runtimecfg.LegacyWorkDir(cmdline),
		User:    runtimecfg.LegacyUser(cmdline),
	}, nil
}

func buildEnv(extra []string) []string {
	env := []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"TERM=xterm-256color",
	}
	env = append(env, extra...)
	return env
}

func loadKernelModules() {
	data, err := os.ReadFile(guestModuleManifest)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return
		}
		klogf("read module manifest failed: %v", err)
		fmt.Fprintf(os.Stderr, "[init] read module manifest: %v\n", err)
		return
	}

	for _, line := range strings.Split(string(data), "\n") {
		path := strings.TrimSpace(line)
		if path == "" || strings.HasPrefix(path, "#") {
			continue
		}
		if err := finitModule(path); err != nil {
			klogf("load module %q failed: %v", path, err)
			fmt.Fprintf(os.Stderr, "[init] load module %s: %v\n", path, err)
			continue
		}
		klogf("loaded module %q", path)
	}
}

func finitModule(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return unix.FinitModule(int(f.Fd()), "", 0)
}

func configureNetwork(c map[string]string) {
	if !networkRequested(c) {
		bringUpLoopback()
		klogf("network config skipped: no guest network requested")
		return
	}

	iface := c["gc.iface"]
	if iface == "" {
		iface = "eth0"
	}
	klogf("network config start iface=%q ip=%q gw=%q", iface, c["gc.ip"], c["gc.gw"])
	bringUpLoopback()

	link, err := waitForLink(iface, 30, 100*time.Millisecond)
	if err != nil {
		networkErrorf("wait for %s: %v", iface, err)
		return
	}
	if err := netlink.LinkSetUp(link); err != nil {
		networkErrorf("bring up %s: %v", iface, err)
		return
	}

	if ip := c["gc.ip"]; ip != "" {
		addr, err := netlink.ParseAddr(ip)
		if err != nil {
			networkErrorf("parse ip %s: %v", ip, err)
			return
		}
		if err := netlink.AddrReplace(link, addr); err != nil {
			networkErrorf("assign %s to %s: %v", ip, iface, err)
			return
		}
		if gw := c["gc.gw"]; gw != "" {
			route := &netlink.Route{
				LinkIndex: link.Attrs().Index,
				Gw:        net.ParseIP(gw),
			}
			if route.Gw == nil {
				networkErrorf("parse gateway %s", gw)
				return
			}
			if err := netlink.RouteReplace(route); err != nil {
				networkErrorf("set default route via %s: %v", gw, err)
				return
			}
		}
		klogf("configured static network iface=%q ip=%q gw=%q", iface, ip, c["gc.gw"])
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, dhcp := range []string{"dhclient", "udhcpc"} {
		if p, err := exec.LookPath(dhcp); err == nil {
			if err := exec.CommandContext(ctx, p, "-i", iface).Run(); err != nil {
				fmt.Fprintf(os.Stderr, "[init] DHCP %s: %v\n", dhcp, err)
			}
			break
		}
	}
	klogf("network config finished iface=%q", iface)
}

func networkRequested(c map[string]string) bool {
	if c["gc.wait_network"] == "1" {
		return true
	}
	if c["gc.ip"] != "" || c["gc.gw"] != "" {
		return true
	}
	return c["gc.iface"] != ""
}

func bringUpLoopback() {
	loopback, err := waitForLink("lo", 30, 100*time.Millisecond)
	if err == nil {
		if err := netlink.LinkSetUp(loopback); err != nil {
			networkErrorf("bring up loopback: %v", err)
		}
	} else {
		networkErrorf("wait for loopback: %v", err)
	}
}

func waitForLink(name string, attempts int, delay time.Duration) (netlink.Link, error) {
	var lastErr error
	for i := 0; i < attempts; i++ {
		link, err := netlink.LinkByName(name)
		if err == nil {
			return link, nil
		}
		lastErr = err
		time.Sleep(delay)
	}
	return nil, lastErr
}

func networkErrorf(format string, args ...interface{}) {
	klogf("network: "+format, args...)
	fmt.Fprintf(os.Stderr, "[init] network: "+format+"\n", args...)
}

func runForeground(path string, args, env []string, workdir, user string) int {
	resolvedPath, err := guestexec.ResolveExecutable(path, env)
	if err != nil {
		klogf("exec resolve failed for %q: %v", path, err)
		fmt.Fprintf(os.Stderr, "[init] exec %s: %v\n", path, err)
		return 1
	}

	var credential *syscall.Credential
	defaultHome := "/root"
	if user != "" {
		resolvedUser, err := usercfg.Resolve("/", user)
		if err != nil {
			klogf("user resolve failed for %q: %v", user, err)
			fmt.Fprintf(os.Stderr, "[init] user %s: %v\n", user, err)
			return 1
		}
		credential = &syscall.Credential{
			Uid:    resolvedUser.UID,
			Gid:    resolvedUser.GID,
			Groups: resolvedUser.Groups,
		}
		if resolvedUser.Home != "" {
			defaultHome = resolvedUser.Home
		}
	}
	env = ensureEnvDefault(env, "HOME", defaultHome)

	cmd := exec.Command(resolvedPath, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = env
	if workdir != "" {
		cmd.Dir = workdir
	}
	stdinFD := int(os.Stdin.Fd())
	// Force the guest console into canonical / cooked mode before exec'ing
	// the user shell. Otherwise the line discipline that arrives from
	// switch_root may have left the tty in a weird state where backspace,
	// Ctrl-C / SIGINT and line editing are inert. We mirror what the
	// kernel installs by default for /dev/ttyS0 + agetty: ICANON + ECHO +
	// ECHOE (so backspace erases on screen) + ISIG (so Ctrl-C → SIGINT) +
	// ICRNL/OPOST/ONLCR (CR/LF translation).
	if isTerminalFD(stdinFD) {
		if termios, err := unix.IoctlGetTermios(stdinFD, unix.TCGETS); err == nil {
			termios.Iflag |= unix.ICRNL | unix.IXON | unix.IUTF8
			termios.Iflag &^= unix.IGNCR | unix.INLCR
			termios.Oflag |= unix.OPOST | unix.ONLCR
			termios.Lflag |= unix.ICANON | unix.ECHO | unix.ECHOE | unix.ECHOK | unix.ECHOCTL | unix.ECHOKE | unix.ISIG | unix.IEXTEN
			termios.Cc[unix.VINTR] = 0x03  // Ctrl-C
			termios.Cc[unix.VERASE] = 0x7f // DEL (backspace key on most terminals)
			termios.Cc[unix.VEOF] = 0x04   // Ctrl-D
			termios.Cc[unix.VKILL] = 0x15  // Ctrl-U
			termios.Cc[unix.VSUSP] = 0x1a  // Ctrl-Z
			termios.Cc[unix.VMIN] = 1
			termios.Cc[unix.VTIME] = 0
			if err := unix.IoctlSetTermios(stdinFD, unix.TCSETS, termios); err != nil {
				klogf("tcsetattr cooked mode failed: %v", err)
			}
		} else {
			klogf("tcgetattr failed: %v", err)
		}
	}
	restorePGRP := 0
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:     true,
		Setctty:    true,
		Ctty:       0,
		Credential: credential,
	}
	if err := cmd.Start(); err != nil {
		klogf("exec start failed for %q (%q): %v", path, resolvedPath, err)
		fmt.Fprintf(os.Stderr, "[init] exec %s: %v\n", path, err)
		return 1
	}
	if isTerminalFD(stdinFD) {
		restorePGRP = unix.Getpgrp()
		if err := setForegroundProcessGroup(stdinFD, cmd.Process.Pid); err != nil {
			klogf("set foreground process group to %d failed: %v", cmd.Process.Pid, err)
		}
	}
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		for s := range sigs {
			cmd.Process.Signal(s)
		}
	}()
	cmd.Wait()
	signal.Stop(sigs)
	if restorePGRP > 0 {
		if err := setForegroundProcessGroup(stdinFD, restorePGRP); err != nil {
			klogf("restore foreground process group to %d failed: %v", restorePGRP, err)
		}
	}
	if ee, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok {
		if ee.Signaled() {
			return 128 + int(ee.Signal())
		}
		return ee.ExitStatus()
	}
	return 0
}

func isTerminalFD(fd int) bool {
	_, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	return err == nil
}

func setForegroundProcessGroup(fd, pgid int) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(unix.TIOCSPGRP), uintptr(unsafe.Pointer(&pgid)))
	if errno != 0 {
		return errno
	}
	return nil
}

func execInPlace(path string, args, env []string, workdir, user string) error {
	resolvedPath, err := guestexec.ResolveExecutable(path, env)
	if err != nil {
		return err
	}

	var resolvedUser usercfg.Resolved
	defaultHome := "/root"
	if user != "" {
		resolvedUser, err = usercfg.Resolve("/", user)
		if err != nil {
			return fmt.Errorf("resolve user %q: %w", user, err)
		}
		if resolvedUser.Home != "" {
			defaultHome = resolvedUser.Home
		}
	}
	env = ensureEnvDefault(env, "HOME", defaultHome)

	if workdir != "" {
		if err := os.MkdirAll(workdir, 0755); err != nil {
			return fmt.Errorf("mkdir workdir %s: %w", workdir, err)
		}
		if err := os.Chdir(workdir); err != nil {
			return fmt.Errorf("chdir workdir %s: %w", workdir, err)
		}
	}

	if user != "" {
		groups := make([]int, 0, len(resolvedUser.Groups))
		for _, gid := range resolvedUser.Groups {
			groups = append(groups, int(gid))
		}
		if err := unix.Setgroups(groups); err != nil {
			return fmt.Errorf("setgroups: %w", err)
		}
		if err := unix.Setgid(int(resolvedUser.GID)); err != nil {
			return fmt.Errorf("setgid: %w", err)
		}
		if err := unix.Setuid(int(resolvedUser.UID)); err != nil {
			return fmt.Errorf("setuid: %w", err)
		}
	}

	argv := append([]string{resolvedPath}, args...)
	return syscall.Exec(resolvedPath, argv, env)
}

func ensureEnvDefault(env []string, key, value string) []string {
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return env
		}
	}
	return append(env, prefix+value)
}

func ensureHosts(entries []string) {
	if len(entries) == 0 {
		return
	}
	if err := os.MkdirAll("/etc", 0755); err != nil {
		fmt.Fprintf(os.Stderr, "[init] mkdir /etc: %v\n", err)
		return
	}
	f, err := os.OpenFile("/etc/hosts", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[init] open /etc/hosts: %v\n", err)
		return
	}
	defer f.Close()

	seen := map[string]struct{}{}
	for _, entry := range entries {
		parts := strings.SplitN(entry, "=", 2)
		if len(parts) != 2 {
			continue
		}
		host := strings.TrimSpace(parts[0])
		ip := strings.TrimSpace(parts[1])
		if host == "" || ip == "" {
			continue
		}
		key := ip + "\t" + host
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		if _, err := fmt.Fprintf(f, "%s\t%s\n", ip, host); err != nil {
			fmt.Fprintf(os.Stderr, "[init] write /etc/hosts: %v\n", err)
			return
		}
	}
}

func ensureResolvConf() {
	const defaultResolvConf = "nameserver 1.1.1.1\nnameserver 8.8.8.8\n"
	data, err := os.ReadFile("/etc/resolv.conf")
	if err == nil && strings.TrimSpace(string(data)) != "" {
		return
	}
	if err := os.MkdirAll("/etc", 0755); err != nil {
		fmt.Fprintf(os.Stderr, "[init] mkdir /etc: %v\n", err)
		return
	}
	if err := os.WriteFile("/etc/resolv.conf", []byte(defaultResolvConf), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "[init] write /etc/resolv.conf: %v\n", err)
		return
	}
	klogf("ensured /etc/resolv.conf")
}

func reapChildren() {
	for {
		pid, err := syscall.Wait4(-1, nil, syscall.WNOHANG, nil)
		if pid <= 0 || err != nil {
			break
		}
	}
}

func runExecIdleSupervisor() {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGCHLD)
	defer signal.Stop(sigs)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		reapChildren()
		select {
		case <-sigs:
		case <-ticker.C:
		}
	}
}

func persistExitCode(code int) {
	if err := os.WriteFile(guestExitCodeFile, []byte(strconv.Itoa(code)+"\n"), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "[init] write exit code: %v\n", err)
	}
}

func findShell() string {
	for _, sh := range []string{"/bin/bash", "/bin/sh", "/bin/ash"} {
		if _, err := os.Stat(sh); err == nil {
			return sh
		}
	}
	return "/bin/sh"
}

func setupConsole() {
	// Ensure /dev exists and create /dev/null (Go runtime needs it for fd 0-2 protection)
	os.MkdirAll("/dev", 0755)
	syscall.Mknod("/dev/null", syscall.S_IFCHR|0666, 1<<8|3)

	// Prefer /dev/console, which is the kernel-selected active console. Some
	// guest kernels can block when ttyS0 is opened directly before PID 1 has a
	// controlling terminal, while /dev/console remains stable.
	for _, dev := range []struct {
		path  string
		major int
		minor int
	}{
		{"/dev/console", 5, 1},
		{"/dev/ttyS0", 4, 64},     // x86 16550A UART
		{"/dev/ttyAMA0", 204, 64}, // ARM64 PL011 UART
	} {
		c, err := os.OpenFile(dev.path, os.O_RDWR|syscall.O_NONBLOCK, 0)
		if err != nil {
			syscall.Mknod(dev.path, syscall.S_IFCHR|0666, dev.major<<8|dev.minor)
			c, err = os.OpenFile(dev.path, os.O_RDWR|syscall.O_NONBLOCK, 0)
		}
		if err != nil {
			continue
		}
		_ = syscall.SetNonblock(int(c.Fd()), false)
		_ = dupTo(int(c.Fd()), 0)
		_ = dupTo(int(c.Fd()), 1)
		_ = dupTo(int(c.Fd()), 2)
		if err := claimControllingTTY(int(c.Fd())); err != nil {
			fmt.Fprintf(os.Stderr, "[init] claim controlling tty %s: %v\n", dev.path, err)
			klogf("claim controlling tty %q failed: %v", dev.path, err)
		}
		if err := configureConsoleTTY(int(c.Fd())); err != nil {
			fmt.Fprintf(os.Stderr, "[init] configure console tty %s: %v\n", dev.path, err)
			klogf("configure console tty %q failed: %v", dev.path, err)
		}
		os.Stdin = c
		os.Stdout = c
		os.Stderr = c
		return
	}
}

func claimControllingTTY(fd int) error {
	if fd < 0 {
		return fmt.Errorf("invalid console fd %d", fd)
	}
	if _, err := unix.Getsid(0); err != nil {
		if _, err := unix.Setsid(); err != nil && !errors.Is(err, syscall.EPERM) {
			return fmt.Errorf("setsid: %w", err)
		}
	}
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(unix.TIOCSCTTY), 0); errno != 0 && errno != syscall.EPERM {
		return errno
	}
	return nil
}

func configureConsoleTTY(fd int) error {
	termios, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return err
	}
	termios.Iflag |= unix.ICRNL | unix.IXON
	termios.Iflag &^= unix.IGNCR | unix.INLCR | unix.IXOFF
	termios.Oflag |= unix.OPOST | unix.ONLCR
	termios.Cflag |= unix.CREAD | unix.CLOCAL | unix.CS8
	termios.Lflag |= unix.ECHO | unix.ECHOE | unix.ECHOK | unix.ICANON | unix.IEXTEN | unix.ISIG
	termios.Lflag &^= unix.ECHONL | unix.NOFLSH | unix.TOSTOP
	termios.Cc[unix.VINTR] = 3
	termios.Cc[unix.VQUIT] = 28
	termios.Cc[unix.VERASE] = 127
	termios.Cc[unix.VKILL] = 21
	termios.Cc[unix.VEOF] = 4
	termios.Cc[unix.VTIME] = 0
	termios.Cc[unix.VMIN] = 1
	termios.Cc[unix.VSTART] = 17
	termios.Cc[unix.VSTOP] = 19
	termios.Cc[unix.VSUSP] = 26
	return unix.IoctlSetTermios(fd, unix.TCSETS, termios)
}

func ensureGuestDevLinks() {
	for _, link := range []struct {
		path   string
		target string
	}{
		{path: "/dev/fd", target: "/proc/self/fd"},
		{path: "/dev/stdin", target: "/proc/self/fd/0"},
		{path: "/dev/stdout", target: "/proc/self/fd/1"},
		{path: "/dev/stderr", target: "/proc/self/fd/2"},
		{path: "/dev/ptmx", target: "/dev/pts/ptmx"},
	} {
		if err := ensureGuestSymlink(link.path, link.target); err != nil {
			fmt.Fprintf(os.Stderr, "[init] symlink %s -> %s: %v\n", link.path, link.target, err)
		}
	}
}

func ensureGuestPTYSupport() {
	if err := os.MkdirAll("/dev/pts", 0755); err != nil {
		fmt.Fprintf(os.Stderr, "[init] mkdir /dev/pts: %v\n", err)
		return
	}
	if err := syscall.Mount("devpts", "/dev/pts", "devpts", 0, "newinstance,gid=5,mode=620,ptmxmode=666"); err != nil && !errors.Is(err, syscall.EBUSY) {
		fmt.Fprintf(os.Stderr, "[init] mount devpts on /dev/pts failed: %v\n", err)
		return
	}
	ensureGuestDevLinks()
}

func applyGuestSysctls() {
	for _, setting := range []struct {
		path  string
		value string
	}{
		// Match common container-runtime behavior so non-root workloads can
		// still bind ports like 53/80/443 inside the guest.
		{path: "/proc/sys/net/ipv4/ip_unprivileged_port_start", value: "0\n"},
	} {
		if err := os.WriteFile(setting.path, []byte(setting.value), 0644); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			klogf("set sysctl %s failed: %v", setting.path, err)
			fmt.Fprintf(os.Stderr, "[init] sysctl %s: %v\n", setting.path, err)
			continue
		}
		klogf("set sysctl %s=%s", setting.path, strings.TrimSpace(setting.value))
	}
}

func materializeRunDirsFromTmpfiles() {
	for _, dir := range []string{"/usr/lib/tmpfiles.d", "/lib/tmpfiles.d", "/etc/tmpfiles.d"} {
		klogf("tmpfiles scan start dir=%q", dir)
		applyRunTmpfilesDir(dir)
		klogf("tmpfiles scan done dir=%q", dir)
	}
}

func applyRunTmpfilesDir(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".conf" {
			continue
		}
		klogf("tmpfiles apply file=%q", filepath.Join(dir, entry.Name()))
		applyRunTmpfilesFile(filepath.Join(dir, entry.Name()))
		klogf("tmpfiles applied file=%q", filepath.Join(dir, entry.Name()))
	}
}

func applyRunTmpfilesFile(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		kind := fields[0]
		if kind == "" {
			continue
		}
		switch kind[0] {
		case 'd', 'D':
		default:
			continue
		}
		target := fields[1]
		if target != "/run" && !strings.HasPrefix(target, "/run/") {
			continue
		}
		if strings.Contains(target, "%") {
			continue
		}
		mode := os.FileMode(0755)
		if len(fields) >= 3 && fields[2] != "-" {
			if parsed, parseErr := strconv.ParseUint(fields[2], 8, 32); parseErr == nil {
				mode = os.FileMode(parsed)
			}
		}
		if err := os.MkdirAll(target, mode); err != nil {
			klogf("tmpfiles mkdir %q from %s failed: %v", target, path, err)
			continue
		}
		if err := os.Chmod(target, mode); err != nil {
			klogf("tmpfiles chmod %q from %s failed: %v", target, path, err)
		}
	}
}

func ensureGuestSymlink(path, target string) error {
	if existing, err := os.Lstat(path); err == nil {
		if existing.Mode()&os.ModeSymlink != 0 {
			current, readErr := os.Readlink(path)
			if readErr == nil && current == target {
				return nil
			}
		}
		if err := os.RemoveAll(path); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.Symlink(target, path)
}

func mountRootDisk(cmdline map[string]string) string {
	rootDev := cmdline["root"]
	if rootDev == "" {
		klogf("no root device requested")
		return ""
	}
	fstype := cmdline["rootfstype"]
	if fstype == "" {
		fstype = "ext4"
	}
	klogf("waiting for root device %q (%s)", rootDev, fstype)
	// Wait for the block device to appear (up to 5 seconds)
	for i := 0; i < 50; i++ {
		if _, err := os.Stat(rootDev); err == nil {
			klogf("root device %q appeared after %d checks", rootDev, i+1)
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if _, err := os.Stat(rootDev); err != nil {
		klogf("root device %q not present: %v", rootDev, err)
		return ""
	}
	// Rootfs overlay: by default the block device mounts read-only as the
	// LOWER layer of an overlayfs, and all guest writes land in a per-VM
	// tmpfs UPPER layer. This matches Docker's "each run gets a fresh
	// writable layer" model, keeps the host-side cached template pristine
	// across reboots, and lets copyDiskImage use the hardlink fast-path
	// (pkg/container/container.go) for zero-copy boot.
	//
	// Pass gc.rootfs_overlay=off (CLI: --rootfs-persistent) when writes
	// need to survive VM shutdown, e.g. for image-building or debugfs-based
	// disk introspection. That path copies the template to a per-VM file
	// and mounts it directly rw.
	os.MkdirAll("/mnt", 0755)
	overlayDisabled := cmdline["gc.rootfs_overlay"] == "off"

	// The legacy flags apply to both paths: gc.fs_sync still forces sync writes
	// on the lower mount, and the cmdline "rw" historically opted the rootfs
	// into writable mode. With overlay on, "rw" is effectively always implied
	// (the overlay itself is writable) but we still honour gc.fs_sync for the
	// rare case where someone wants synchronous writes to punch through.
	lowerFlags := uintptr(syscall.MS_RDONLY)
	if overlayDisabled && cmdline["rw"] != "" {
		lowerFlags &^= syscall.MS_RDONLY
	}
	if cmdline["gc.fs_sync"] == "1" {
		lowerFlags |= syscall.MS_SYNCHRONOUS
	}

	klogf("about to mount %q (overlay=%v)", rootDev, !overlayDisabled)
	stopMountLog := make(chan struct{})
	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				klogf("mount %q still in progress", rootDev)
			case <-stopMountLog:
				return
			}
		}
	}()

	if overlayDisabled {
		if err := syscall.Mount(rootDev, "/mnt", fstype, lowerFlags, ""); err != nil {
			close(stopMountLog)
			klogf("mount %q on /mnt failed: %v", rootDev, err)
			fmt.Fprintf(os.Stderr, "[init] mount %s on /mnt: %v\n", rootDev, err)
			return ""
		}
		close(stopMountLog)
		klogf("mounted %q on /mnt (direct, no overlay)", rootDev)
		return "/mnt"
	}

	lowerDir := "/rootfs-lower"
	scratchDir := "/rootfs-overlay"
	upperDir := scratchDir + "/upper"
	workDir := scratchDir + "/work"
	for _, d := range []string{lowerDir, scratchDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			close(stopMountLog)
			klogf("mkdir %q failed: %v", d, err)
			fmt.Fprintf(os.Stderr, "[init] mkdir %s: %v\n", d, err)
			return ""
		}
	}
	if err := syscall.Mount(rootDev, lowerDir, fstype, lowerFlags, ""); err != nil {
		close(stopMountLog)
		klogf("mount %q on %q failed: %v", rootDev, lowerDir, err)
		fmt.Fprintf(os.Stderr, "[init] mount %s on %s: %v\n", rootDev, lowerDir, err)
		return ""
	}
	if err := syscall.Mount("tmpfs", scratchDir, "tmpfs", 0, "mode=0755"); err != nil {
		close(stopMountLog)
		klogf("mount tmpfs on %q failed: %v", scratchDir, err)
		fmt.Fprintf(os.Stderr, "[init] mount tmpfs %s: %v\n", scratchDir, err)
		return ""
	}
	if err := os.MkdirAll(upperDir, 0755); err != nil {
		close(stopMountLog)
		klogf("mkdir %q failed: %v", upperDir, err)
		fmt.Fprintf(os.Stderr, "[init] mkdir %s: %v\n", upperDir, err)
		return ""
	}
	if err := os.MkdirAll(workDir, 0755); err != nil {
		close(stopMountLog)
		klogf("mkdir %q failed: %v", workDir, err)
		fmt.Fprintf(os.Stderr, "[init] mkdir %s: %v\n", workDir, err)
		return ""
	}
	overlayOpts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", lowerDir, upperDir, workDir)
	if err := syscall.Mount("overlay", "/mnt", "overlay", 0, overlayOpts); err != nil {
		close(stopMountLog)
		klogf("mount overlay on /mnt failed: %v", err)
		fmt.Fprintf(os.Stderr, "[init] mount overlay: %v\n", err)
		return ""
	}
	close(stopMountLog)
	klogf("mounted overlay rootfs on /mnt (lower=%s ro, upper=tmpfs)", rootDev)
	return "/mnt"
}

func switchRoot(newRoot string) error {
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil && !errors.Is(err, syscall.EINVAL) {
		return fmt.Errorf("make mounts private: %w", err)
	}

	for _, dir := range []string{"/proc", "/sys", "/dev", "/run", "/tmp"} {
		target := filepath.Join(newRoot, strings.TrimPrefix(dir, "/"))
		if err := os.MkdirAll(target, 0755); err != nil {
			return fmt.Errorf("mkdir %s: %w", target, err)
		}
		if err := syscall.Mount(dir, target, "", syscall.MS_MOVE, ""); err != nil {
			return fmt.Errorf("move mount %s -> %s: %w", dir, target, err)
		}
	}

	if err := os.Chdir(newRoot); err != nil {
		return fmt.Errorf("chdir %s: %w", newRoot, err)
	}
	if err := syscall.Mount(".", "/", "", syscall.MS_MOVE, ""); err != nil {
		return fmt.Errorf("move root mount %s -> /: %w", newRoot, err)
	}
	if err := syscall.Chroot("."); err != nil {
		return fmt.Errorf("chroot .: %w", err)
	}
	if err := os.Chdir("/"); err != nil {
		return fmt.Errorf("chdir /: %w", err)
	}
	klogf("switch_root completed into %q", newRoot)
	return nil
}

func mountSharedFilesystems(mounts []runtimecfg.SharedFSMount) {
	for _, mount := range mounts {
		if mount.Tag == "" || mount.Target == "" {
			continue
		}
		target := mount.Target
		if err := os.MkdirAll(target, 0755); err != nil {
			klogf("mkdir sharedfs target %q failed: %v", target, err)
			fmt.Fprintf(os.Stderr, "[init] mkdir sharedfs target %s: %v\n", target, err)
			continue
		}
		flags := uintptr(0)
		if mount.ReadOnly {
			flags |= syscall.MS_RDONLY
		}
		if err := mountSharedFilesystemWithRetry(mount.Tag, target, flags); err != nil {
			klogf("mount sharedfs tag=%q target=%q failed: %v", mount.Tag, target, err)
			fmt.Fprintf(os.Stderr, "[init] mount virtiofs %s on %s: %v\n", mount.Tag, target, err)
			continue
		}
		klogf("mounted sharedfs tag=%q target=%q", mount.Tag, target)
	}
}

func mountSharedFilesystemWithRetry(tag, target string, flags uintptr) error {
	const (
		maxWait = 10 * time.Second
		retryIn = 100 * time.Millisecond
	)
	deadline := time.Now().Add(maxWait)
	for {
		err := syscall.Mount(tag, target, "virtiofs", flags, "")
		if err == nil {
			return nil
		}
		if !errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.ENODEV) && !errors.Is(err, syscall.ENOENT) && !errors.Is(err, syscall.ENXIO) {
			return err
		}
		if time.Now().After(deadline) {
			return err
		}
		time.Sleep(retryIn)
	}
}

func mountFS(fstype, target, vfstype string, flags uintptr, data string) {
	if err := syscall.Mount(fstype, target, vfstype, flags, data); err != nil {
		klogf("mount %q on %q failed: %v", fstype, target, err)
		fmt.Fprintf(os.Stderr, "[init] mount %s on %s failed: %v\n", fstype, target, err)
		return
	}
	klogf("mounted %q on %q", fstype, target)
}
