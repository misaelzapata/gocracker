// pool-bench measures end-to-end lease latency against the Fase 5
// sandboxes/internal/pool warm pool. The plan §5 success criterion
// is p95 lease < 20 ms on a burst of 50 concurrent CreateSandbox
// requests, with 0 errors and 0 orphan VMs after 5 min.
//
// This bench drives sandboxd's Manager directly (no HTTP overhead)
// so the measurement is pure pool latency: pool.AcquireWait + IP
// allocator + Resume + SetNetwork. The legacy v0 bench in
// legacy_v0.go (build-tag-gated) exercises the warmcache restore
// path without going through the pool surface — keep both for now,
// the legacy one stays useful for regression checks on the warm-cache
// internals.
//
// Usage:
//
//	sudo go run ./tools/pool-bench \
//	  -image alpine:3.20 \
//	  -kernel artifacts/kernels/gocracker-guest-standard-vmlinux \
//	  -burst 50 -warm 8
//
// Reports p50 / p95 / p99 lease ms, plus a budget check against the
// plan target (--p95-budget-ms, default 20).
package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gocracker/gocracker/internal/toolbox/agent"
	toolboxspec "github.com/gocracker/gocracker/internal/toolbox/spec"
	"github.com/gocracker/gocracker/pkg/container"
	"github.com/gocracker/gocracker/sandboxes/internal/sandboxd"
)

func main() {
	image := flag.String("image", "alpine:3.20", "OCI image")
	kernel := flag.String("kernel", "artifacts/kernels/gocracker-guest-standard-vmlinux", "kernel path")
	burst := flag.Int("burst", 50, "concurrent lease requests")
	warm := flag.Int("warm", 8, "MinPaused / MaxPaused (pool size)")
	memMB := flag.Uint64("mem", 256, "guest memory MiB")
	jailerMode := flag.String("jailer", container.JailerModeOff, "jailer mode: on or off")
	p95BudgetMs := flag.Int("p95-budget-ms", 20, "fail if p95 exceeds this (plan §5: <20 ms)")
	stateDir := flag.String("state-dir", "/tmp/pool-bench-state", "sandboxd state directory")
	timeoutS := flag.Int("timeout-s", 30, "per-lease wait timeout")
	execCmd := flag.String("exec", "", "if set, exec this command in each leased VM (e.g. \"node --version\" or \"bun --version\") and report TTI as lease+exec end-to-end")
	flag.Parse()
	cmdParts := splitCmd(*execCmd)

	if os.Getuid() != 0 {
		fmt.Fprintln(os.Stderr, "pool-bench: must run as root (KVM + jailer require it)")
		os.Exit(2)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := os.MkdirAll(*stateDir, 0o755); err != nil {
		fatal("mkdir state-dir: %v", err)
	}
	store, err := sandboxd.NewStore("")
	if err != nil {
		fatal("new store: %v", err)
	}
	vmmBin := resolveVMMBinary()
	mgr := &sandboxd.Manager{
		Store:        store,
		StateDir:     *stateDir,
		VMMBinary:    vmmBin,
		JailerBinary: vmmBin,
	}

	tmpl := "bench-pool"
	fmt.Printf("pool-bench: image=%s burst=%d warm=%d jailer=%s\n", *image, *burst, *warm, *jailerMode)
	fmt.Printf("registering pool '%s' (MinPaused=%d MaxPaused=%d)... ", tmpl, *warm, *warm)
	tStart := time.Now()
	reg, err := mgr.RegisterPool(ctx, sandboxd.CreatePoolRequest{
		TemplateID: tmpl,
		Image:      *image,
		KernelPath: *kernel,
		MemMB:      *memMB,
		JailerMode: *jailerMode,
		MinPaused:  *warm,
		MaxPaused:  *warm,
	})
	if err != nil {
		fatal("register pool: %v", err)
	}
	defer func() {
		_ = mgr.UnregisterPool(tmpl)
	}()
	fmt.Printf("ok (%dms)\n", time.Since(tStart).Milliseconds())

	// Wait until the pool is fully warm. Bench timing depends on hot
	// hits — measuring while half the requests cold-boot inflates p95
	// to seconds and obscures the steady-state target.
	fmt.Printf("warming up to %d paused VMs", *warm)
	warmStart := time.Now()
	for {
		counts := getPausedCount(mgr, tmpl)
		fmt.Printf(".")
		if counts >= *warm {
			break
		}
		if time.Since(warmStart) > 5*time.Minute {
			fmt.Println()
			fatal("pool didn't reach %d paused in 5 min (current: %d)", *warm, counts)
		}
		time.Sleep(200 * time.Millisecond)
	}
	fmt.Printf(" ok (%ds)\n\n", int(time.Since(warmStart).Seconds()))
	_ = reg

	// Burst N concurrent leases.
	fmt.Printf("burst-%d leases ...\n", *burst)
	type sample struct {
		idx     int
		latency time.Duration
		err     error
		id      string
	}
	results := make(chan sample, *burst)
	var wg sync.WaitGroup
	var inFlight atomic.Int32

	burstStart := time.Now()
	for i := 0; i < *burst; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			inFlight.Add(1)
			defer inFlight.Add(-1)

			t0 := time.Now()
			sb, err := mgr.LeaseSandbox(ctx, sandboxd.LeaseSandboxRequest{
				TemplateID: tmpl,
				Timeout:    time.Duration(*timeoutS) * time.Second,
			})
			if err != nil {
				results <- sample{idx: i, latency: time.Since(t0), err: err}
				return
			}
			// If -exec is set, measure TTI = lease + exec round-trip.
			// This is the number that matters for the user-visible
			// "I asked for a sandbox and got my command output back".
			if len(cmdParts) > 0 {
				_, execErr := execAndReadFirstChunk(ctx, sb.UDSPath, cmdParts, 5*time.Second)
				if execErr != nil {
					results <- sample{idx: i, latency: time.Since(t0), err: execErr, id: sb.ID}
					return
				}
			}
			elapsed := time.Since(t0)
			results <- sample{idx: i, latency: elapsed, err: nil, id: sb.ID}
		}(i)
	}
	wg.Wait()
	close(results)
	burstElapsed := time.Since(burstStart)

	// Collect.
	var oks []time.Duration
	var leasedIDs []string
	fails := 0
	var firstErr error
	for s := range results {
		if s.err != nil {
			fails++
			if firstErr == nil {
				firstErr = s.err
			}
			continue
		}
		oks = append(oks, s.latency)
		leasedIDs = append(leasedIDs, s.id)
	}

	// Tear leased VMs down so the next bench / shutdown is clean.
	fmt.Printf("\nreleasing %d leases...\n", len(leasedIDs))
	for _, id := range leasedIDs {
		_ = mgr.ReleaseLeased(id)
	}

	if len(oks) == 0 {
		fatal("no successful leases (fails=%d, first err: %v)", fails, firstErr)
	}

	// Stats.
	sort.Slice(oks, func(i, j int) bool { return oks[i] < oks[j] })
	p50 := oks[len(oks)/2].Milliseconds()
	p95 := oks[int(float64(len(oks))*0.95)].Milliseconds()
	p99 := oks[int(float64(len(oks))*0.99)].Milliseconds()
	min := oks[0].Milliseconds()
	max := oks[len(oks)-1].Milliseconds()
	var sum time.Duration
	for _, d := range oks {
		sum += d
	}
	mean := (sum / time.Duration(len(oks))).Milliseconds()

	fmt.Println("\n================================================================")
	fmt.Printf("burst-%d (%d ok / %d fail, total %s)\n", *burst, len(oks), fails, burstElapsed.Round(time.Millisecond))
	fmt.Println("----------------------------------------------------------------")
	fmt.Printf("min   %3d ms\n", min)
	fmt.Printf("p50   %3d ms\n", p50)
	fmt.Printf("mean  %3d ms\n", mean)
	fmt.Printf("p95   %3d ms  (budget %d ms)\n", p95, *p95BudgetMs)
	fmt.Printf("p99   %3d ms\n", p99)
	fmt.Printf("max   %3d ms\n", max)
	fmt.Println("================================================================")

	if firstErr != nil {
		fmt.Printf("\nfirst error: %v\n", firstErr)
	}

	// Gate.
	if fails > 0 {
		fmt.Printf("\nFAIL: %d failures (plan: 0)\n", fails)
		os.Exit(1)
	}
	if int(p95) > *p95BudgetMs {
		fmt.Printf("\nFAIL: p95=%dms > budget=%dms\n", p95, *p95BudgetMs)
		os.Exit(1)
	}
	fmt.Printf("\nPASS\n")
}

func getPausedCount(mgr *sandboxd.Manager, templateID string) int {
	for _, r := range mgr.ListPools() {
		if r.TemplateID == templateID {
			return r.Counts["paused"]
		}
	}
	return 0
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "pool-bench: "+format+"\n", args...)
	os.Exit(1)
}

// resolveVMMBinary locates the `gocracker` binary used to spawn VMM
// workers. Checks sibling-of-self (which is how the CI job invokes
// us, with /tmp/pool-bench next to nothing — fallthrough) then $PWD
// and $PWD/bin. Returns "gocracker" for PATH lookup as last resort.
func resolveVMMBinary() string {
	if self, err := os.Executable(); err == nil {
		for _, candidate := range []string{
			filepath.Join(filepath.Dir(self), "gocracker"),
			filepath.Join(mustCwd(), "gocracker"),
			filepath.Join(mustCwd(), "bin", "gocracker"),
		} {
			if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
				return candidate
			}
		}
	}
	return "gocracker"
}

func mustCwd() string {
	d, _ := os.Getwd()
	return d
}

// splitCmd is a poor-man's shlex — splits on whitespace, no quoting.
// Sufficient for benchmark commands like "node --version" or
// "bun --eval 'console.log(1+1)'" (avoid quotes — use a wrapper
// script if you need them).
func splitCmd(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	parts := strings.Fields(s)
	return parts
}

// execAndReadFirstChunk dials the toolbox UDS, sends an exec request
// for cmd, and waits until either (a) a stdout/stderr frame arrives,
// or (b) the exit frame arrives, or (c) timeout. Returns the first
// chunk of output (capped at 256 bytes for logging). Used by
// pool-bench to measure end-to-end TTI = lease + exec round-trip.
//
// We DON'T wait for the full output because TTI is "first byte" —
// node --version prints ~10 bytes and exits immediately, so reading
// just one frame is all we need.
func execAndReadFirstChunk(ctx context.Context, udsPath string, cmd []string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	dialCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	conn, err := dialUDSAndConnect(dialCtx, udsPath, toolboxspec.VsockPort)
	if err != nil {
		return "", fmt.Errorf("dial uds: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(deadline)

	body, _ := json.Marshal(agent.ExecRequest{Cmd: cmd})
	httpReq := fmt.Sprintf(
		"POST /exec HTTP/1.0\r\nHost: x\r\nContent-Length: %d\r\nContent-Type: application/json\r\nConnection: close\r\n\r\n",
		len(body),
	)
	if _, err := conn.Write([]byte(httpReq)); err != nil {
		return "", fmt.Errorf("write headers: %w", err)
	}
	if _, err := conn.Write(body); err != nil {
		return "", fmt.Errorf("write body: %w", err)
	}

	br := bufio.NewReader(conn)
	// Drain HTTP response line + headers.
	if _, err := br.ReadString('\n'); err != nil {
		return "", fmt.Errorf("read status: %w", err)
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return "", fmt.Errorf("read header: %w", err)
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}

	// Read framed responses until first stdout/stderr OR exit.
	for {
		ch, payload, err := readFrame(br)
		if err != nil {
			return "", fmt.Errorf("read frame: %w", err)
		}
		switch ch {
		case agent.ChannelStdout, agent.ChannelStderr:
			out := string(payload)
			if len(out) > 256 {
				out = out[:256]
			}
			return out, nil
		case agent.ChannelExit:
			// Exit before any output — return empty (still counts as
			// successful TTI; the command was a no-op printer that
			// wrote nothing or only whitespace).
			return "", nil
		}
	}
}

// dialUDSAndConnect mirrors internal/toolbox/client.dialAndConnect
// without depending on it (exported helpers would mean reaching
// across an internal/ boundary that we can't cross from cmd/).
func dialUDSAndConnect(ctx context.Context, udsPath string, port uint32) (net.Conn, error) {
	d := net.Dialer{}
	conn, err := d.DialContext(ctx, "unix", udsPath)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Write([]byte(fmt.Sprintf("CONNECT %d\n", port))); err != nil {
		conn.Close()
		return nil, fmt.Errorf("write CONNECT: %w", err)
	}
	br := bufio.NewReader(conn)
	line, err := br.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("read CONNECT response: %w", err)
	}
	if !strings.HasPrefix(line, "OK") {
		conn.Close()
		return nil, fmt.Errorf("CONNECT rejected: %s", strings.TrimSpace(line))
	}
	return conn, nil
}

// readFrame reads one framed message: [1 byte channel][4 byte len BE][payload].
// Mirrors internal/toolbox/agent.ReadFrame which is in an internal
// package we can't import from cmd/.
func readFrame(r io.Reader) (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	channel := hdr[0]
	n := binary.BigEndian.Uint32(hdr[1:5])
	if n > 64<<10 {
		return 0, nil, fmt.Errorf("frame too large: %d", n)
	}
	payload := make([]byte, n)
	if n > 0 {
		if _, err := io.ReadFull(r, payload); err != nil {
			return 0, nil, err
		}
	}
	return channel, payload, nil
}

// errors used to keep import list minimal.
var _ = errors.New
