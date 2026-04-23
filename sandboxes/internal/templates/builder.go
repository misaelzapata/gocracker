package templates

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	gclog "github.com/gocracker/gocracker/internal/log"
	"github.com/gocracker/gocracker/pkg/container"
	"github.com/gocracker/gocracker/pkg/vmm"
	"github.com/gocracker/gocracker/pkg/warmcache"
)

// ColdBooter abstracts the cold-boot-and-capture step so tests can
// inject a fake (returning a fabricated snapshot dir) instead of
// standing up real KVM. Production wires the default via
// containerColdBooter which calls container.Run + waits for
// WarmDone + warmcache.Lookup.
type ColdBooter interface {
	BootAndCapture(ctx context.Context, opts container.RunOptions) (snapshotDir string, err error)
}

// Builder turns a Spec into a ready Template by cold-booting the VM
// once, capturing the warmcache snapshot, and storing the dir in the
// registry. Subsequent Builds with the same Spec hit the cache and
// return in ~ms instead of re-running the cold-boot.
//
// The builder is intentionally minimal — it doesn't own goroutine
// lifecycle (callers can wrap Build in their own goroutine if they
// want async builds). Concurrent Builds for the SAME SpecHash are
// serialized via per-hash mutex so two near-simultaneous POST
// /templates calls don't both cold-boot the same image.
type Builder struct {
	Registry *Registry
	// Booter is the cold-boot-and-capture implementation. nil =
	// production default (containerColdBooter). Tests inject a
	// fake.
	Booter ColdBooter

	// VMMBinary / JailerBinary are plumbed into the container.Run
	// RunOptions on every Build. These aren't part of Spec (they're
	// per-operator, not per-template — the SAME snapshot can be
	// produced by different operator paths) so they live on the
	// Builder instead of SpecHash.
	VMMBinary    string
	JailerBinary string

	mu       sync.Mutex
	building map[string]*sync.Mutex // spec_hash → per-hash lock
}

// NewBuilder wires a builder onto a registry. Both must be set.
func NewBuilder(r *Registry) *Builder {
	return &Builder{
		Registry: r,
		building: map[string]*sync.Mutex{},
	}
}

// ErrSpecRequired is returned by Build when the spec doesn't pass
// Validate.
var ErrSpecRequired = errors.New("templates: spec validation failed")

// BuildResult is what Build returns. ID is the registry key (newly
// generated for a fresh build, reused for a cache hit). CacheHit
// distinguishes the two paths so callers / metrics can track ratio.
type BuildResult struct {
	Template Template
	CacheHit bool
}

// Build registers the spec with the given ID (or generates one if
// id == "") and returns a Ready template. If a Ready template with
// the same SpecHash already exists, Build is a near-no-op: the new
// id maps to the existing snapshot, no cold-boot runs.
//
// On cache miss, Build cold-boots via container.Run with WarmCapture
// + InteractiveExec, waits for WarmDone, stops the VM, and stores
// the snapshot dir on the template record. On error the template
// transitions to StateError with LastError populated.
//
// ctx cancellation aborts the build; the underlying container.Run
// won't gracefully cancel mid-boot but Build returns promptly once
// the VM lands and Close runs. A subsequent Build for the same Spec
// will retry from scratch (the failed template stays in the registry
// for diagnostic; FindBySpecHash skips it).
func (b *Builder) Build(ctx context.Context, id string, spec Spec) (BuildResult, error) {
	if err := spec.Validate(); err != nil {
		return BuildResult{}, err
	}
	hash := SpecHash(spec)

	// Per-hash lock: serialize concurrent Build calls for the same
	// spec so we never cold-boot twice for the same snapshot.
	hashLock := b.lockForHash(hash)
	hashLock.Lock()
	defer hashLock.Unlock()

	// Cache hit fast path.
	if existing, ok := b.Registry.FindBySpecHash(hash); ok {
		// Verify the snapshot dir still exists — operators sometimes
		// rm -rf the cache to free disk. If gone, fall through to
		// rebuild.
		if existing.SnapshotDir != "" && pathExists(existing.SnapshotDir) {
			tmpl := &Template{
				ID:          chooseID(id),
				SpecHash:    hash,
				Spec:        spec,
				State:       StateReady,
				SnapshotDir: existing.SnapshotDir,
				CreatedAt:   time.Now().UTC(),
			}
			if err := b.Registry.Add(tmpl); err != nil {
				return BuildResult{}, fmt.Errorf("templates: register cache hit: %w", err)
			}
			return BuildResult{Template: *tmpl, CacheHit: true}, nil
		}
		// Stale registry entry — continue to rebuild path.
	}

	// Cache miss: register Building, cold-boot, transition to Ready
	// or Error.
	tmpl := &Template{
		ID:        chooseID(id),
		SpecHash:  hash,
		Spec:      spec,
		State:     StateBuilding,
		CreatedAt: time.Now().UTC(),
	}
	if err := b.Registry.Add(tmpl); err != nil {
		return BuildResult{}, fmt.Errorf("templates: register building: %w", err)
	}

	booter := b.Booter
	if booter == nil {
		booter = containerColdBooter{}
	}
	opts := tmpl.AsRunOptions()
	opts.VMMBinary = b.VMMBinary
	opts.JailerBinary = b.JailerBinary

	// Readiness-aware snapshot: if the Spec declares an HTTP probe,
	// skip container.Run's built-in WarmCapture (which snaps at
	// first-output) and instead run the probe against the live guest
	// over the toolbox agent's /proxy/http/<port> surface. Once the
	// probe passes we take the snapshot manually so the captured
	// memory image reflects the app already serving — lease → restore
	// lands straight into a hot app, saving the 2-4 s init tax that
	// Postgres/Flask/etc. would otherwise pay on every lease.
	var (
		snapshotDir string
		err         error
	)
	if spec.Readiness != nil {
		// Readiness-aware capture needs the CMD to actually run (so the
		// app inside the guest comes up and answers the probe). The
		// default template flow uses InteractiveExec=true for a CMD-
		// agnostic snapshot; flip it off here so init runs spec.Cmd.
		// WarmCapture=false disables container.Run's auto-snapshot —
		// bootProbeCapture does the manual TakeSnapshot + warmcache.Store
		// after the probe passes.
		probeOpts := opts
		probeOpts.WarmCapture = false
		probeOpts.InteractiveExec = false
		snapshotDir, err = bootProbeCapture(ctx, probeOpts, *spec.Readiness)
	} else {
		snapshotDir, err = booter.BootAndCapture(ctx, opts)
	}
	if err != nil {
		b.Registry.Update(tmpl.ID, func(t *Template) {
			t.State = StateError
			t.LastError = err.Error()
		})
		return BuildResult{}, fmt.Errorf("templates: build %s: %w", tmpl.ID, err)
	}
	b.Registry.Update(tmpl.ID, func(t *Template) {
		t.State = StateReady
		t.SnapshotDir = snapshotDir
	})
	final, _ := b.Registry.Get(tmpl.ID)
	return BuildResult{Template: final, CacheHit: false}, nil
}

// containerColdBooter is the production ColdBooter: cold-boots via
// container.Run with WarmCapture, waits for WarmDone (snapshot
// write), stops the VM, and returns the warmcache dir.
type containerColdBooter struct{}

func (containerColdBooter) BootAndCapture(ctx context.Context, opts container.RunOptions) (string, error) {
	result, err := container.Run(opts)
	if err != nil {
		return "", err
	}
	defer result.Close()
	defer result.VM.Stop()
	if result.WarmDone != nil {
		select {
		case <-result.WarmDone:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	// After WarmDone, container.Run has populated the warmcache. Look
	// up the snapshot dir for the spec's key.
	//
	// container.Run applies defaults (Arch=GOARCH, MemMB=256 when 0)
	// BEFORE computing the store key, via runLocal/runViaWorker. The
	// builder needs to mirror those defaults so its post-capture Lookup
	// computes the same key as the Store, otherwise every first-time
	// template build errors with "lookup miss after capture" even though
	// the snapshot is on disk.
	lookupOpts := opts
	if lookupOpts.Arch == "" {
		lookupOpts.Arch = runtime.GOARCH
	}
	if lookupOpts.MemMB == 0 {
		lookupOpts.MemMB = 256
	}
	key, ok := container.ComputeWarmCacheKey(lookupOpts)
	if !ok {
		return "", errors.New("templates: warmcache key not derivable from spec")
	}
	dir, hit := warmcache.Lookup(warmcache.DefaultRoot(), key)
	if !hit {
		return "", fmt.Errorf("templates: warm-cache lookup miss after capture (key=%s, opts: image=%s mem=%d cpus=%d arch=%s network=%s exec=%v interactive=%v)",
			key[:12], lookupOpts.Image, lookupOpts.MemMB, lookupOpts.CPUs, lookupOpts.Arch, lookupOpts.NetworkMode, lookupOpts.ExecEnabled, lookupOpts.InteractiveExec)
	}
	return dir, nil
}

// Delete removes the template from the registry AND attempts to
// delete the snapshot dir from disk. The snapshot dir is shared
// across templates with the same SpecHash — only deleted if no
// other Ready template references the same dir.
//
// Returns ErrTemplateNotFound when the id is unknown.
func (b *Builder) Delete(id string) error {
	t, ok := b.Registry.Remove(id)
	if !ok {
		return ErrTemplateNotFound
	}
	if t.SnapshotDir == "" {
		return nil
	}
	// Don't delete the snapshot if any other Ready template still
	// points at it (same SpecHash → same dir).
	for _, other := range b.Registry.List() {
		if other.SnapshotDir == t.SnapshotDir && other.State == StateReady {
			return nil
		}
	}
	_ = os.RemoveAll(t.SnapshotDir)
	return nil
}

func (b *Builder) lockForHash(hash string) *sync.Mutex {
	b.mu.Lock()
	defer b.mu.Unlock()
	if m, ok := b.building[hash]; ok {
		return m
	}
	m := &sync.Mutex{}
	b.building[hash] = m
	return m
}

// chooseID returns the requested id or generates a new one if empty.
// Format "tmpl-XXXXXX" with 12 hex chars (~48 bits of entropy —
// plenty given templates are user-created, low cardinality).
func chooseID(requested string) string {
	if requested != "" {
		return requested
	}
	var b [6]byte
	_, _ = rand.Read(b[:])
	return "tmpl-" + hex.EncodeToString(b[:])
}

func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// bootProbeCapture cold-boots with WarmCapture=false (VM stays running,
// no auto-snapshot), polls an HTTP readiness probe via the toolbox
// agent's /proxy/http/<port> surface, and once the app returns 2xx
// takes a snapshot + stores it in the warm cache under the same key
// the non-readiness path would have used. The returned snapshotDir is
// the final `<root>/<key>` path the restore fast-path will hit.
//
// Timeout semantics: probe is capped by `probe.Timeout` (default 2m);
// ctx cancellation aborts the probe immediately. Any probe error tears
// down the VM so the caller doesn't leak a half-ready sandbox.
//
// Why this exists: snapshots taken at first-output (the default) miss
// the app's real readiness. A Flask+Postgres template snapped at first
// output restores in 30 ms but the guest still spends 2–4 s finishing
// Postgres initdb + Flask startup on every lease. Snapshotting *after*
// the app answers 2xx captures the live-service memory image — every
// lease is immediately usable.
func bootProbeCapture(ctx context.Context, opts container.RunOptions, probe ReadinessProbe) (string, error) {
	timeout := probe.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	interval := probe.Interval
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}
	path := probe.HTTPPath
	if path == "" {
		path = "/"
	}
	if probe.HTTPPort == 0 {
		return "", errors.New("templates: Readiness.HTTPPort required")
	}

	result, err := container.Run(opts)
	if err != nil {
		return "", err
	}
	defer result.Close()
	defer result.VM.Stop()

	dialer, ok := result.VM.(vmm.VsockDialer)
	if !ok {
		return "", errors.New("templates: VM handle does not implement VsockDialer (cannot drive readiness probe)")
	}

	// Poll /proxy/http/<port><path> over vsock:10023.
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var lastErr error
	var lastStatus int
	for attempt := 0; ; attempt++ {
		if err := probeCtx.Err(); err != nil {
			return "", fmt.Errorf("templates: readiness timed out after %s (%d attempts, last_status=%d, last_err=%v): %w", timeout, attempt, lastStatus, lastErr, err)
		}
		status, perr := probeOnce(dialer, probe.HTTPPort, path, 2*time.Second)
		lastErr = perr
		lastStatus = status
		if perr == nil && status >= 200 && status < 300 {
			break
		}
		select {
		case <-time.After(interval):
		case <-probeCtx.Done():
			return "", fmt.Errorf("templates: readiness timed out waiting for %d%s (last_status=%d, last_err=%v)", probe.HTTPPort, path, lastStatus, lastErr)
		}
	}

	// Quiesce the guest's network stack before snapshot.
	//
	// Why: a snapshot of a live guest with a listening TCP socket leaves
	// virtio-net skbuff state (RX/TX ring descriptors, in-flight sk_buff
	// chains) in a transient mid-flight form. On restore the guest
	// kernel walks those buffers and can hit `skb_over_panic: len=6.3M`
	// when it reads garbage as a packet length — the kernel then
	// BUG()s in net/core/skbuff.c and the restored VM is dead before
	// init even finishes resuming.
	//
	// Fix: ask the agent to `ip link set eth0 down` right before
	// snapshot. That flushes skbuff in both directions, parks the
	// virtio queues, and leaves the guest kernel in a clean point.
	// User listen sockets are bound to INADDR_ANY so they survive
	// (they just don't receive traffic while eth0 is down). On
	// restore, container.Run's reIPGuest brings eth0 back up with
	// the new IP, and the socket resumes receiving traffic.
	if err := quiesceGuestNet(dialer, 2*time.Second); err != nil {
		gclog.VMM.Warn("templates: eth0 quiesce failed; snapshot may panic on restore", "err", err.Error())
	}
	// Let the guest drain pending RX/TX descriptors.
	time.Sleep(50 * time.Millisecond)

	// Snapshot the live VM. TakeSnapshot pauses → writes → resumes.
	tmp, err := os.MkdirTemp("", "gocracker-readysnap-*")
	if err != nil {
		return "", fmt.Errorf("templates: mktemp: %w", err)
	}
	defer func() {
		// On success Store renames tmp away; on failure we own the
		// cleanup ourselves.
		if _, statErr := os.Stat(tmp); statErr == nil {
			_ = os.RemoveAll(tmp)
		}
	}()
	snap, err := result.VM.TakeSnapshot(tmp)
	if err != nil {
		return "", fmt.Errorf("templates: take snapshot: %w", err)
	}

	// Hardlink the runtime disk into the snapshot dir so a) the warm
	// cache is self-contained (survives runtime workdir cleanup) and
	// b) snapshot.json's DiskImage is relative. Mirrors what
	// captureWarmSnapshot does in pkg/container for the non-readiness
	// path.
	if snap != nil && snap.Config.DiskImage != "" && !strings.HasPrefix(snap.Config.DiskImage, "artifacts/") {
		artifactsDir := filepath.Join(tmp, "artifacts")
		if err := os.MkdirAll(artifactsDir, 0o755); err != nil {
			return "", fmt.Errorf("templates: mkdir artifacts: %w", err)
		}
		destDisk := filepath.Join(artifactsDir, "disk.ext4")
		if err := os.Link(snap.Config.DiskImage, destDisk); err != nil {
			// If hardlink fails (cross-fs), fall back to copy.
			if copyErr := copyFile(snap.Config.DiskImage, destDisk); copyErr != nil {
				return "", fmt.Errorf("templates: link disk: %w (copy fallback: %v)", err, copyErr)
			}
		}
		snap.Config.DiskImage = "artifacts/disk.ext4"
		data, err := json.MarshalIndent(snap, "", "  ")
		if err != nil {
			return "", fmt.Errorf("templates: marshal snapshot.json: %w", err)
		}
		if err := os.WriteFile(filepath.Join(tmp, "snapshot.json"), data, 0o644); err != nil {
			return "", fmt.Errorf("templates: rewrite snapshot.json: %w", err)
		}
	}

	// Mirror the defaults container.Run applied before storing (see
	// the same workaround in the non-readiness path below).
	lookupOpts := opts
	if lookupOpts.Arch == "" {
		lookupOpts.Arch = runtime.GOARCH
	}
	if lookupOpts.MemMB == 0 {
		lookupOpts.MemMB = 256
	}
	// Enable WarmCapture=true on the lookup opts so ComputeWarmCacheKey
	// excludes the Cmdline (CMD-agnostic). Otherwise the readiness
	// snapshot's lookup key would include opts.Cmd and miss on a lease
	// that fills in its own cmd.
	lookupOpts.WarmCapture = true
	key, ok := container.ComputeWarmCacheKey(lookupOpts)
	if !ok {
		return "", errors.New("templates: warmcache key not derivable from spec")
	}
	root := warmcache.DefaultRoot()
	if err := warmcache.Store(tmp, root, key); err != nil {
		return "", fmt.Errorf("templates: warmcache store: %w", err)
	}
	return warmcache.Dir(root, key), nil
}

// quiesceGuestNet asks the toolbox agent to run `ip link set eth0 down`
// inside the guest before we take the snapshot. Purpose: flush
// virtio-net skbuff state so the captured snapshot has no in-flight
// packets — without this, the restored guest kernel BUG()s at
// skb_over_panic (net/core/skbuff.c:120) as it walks a mid-snapshot
// sk_buff chain and reads a garbage length.
//
// Best-effort — a failure is logged by the caller but doesn't abort
// the build. Worst case the restore panics, which is the status quo
// we're trying to fix.
func quiesceGuestNet(dialer vmm.VsockDialer, timeout time.Duration) error {
	conn, err := dialer.DialVsock(10023)
	if err != nil {
		return fmt.Errorf("dial agent: %w", err)
	}
	defer conn.Close()
	// Exec over the agent: POST /exec with cmd = ["/sbin/ip", "link",
	// "set", "eth0", "down"]. Agent streams back the framed response;
	// we only care about the exit code.
	reqBody := `{"cmd":["ip","link","set","eth0","down"]}`
	req := fmt.Sprintf(
		"POST /exec HTTP/1.0\r\nHost: x\r\nContent-Length: %d\r\nContent-Type: application/json\r\nConnection: close\r\n\r\n%s",
		len(reqBody), reqBody,
	)
	if _, err := io.WriteString(conn, req); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	// Drain briefly so we don't leave the connection in RST state.
	buf := make([]byte, 1024)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		n, err := conn.Read(buf)
		if n == 0 || err != nil {
			break
		}
	}
	gclog.VMM.Info("guest eth0 quiesced for snapshot")
	return nil
}

// probeOnce issues a single `GET /proxy/http/<port><path>` request
// against the toolbox agent over vsock:10023 and returns the HTTP
// status code. Treats the first non-blank line as the HTTP status
// line (HTTP/1.0 or HTTP/1.1). Connection is closed after read.
func probeOnce(dialer vmm.VsockDialer, port uint16, path string, timeout time.Duration) (int, error) {
	// Dial the agent in a goroutine so a hung DialVsock can't wedge
	// the probe loop indefinitely (the outer retry loop checks
	// probeCtx.Err() only between iterations — if DialVsock never
	// returns, the loop never advances, and the build hangs past the
	// template's declared timeout).
	type dialResult struct {
		conn interface {
			io.ReadWriter
			Close() error
		}
		err error
	}
	dialCh := make(chan dialResult, 1)
	go func() {
		c, err := dialer.DialVsock(10023)
		dialCh <- dialResult{c, err}
	}()
	var conn interface {
		io.ReadWriter
		Close() error
	}
	select {
	case r := <-dialCh:
		if r.err != nil {
			return 0, fmt.Errorf("dial agent vsock: %w", r.err)
		}
		conn = r.conn
	case <-time.After(timeout):
		// Orphan the goroutine — DialVsock will eventually fail or
		// succeed; either way we're no longer interested. It may leak
		// the conn briefly but it's capped by the underlying vsock
		// bridge lifetime.
		return 0, fmt.Errorf("dial agent vsock: timeout after %s", timeout)
	}
	defer conn.Close()
	// Apply a deadline to the whole request-response cycle too, using
	// SetDeadline when the conn supports it (the vsock-bridged conn
	// returned by the UDS listener does).
	if dc, ok := conn.(interface{ SetDeadline(time.Time) error }); ok {
		_ = dc.SetDeadline(time.Now().Add(timeout))
	}
	req := fmt.Sprintf(
		"GET /proxy/http/%d%s HTTP/1.0\r\nHost: x\r\nConnection: close\r\nUser-Agent: gocracker-templates/1\r\n\r\n",
		port, path,
	)
	if _, err := io.WriteString(conn, req); err != nil {
		return 0, fmt.Errorf("write probe: %w", err)
	}
	br := bufio.NewReader(conn)
	statusLine, err := br.ReadString('\n')
	if err != nil {
		return 0, fmt.Errorf("read probe status: %w", err)
	}
	// Discard remaining headers/body — we only care about status.
	go func() { _, _ = io.Copy(io.Discard, br) }()
	parts := strings.SplitN(strings.TrimRight(statusLine, "\r\n"), " ", 3)
	if len(parts) < 2 || !strings.HasPrefix(parts[0], "HTTP/") {
		return 0, fmt.Errorf("bad status line: %q", statusLine)
	}
	var status int
	if _, err := fmt.Sscanf(parts[1], "%d", &status); err != nil {
		return 0, fmt.Errorf("parse status code: %w", err)
	}
	// Courtesy sanity: 502 from the agent with our custom error body is
	// the canonical "app not listening yet" signal — callers treat it
	// identically to a timeout, as if it were a non-2xx.
	if status == http.StatusBadGateway {
		return status, nil
	}
	return status, nil
}

// copyFile is the hardlink fallback when src + dst live on different
// filesystems. Streams via io.Copy; mode bits are preserved.
func copyFile(src, dst string) error {
	s, err := os.Open(src)
	if err != nil {
		return err
	}
	defer s.Close()
	info, err := s.Stat()
	if err != nil {
		return err
	}
	d, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	defer d.Close()
	if _, err := io.Copy(d, s); err != nil {
		return err
	}
	return nil
}

