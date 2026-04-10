//go:build darwin

package buildkit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gocracker/gocracker/internal/buildbackend"
	"github.com/gocracker/gocracker/internal/guest"
	"github.com/gocracker/gocracker/internal/guestexec"
	"github.com/gocracker/gocracker/internal/oci"
	"github.com/gocracker/gocracker/internal/runtimecfg"
	"github.com/gocracker/gocracker/pkg/vmm"
	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/exporter/containerimage/exptypes"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/auth/authprovider"
	"github.com/moby/buildkit/session/secrets/secretsprovider"
	"github.com/moby/buildkit/session/sshforward"
	"github.com/moby/buildkit/session/sshforward/sshprovider"
	"github.com/tonistiigi/fsutil"
	"github.com/tonistiigi/go-csvvalue"
)

const (
	defaultBuilderImageRef  = "moby/buildkit:buildx-stable-1"
	builderImageEnvKey      = "GOCRACKER_DARWIN_BUILDKIT_IMAGE"
	builderKernelEnvKey     = "GOCRACKER_DARWIN_BUILDKIT_KERNEL"
	builderStateMountPath   = "/var/lib/buildkit"
	builderSocketGuestPath  = builderStateMountPath + "/buildkitd.sock"
	builderStateMountTag    = "buildkit-state"
	builderReadyTimeout     = 30 * time.Second
	builderProbeInterval    = 500 * time.Millisecond
	builderProbeTimeout     = 2 * time.Second
	builderMemMB      uint64 = 2048
	builderCPUCount          = 2
	builderDiskMB            = 4096
)

var (
	getwdFunc       = os.Getwd
	executableFunc  = os.Executable
	defaultManagers sync.Map
)

type Backend struct {
	fallback buildbackend.Backend
	manager  *manager
}

func NewBackend(fallback buildbackend.Backend) buildbackend.Backend {
	if fallback == nil {
		fallback = buildbackend.NewDockerfileBackend()
	}
	return &Backend{
		fallback: fallback,
		manager:  sharedManager(),
	}
}

func (b *Backend) BuildRootfs(ctx context.Context, req buildbackend.Request) (*buildbackend.Result, error) {
	if strings.TrimSpace(req.Dockerfile) == "" {
		return b.fallback.BuildRootfs(ctx, req)
	}
	cacheDir := resolvedCacheDir(req.CacheDir)
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		inst, err := b.manager.ensure(ctx, cacheDir)
		if err != nil {
			return nil, err
		}
		result, err := b.buildViaBuilder(ctx, inst, req)
		if err == nil {
			return result, nil
		}
		lastErr = err
		b.manager.invalidate(cacheDir, inst)
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("darwin buildkit backend failed")
}

func (b *Backend) buildViaBuilder(ctx context.Context, inst *builderInstance, req buildbackend.Request) (*buildbackend.Result, error) {
	bk, err := newBuildkitClient(ctx, inst.dialBuildkit)
	if err != nil {
		return nil, fmt.Errorf("connect buildkit helper: %w", err)
	}
	defer bk.Close()

	waitCtx, cancel := context.WithTimeout(ctx, builderProbeTimeout)
	err = bk.Wait(waitCtx)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("buildkit helper is not ready: %w", err)
	}

	contextDir := strings.TrimSpace(req.Context)
	if contextDir == "" {
		contextDir = filepath.Dir(req.Dockerfile)
	}
	contextDir, err = filepath.Abs(contextDir)
	if err != nil {
		return nil, fmt.Errorf("resolve build context: %w", err)
	}
	dockerfilePath, err := filepath.Abs(req.Dockerfile)
	if err != nil {
		return nil, fmt.Errorf("resolve dockerfile: %w", err)
	}
	dockerfileDir := filepath.Dir(dockerfilePath)
	dockerfileName := filepath.Base(dockerfilePath)

	contextFS, err := fsutil.NewFS(contextDir)
	if err != nil {
		return nil, fmt.Errorf("open build context %s: %w", contextDir, err)
	}
	dockerfileFS, err := fsutil.NewFS(dockerfileDir)
	if err != nil {
		return nil, fmt.Errorf("open dockerfile dir %s: %w", dockerfileDir, err)
	}

	attachables, err := buildSessionAttachables(req.BuildSecrets, req.BuildSSH)
	if err != nil {
		return nil, err
	}

	exportDir, err := os.MkdirTemp(filepath.Join(inst.cacheDir, "buildkit"), "export-oci-*")
	if err != nil {
		return nil, fmt.Errorf("create oci export dir: %w", err)
	}
	defer os.RemoveAll(exportDir)

	solveResp, err := bk.Solve(ctx, client.SolveOpt{
		Frontend: "dockerfile.v0",
		FrontendAttrs: buildFrontendAttrs(req, dockerfileName),
		LocalMounts: map[string]fsutil.FS{
			"context":    contextFS,
			"dockerfile": dockerfileFS,
		},
		Session: attachables,
		Exports: []client.ExportEntry{{
			Type:      client.ExporterOCI,
			OutputDir: exportDir,
		}},
	})
	if err != nil {
		return nil, fmt.Errorf("buildkit solve: %w", err)
	}

	exportedImage, err := oci.LoadLayoutImage(exportDir)
	if err != nil {
		return nil, fmt.Errorf("load buildkit export: %w", err)
	}
	if err := exportedImage.ExtractToDir(req.OutputDir); err != nil {
		return nil, fmt.Errorf("extract buildkit export: %w", err)
	}

	config := exportedImage.Config
	if rawConfig := pickImageConfigMetadata(solveResp.ExporterResponse); rawConfig != "" {
		if parsed, err := oci.ImageConfigFromJSON([]byte(rawConfig)); err == nil {
			config = parsed
		}
	}

	return &buildbackend.Result{
		RootfsDir: req.OutputDir,
		Config:    config,
	}, nil
}

type buildkitClient interface {
	Wait(context.Context) error
	Solve(context.Context, client.SolveOpt) (*client.SolveResponse, error)
	Close() error
}

type realBuildkitClient struct {
	*client.Client
}

func (c *realBuildkitClient) Solve(ctx context.Context, opt client.SolveOpt) (*client.SolveResponse, error) {
	return c.Client.Solve(ctx, nil, opt, nil)
}

func newBuildkitClient(ctx context.Context, dial func(context.Context) (net.Conn, error)) (buildkitClient, error) {
	cl, err := client.New(
		ctx,
		"unix:///var/run/buildkit/buildkitd.sock",
		client.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return dial(ctx)
		}),
	)
	if err != nil {
		return nil, err
	}
	return &realBuildkitClient{Client: cl}, nil
}

type manager struct {
	mu       sync.Mutex
	builders map[string]*builderInstance
}

func sharedManager() *manager {
	key := resolvedCacheDir("")
	if existing, ok := defaultManagers.Load(key); ok {
		return existing.(*manager)
	}
	created := &manager{builders: map[string]*builderInstance{}}
	actual, _ := defaultManagers.LoadOrStore(key, created)
	return actual.(*manager)
}

func (m *manager) ensure(ctx context.Context, cacheDir string) (*builderInstance, error) {
	cacheDir = resolvedCacheDir(cacheDir)
	m.mu.Lock()
	inst := m.builders[cacheDir]
	if inst == nil {
		inst = &builderInstance{cacheDir: cacheDir}
		m.builders[cacheDir] = inst
	}
	m.mu.Unlock()
	if err := inst.ensure(ctx); err != nil {
		m.invalidate(cacheDir, inst)
		return nil, err
	}
	return inst, nil
}

func (m *manager) invalidate(cacheDir string, inst *builderInstance) {
	cacheDir = resolvedCacheDir(cacheDir)
	m.mu.Lock()
	current := m.builders[cacheDir]
	if current == inst {
		delete(m.builders, cacheDir)
	}
	m.mu.Unlock()
	if current == inst {
		inst.shutdown()
	}
}

type builderInstance struct {
	mu        sync.Mutex
	cacheDir  string
	workDir   string
	stateDir  string
	kernel    string
	vm        *vmm.VM
}

func (b *builderInstance) ensure(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.vm != nil {
		if err := b.waitReadyLocked(ctx, builderProbeTimeout); err == nil {
			return nil
		}
		b.shutdownLocked()
	}

	workDir, stateDir, kernelPath, err := prepareBuilderArtifacts(b.cacheDir)
	if err != nil {
		return err
	}
	vm, err := bootBuilderVM(workDir, stateDir, kernelPath)
	if err != nil {
		return err
	}

	b.workDir = workDir
	b.stateDir = stateDir
	b.kernel = kernelPath
	b.vm = vm
	if err := b.waitReadyLocked(ctx, builderReadyTimeout); err != nil {
		console := strings.TrimSpace(string(vm.ConsoleOutput()))
		b.shutdownLocked()
		if console != "" {
			return fmt.Errorf("wait for buildkit helper: %w\n%s", err, console)
		}
		return fmt.Errorf("wait for buildkit helper: %w", err)
	}
	return nil
}

func (b *builderInstance) shutdown() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.shutdownLocked()
}

func (b *builderInstance) shutdownLocked() {
	if b.vm != nil {
		b.vm.Stop()
		b.vm = nil
	}
}

func (b *builderInstance) waitReadyLocked(ctx context.Context, timeout time.Duration) error {
	deadlineCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var lastErr error
	for {
		probeCtx, probeCancel := context.WithTimeout(deadlineCtx, builderProbeTimeout)
		err := b.probeLocked(probeCtx)
		probeCancel()
		if err == nil {
			return nil
		}
		lastErr = err
		select {
		case <-deadlineCtx.Done():
			if lastErr != nil {
				return lastErr
			}
			return deadlineCtx.Err()
		case <-time.After(builderProbeInterval):
		}
	}
}

func (b *builderInstance) probeLocked(ctx context.Context) error {
	bk, err := newBuildkitClient(ctx, b.dialBuildkit)
	if err != nil {
		return err
	}
	defer bk.Close()
	return bk.Wait(ctx)
}

func (b *builderInstance) dialBuildkit(ctx context.Context) (net.Conn, error) {
	b.mu.Lock()
	vm := b.vm
	b.mu.Unlock()
	if vm == nil {
		return nil, fmt.Errorf("buildkit helper VM is not running")
	}
	return startExecStream(ctx, vm, []string{"buildctl", "--addr", "unix://" + builderSocketGuestPath, "dial-stdio"})
}

func prepareBuilderArtifacts(cacheDir string) (string, string, string, error) {
	kernelPath, err := resolveBuilderKernelPath()
	if err != nil {
		return "", "", "", err
	}
	workDir := filepath.Join(cacheDir, "buildkit", "vm")
	stateDir := filepath.Join(cacheDir, "buildkit", "state")
	rootfsDir := filepath.Join(workDir, "rootfs")
	diskPath := filepath.Join(workDir, "disk.ext4")
	initrdPath := filepath.Join(workDir, "initrd.img")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return "", "", "", fmt.Errorf("create buildkit workdir: %w", err)
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return "", "", "", fmt.Errorf("create buildkit state dir: %w", err)
	}

	if _, err := os.Stat(diskPath); err == nil {
		if _, err := os.Stat(initrdPath); err == nil {
			_ = os.Remove(filepath.Join(stateDir, "buildkitd.sock"))
			return workDir, stateDir, kernelPath, nil
		}
	}

	pulled, err := oci.Pull(oci.PullOptions{
		Ref:      builderImageRef(),
		OS:       "linux",
		Arch:     runtime.GOARCH,
		CacheDir: filepath.Join(cacheDir, "layers"),
	})
	if err != nil {
		return "", "", "", fmt.Errorf("pull buildkit image %s: %w", builderImageRef(), err)
	}

	if err := os.RemoveAll(rootfsDir); err != nil {
		return "", "", "", fmt.Errorf("reset buildkit rootfs: %w", err)
	}
	if err := os.MkdirAll(rootfsDir, 0o755); err != nil {
		return "", "", "", fmt.Errorf("create buildkit rootfs: %w", err)
	}
	if err := pulled.ExtractToDir(rootfsDir); err != nil {
		return "", "", "", fmt.Errorf("extract buildkit image: %w", err)
	}

	guestSpec := runtimecfg.GuestSpec{
		Process: runtimecfg.ResolveProcess(nonEmptySlice(pulled.Config.Entrypoint, []string{"buildkitd"}), []string{
			"--addr", "unix://" + builderSocketGuestPath,
			"--root", builderStateMountPath,
		}),
		Env:     append([]string(nil), pulled.Config.Env...),
		SharedFS: []runtimecfg.SharedFSMount{{
			Tag:    builderStateMountTag,
			Target: builderStateMountPath,
		}},
		WorkDir: pulled.Config.WorkingDir,
		User:    pulled.Config.User,
		Exec: runtimecfg.ExecConfig{
			Enabled:   true,
			VsockPort: runtimecfg.DefaultExecVsockPort,
		},
	}
	if err := guest.BuildInitrdWithOptions(initrdPath, guest.InitrdOptions{
		RuntimeSpec: &guestSpec,
	}); err != nil {
		return "", "", "", fmt.Errorf("build buildkit initrd: %w", err)
	}
	if err := oci.BuildExt4(rootfsDir, diskPath, builderDiskMB); err != nil {
		return "", "", "", fmt.Errorf("build buildkit disk: %w", err)
	}
	_ = os.Remove(filepath.Join(stateDir, "buildkitd.sock"))
	return workDir, stateDir, kernelPath, nil
}

func bootBuilderVM(workDir, stateDir, kernelPath string) (*vmm.VM, error) {
	diskPath := filepath.Join(workDir, "disk.ext4")
	initrdPath := filepath.Join(workDir, "initrd.img")
	cmdline := strings.Join(append(runtimecfg.DarwinKernelArgsForRuntime(true, false), "rw", "root=/dev/vda", "rootfstype=ext4"), " ")
	vm, err := vmm.New(vmm.Config{
		ID:         "buildkit-" + shortStableID(workDir),
		MemMB:      builderMemMB,
		Arch:       runtime.GOARCH,
		VCPUs:      builderCPUCount,
		KernelPath: kernelPath,
		InitrdPath: initrdPath,
		Cmdline:    cmdline,
		DiskImage:  diskPath,
		Network: &vmm.NetworkConfig{
			Mode: vmm.NetworkAttachmentNAT,
		},
		Metadata: map[string]string{
			"gocracker_internal_role": "darwin-buildkit",
		},
		SharedFS: []vmm.SharedFSConfig{{
			Source: stateDir,
			Tag:    builderStateMountTag,
		}},
		Vsock:      &vmm.VsockConfig{Enabled: true},
		Exec:       &vmm.ExecConfig{Enabled: true, VsockPort: runtimecfg.DefaultExecVsockPort},
		ConsoleOut: io.Discard,
	})
	if err != nil {
		return nil, fmt.Errorf("create buildkit helper vm: %w", err)
	}
	if err := vm.Start(); err != nil {
		vm.Stop()
		return nil, fmt.Errorf("start buildkit helper vm: %w", err)
	}
	return vm, nil
}

func startExecStream(_ context.Context, vm *vmm.VM, command []string) (net.Conn, error) {
	if vm == nil {
		return nil, fmt.Errorf("buildkit helper VM is nil")
	}
	conn, err := vm.DialVsock(runtimecfg.DefaultExecVsockPort)
	if err != nil {
		return nil, err
	}
	if err := guestexec.Encode(conn, guestexec.Request{
		Mode:    guestexec.ModeStream,
		Command: append([]string(nil), command...),
		Columns: 120,
		Rows:    40,
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

func buildFrontendAttrs(req buildbackend.Request, dockerfileName string) map[string]string {
	attrs := map[string]string{
		"filename": dockerfileName,
	}
	if strings.TrimSpace(req.Target) != "" {
		attrs["target"] = strings.TrimSpace(req.Target)
	}
	if strings.TrimSpace(req.Platform) != "" {
		attrs["platform"] = strings.TrimSpace(req.Platform)
	}
	if req.NoCache {
		attrs["no-cache"] = ""
	}
	keys := make([]string, 0, len(req.BuildArgs))
	for key := range req.BuildArgs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		attrs["build-arg:"+key] = req.BuildArgs[key]
	}
	return attrs
}

func buildSessionAttachables(secretSpecs, sshSpecs []string) ([]session.Attachable, error) {
	attachables := []session.Attachable{
		authprovider.NewDockerAuthProvider(authprovider.DockerAuthProviderConfig{}),
	}
	if len(secretSpecs) > 0 {
		secrets, err := parseSecretSpecs(secretSpecs)
		if err != nil {
			return nil, err
		}
		store, err := secretsprovider.NewStore(secrets)
		if err != nil {
			return nil, fmt.Errorf("parse build secrets: %w", err)
		}
		attachables = append(attachables, secretsprovider.NewSecretProvider(store))
	}
	if len(sshSpecs) > 0 {
		sshAgents, err := parseSSHSpecs(sshSpecs)
		if err != nil {
			return nil, err
		}
		provider, err := sshprovider.NewSSHAgentProvider(sshAgents)
		if err != nil {
			return nil, fmt.Errorf("parse build ssh: %w", err)
		}
		attachables = append(attachables, provider)
	}
	return attachables, nil
}

func parseSecretSpecs(specs []string) ([]secretsprovider.Source, error) {
	out := make([]secretsprovider.Source, 0, len(specs))
	for _, spec := range specs {
		fields, err := csvvalue.Fields(spec, nil)
		if err != nil {
			return nil, fmt.Errorf("parse build secret %q: %w", spec, err)
		}
		var src secretsprovider.Source
		var typ string
		for _, field := range fields {
			key, value, ok := strings.Cut(field, "=")
			if !ok {
				return nil, fmt.Errorf("parse build secret %q: invalid field %q", spec, field)
			}
			switch strings.ToLower(strings.TrimSpace(key)) {
			case "type":
				typ = strings.TrimSpace(value)
			case "id":
				src.ID = strings.TrimSpace(value)
			case "source", "src":
				src.FilePath = strings.TrimSpace(value)
			case "env":
				src.Env = strings.TrimSpace(value)
			default:
				return nil, fmt.Errorf("parse build secret %q: unexpected key %q", spec, key)
			}
		}
		if typ != "" && typ != "file" && typ != "env" {
			return nil, fmt.Errorf("parse build secret %q: unsupported type %q", spec, typ)
		}
		if typ == "env" && src.Env == "" {
			src.Env = src.FilePath
			src.FilePath = ""
		}
		out = append(out, src)
	}
	return out, nil
}

func parseSSHSpecs(specs []string) ([]sshprovider.AgentConfig, error) {
	out := make([]sshprovider.AgentConfig, 0, len(specs))
	for _, spec := range specs {
		id, value, ok := strings.Cut(spec, "=")
		cfg := sshprovider.AgentConfig{ID: strings.TrimSpace(id)}
		if !ok {
			if cfg.ID == "" {
				cfg.ID = sshforward.DefaultID
			}
			out = append(out, cfg)
			continue
		}
		for _, part := range strings.Split(value, ",") {
			key, raw, hasKV := strings.Cut(part, "=")
			if hasKV && strings.TrimSpace(key) == "raw" {
				cfg.Raw = strings.EqualFold(strings.TrimSpace(raw), "true")
				continue
			}
			cfg.Paths = append(cfg.Paths, strings.TrimSpace(part))
		}
		if cfg.ID == "" {
			cfg.ID = sshforward.DefaultID
		}
		out = append(out, cfg)
	}
	return out, nil
}

func pickImageConfigMetadata(meta map[string]string) string {
	if len(meta) == 0 {
		return ""
	}
	if raw := strings.TrimSpace(meta[exptypes.ExporterImageConfigKey]); raw != "" {
		return raw
	}
	prefix := exptypes.ExporterImageConfigKey + "/"
	keys := make([]string, 0, len(meta))
	for key := range meta {
		if strings.HasPrefix(key, prefix) && strings.TrimSpace(meta[key]) != "" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return ""
	}
	return strings.TrimSpace(meta[keys[0]])
}

func resolvedCacheDir(cacheDir string) string {
	cacheDir = strings.TrimSpace(cacheDir)
	if cacheDir != "" {
		return cacheDir
	}
	return filepath.Join(os.TempDir(), "gocracker", "cache")
}

func resolveBuilderKernelPath() (string, error) {
	if envPath := strings.TrimSpace(os.Getenv(builderKernelEnvKey)); envPath != "" {
		abs, err := filepath.Abs(envPath)
		if err != nil {
			return "", fmt.Errorf("resolve %s: %w", builderKernelEnvKey, err)
		}
		if _, err := os.Stat(abs); err != nil {
			return "", fmt.Errorf("builder kernel %s: %w", abs, err)
		}
		return abs, nil
	}
	candidates := []string{}
	if cwd, err := getwdFunc(); err == nil {
		candidates = append(candidates, filepath.Join(cwd, "artifacts", "kernels", "gocracker-guest-standard-arm64-Image"))
	}
	if execPath, err := executableFunc(); err == nil {
		execDir := filepath.Dir(execPath)
		candidates = append(candidates,
			filepath.Join(execDir, "artifacts", "kernels", "gocracker-guest-standard-arm64-Image"),
			filepath.Join(execDir, "..", "artifacts", "kernels", "gocracker-guest-standard-arm64-Image"),
		)
	}
	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil {
			abs, _ := filepath.Abs(candidate)
			return abs, nil
		}
	}
	return "", fmt.Errorf("darwin buildkit kernel not found; set %s or place artifacts/kernels/gocracker-guest-standard-arm64-Image next to the repo or binary", builderKernelEnvKey)
}

func builderImageRef() string {
	if value := strings.TrimSpace(os.Getenv(builderImageEnvKey)); value != "" {
		return value
	}
	return defaultBuilderImageRef
}

func shortStableID(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:8])
}

func nonEmptySlice(value, fallback []string) []string {
	if len(value) > 0 {
		return append([]string(nil), value...)
	}
	return append([]string(nil), fallback...)
}
