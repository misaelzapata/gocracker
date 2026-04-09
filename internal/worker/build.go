package worker

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gocracker/gocracker/internal/buildserver"
	"github.com/gocracker/gocracker/internal/oci"
)

type BuildOptions struct {
	JailerBinary string
	WorkerBinary string
	UID          int
	GID          int
	ChrootBase   string
}

func BuildRootfs(req buildserver.BuildRequest, opts BuildOptions) (oci.ImageConfig, error) {
	if opts.ChrootBase == "" {
		opts.ChrootBase = DefaultChrootBaseDir()
	}
	jailerExec, jailerPrefix, err := resolveLauncher(opts.JailerBinary, "jailer")
	if err != nil {
		return oci.ImageConfig{}, err
	}
	workerExec, workerArgsPrefix, err := resolveLauncher(opts.WorkerBinary, "build-worker")
	if err != nil {
		return oci.ImageConfig{}, err
	}
	if req.OutputDir == "" {
		return oci.ImageConfig{}, fmt.Errorf("build worker output dir is required")
	}

	runDir, err := os.MkdirTemp("", "gocracker-build-worker-*")
	if err != nil {
		return oci.ImageConfig{}, err
	}
	socketHostPath := filepath.Join(runDir, "build.sock")
	cleanup := func() { _ = os.RemoveAll(runDir) }
	defer cleanup()

	jailedReq := req
	outputParent := filepath.Dir(req.OutputDir)
	outputBase := filepath.Base(req.OutputDir)
	if err := os.MkdirAll(outputParent, 0755); err != nil {
		return oci.ImageConfig{}, fmt.Errorf("create build output parent %s: %w", outputParent, err)
	}
	mounts := []string{
		"rw:" + runDir + ":/worker",
		"rw:" + outputParent + ":/worker/out-parent",
	}
	jailedReq.OutputDir = filepath.Join("/worker/out-parent", outputBase)
	jailedReq, mounts, err = rewriteBuildCacheDirForJail(jailedReq, mounts)
	if err != nil {
		return oci.ImageConfig{}, err
	}
	if req.Context != "" {
		mounts = append(mounts, "ro:"+req.Context+":/input/context")
		jailedReq.Context = "/input/context"
	}
	if req.Dockerfile != "" {
		mounts = append(mounts, "ro:"+req.Dockerfile+":/input/Dockerfile")
		jailedReq.Dockerfile = "/input/Dockerfile"
	}
	mounts = append(mounts, buildSupportMounts()...)
	env := buildWorkerEnv()

	jailerArgs := []string{
		"--id", fmt.Sprintf("build-%d", time.Now().UnixNano()%100000),
		"--uid", fmt.Sprintf("%d", firstNonNegative(opts.UID, os.Getuid())),
		"--gid", fmt.Sprintf("%d", firstNonNegative(opts.GID, os.Getgid())),
		"--exec-file", workerExec,
	}
	if opts.ChrootBase != "" {
		jailerArgs = append(jailerArgs, "--chroot-base-dir", opts.ChrootBase)
	}
	for _, mount := range mounts {
		jailerArgs = append(jailerArgs, "--mount", mount)
	}
	for _, entry := range env {
		jailerArgs = append(jailerArgs, "--env", entry)
	}
	jailerArgs = append(jailerArgs, "--")
	jailerArgs = append(jailerArgs, workerArgsPrefix...)
	jailerArgs = append(jailerArgs, "--socket", "/worker/build.sock")

	cmd := exec.Command(jailerExec, append(jailerPrefix, jailerArgs...)...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	if err := cmd.Start(); err != nil {
		return oci.ImageConfig{}, fmt.Errorf("start build jailer: %w", err)
	}
	waitErrCh := make(chan error, 1)
	go func() { waitErrCh <- cmd.Wait() }()

	if exited, err := waitForSocketOrExit(socketHostPath, socketWaitMax, waitErrCh); err != nil {
		if !exited {
			_ = cmd.Process.Kill()
			<-waitErrCh
		}
		return oci.ImageConfig{}, err
	}

	client := buildserver.NewClient(socketHostPath)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	resp, err := client.Build(ctx, jailedReq)
	_ = cmd.Process.Kill()
	<-waitErrCh
	if err != nil {
		return oci.ImageConfig{}, err
	}
	return resp.Config, nil
}

func rewriteBuildCacheDirForJail(req buildserver.BuildRequest, mounts []string) (buildserver.BuildRequest, []string, error) {
	if strings.TrimSpace(req.CacheDir) == "" {
		return req, mounts, nil
	}
	if err := os.MkdirAll(req.CacheDir, 0755); err != nil {
		return req, mounts, fmt.Errorf("create cache dir %s: %w", req.CacheDir, err)
	}
	mounts = append(mounts, "rw:"+req.CacheDir+":/worker/cache")
	req.CacheDir = "/worker/cache"
	return req, mounts, nil
}

func buildSupportMounts() []string {
	candidates := []string{
		"/etc/resolv.conf",
		"/etc/hosts",
		"/etc/nsswitch.conf",
		"/etc/passwd",
		"/etc/group",
		"/etc/ssl/certs",
		"/etc/ca-certificates",
		"/usr/share/ca-certificates",
		"/etc/pki",
	}
	mounts := make([]string, 0, len(candidates)+2)
	for _, path := range candidates {
		if _, err := os.Stat(path); err == nil {
			mounts = append(mounts, "ro:"+path+":"+path)
		}
	}
	if home := os.Getenv("HOME"); home != "" {
		dockerDir := filepath.Join(home, ".docker")
		if _, err := os.Stat(dockerDir); err == nil {
			mounts = append(mounts, "ro:"+dockerDir+":"+dockerDir)
		}
	}
	if dockerConfig := os.Getenv("DOCKER_CONFIG"); dockerConfig != "" {
		if _, err := os.Stat(dockerConfig); err == nil {
			mounts = append(mounts, "ro:"+dockerConfig+":"+dockerConfig)
		}
	}
	mounts = append(mounts, buildToolMounts("cp", "proot", "fakechroot")...)
	return mounts
}

func buildWorkerEnv() []string {
	env := []string{}
	for _, key := range []string{"HOME", "DOCKER_CONFIG", "PATH"} {
		if value := os.Getenv(key); value != "" {
			env = append(env, key+"="+value)
		}
	}
	return env
}

func buildToolMounts(names ...string) []string {
	mounts := []string{}
	seen := map[string]struct{}{}
	for _, name := range names {
		path, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		for _, mount := range binaryMounts(path) {
			if _, ok := seen[mount]; ok {
				continue
			}
			seen[mount] = struct{}{}
			mounts = append(mounts, mount)
		}
	}
	return mounts
}

func binaryMounts(path string) []string {
	mounts := []string{"ro:" + path + ":" + path}
	cmd := exec.Command("ldd", path)
	output, err := cmd.Output()
	if err != nil {
		return mounts
	}
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		dep := ""
		if idx := strings.Index(line, "=>"); idx >= 0 {
			rest := strings.TrimSpace(line[idx+2:])
			if fields := strings.Fields(rest); len(fields) > 0 && strings.HasPrefix(fields[0], "/") {
				dep = fields[0]
			}
		} else {
			fields := strings.Fields(line)
			if len(fields) > 0 && strings.HasPrefix(fields[0], "/") {
				dep = fields[0]
			}
		}
		if dep == "" {
			continue
		}
		if _, err := os.Stat(dep); err == nil {
			mounts = append(mounts, "ro:"+dep+":"+dep)
		}
	}
	return mounts
}
