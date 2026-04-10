//go:build linux

package dockerfile

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	dfinstructions "github.com/moby/buildkit/frontend/dockerfile/instructions"

	"github.com/gocracker/gocracker/internal/hostguard"
	"github.com/gocracker/gocracker/internal/usercfg"
	"golang.org/x/sys/unix"
)

// upsertEnv inserts or replaces a KEY=VALUE entry in a docker-style env slice.
// Used by the privileged build helper to inject HOME/USER/LOGNAME after a
// USER switch (without losing whatever the Dockerfile already set via ENV).
func upsertEnv(env []string, key, value string) []string {
	prefix := key + "="
	for i, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}

const (
	rootlessHelperEnv       = "GOCRACKER_ROOTLESS_RUN"
	rootlessHelperConfigEnv = "GOCRACKER_ROOTLESS_RUN_CONFIG"
	privilegedHelperEnv     = "GOCRACKER_PRIVILEGED_RUN"
	privilegedHelperConfig  = "GOCRACKER_PRIVILEGED_RUN_CONFIG"
)

var errRootlessUserUnsupported = errors.New("rootless RUN with non-root USER requires privileged build or external compatibility tools")

type isolatedRunSpec struct {
	Rootfs string             `json:"rootfs"`
	Args   []string           `json:"args"`
	Env    []string           `json:"env"`
	User   string             `json:"user,omitempty"`
	Mounts []isolatedRunMount `json:"mounts,omitempty"`
}

type isolatedRunMount struct {
	Type      string `json:"type"`
	Source    string `json:"source,omitempty"`
	Target    string `json:"target"`
	ReadOnly  bool   `json:"read_only,omitempty"`
	SizeLimit int64  `json:"size_limit,omitempty"`
	IsDir     bool   `json:"is_dir,omitempty"`
}

func init() {
	switch {
	case os.Getenv(rootlessHelperEnv) == "1":
		code, err := runRootlessHelper()
		if err != nil {
			fmt.Fprintf(os.Stderr, "[build] rootless RUN failed: %v\n", err)
			os.Exit(1)
		}
		os.Exit(code)
	case os.Getenv(privilegedHelperEnv) == "1":
		code, err := runPrivilegedHelper()
		if err != nil {
			fmt.Fprintf(os.Stderr, "[build] privileged RUN failed: %v\n", err)
			os.Exit(1)
		}
		os.Exit(code)
	}
}

func (b *builder) runRootless(args, envArgs []string) error {
	if b.user != "" {
		resolved, err := usercfg.Resolve(b.rootfs, b.user)
		if err != nil {
			return err
		}
		if resolved.UID != 0 || resolved.GID != 0 || len(resolved.Groups) > 0 {
			return errRootlessUserUnsupported
		}
	}

	cleanupFns, err := b.prepareRootlessFiles()
	if err != nil {
		return err
	}
	defer func() {
		for i := len(cleanupFns) - 1; i >= 0; i-- {
			cleanupFns[i]()
		}
	}()

	spec := isolatedRunSpec{
		Rootfs: b.rootfs,
		Args:   append([]string(nil), args...),
		Env:    append([]string(nil), envArgs...),
	}
	cfgPath, err := writeIsolatedConfig(spec)
	if err != nil {
		return err
	}
	defer os.RemoveAll(filepath.Dir(cfgPath))

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve current executable: %w", err)
	}

	cmd := exec.Command(exe)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	cmd.Env = append(os.Environ(),
		rootlessHelperEnv+"=1",
		rootlessHelperConfigEnv+"="+cfgPath,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:                 unix.CLONE_NEWUSER | unix.CLONE_NEWNS,
		UidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappingsEnableSetgroups: false,
		GidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
		Credential:                 &syscall.Credential{Uid: 0, Gid: 0},
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("rootless user namespace runner: %w", err)
	}
	return nil
}

func (b *builder) runPrivileged(args, envArgs []string, mounts []RunMount) error {
	spec := isolatedRunSpec{
		Rootfs: b.rootfs,
		Args:   append([]string(nil), args...),
		Env:    append([]string(nil), envArgs...),
		User:   b.user,
	}
	resolved, err := b.resolveIsolatedRunMounts(mounts)
	if err != nil {
		return err
	}
	spec.Mounts = resolved

	cfgPath, err := writeIsolatedConfig(spec)
	if err != nil {
		return err
	}
	defer os.RemoveAll(filepath.Dir(cfgPath))

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve current executable: %w", err)
	}

	cmd := exec.Command(exe)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	cmd.Env = append(os.Environ(),
		privilegedHelperEnv+"=1",
		privilegedHelperConfig+"="+cfgPath,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: unix.CLONE_NEWNS,
		Credential: &syscall.Credential{Uid: 0, Gid: 0},
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("privileged mount namespace runner: %w", err)
	}
	return nil
}

func (b *builder) prepareRootlessFiles() ([]func(), error) {
	cleanupFns := make([]func(), 0, 2)
	for _, path := range []string{"/etc/resolv.conf", "/etc/hosts"} {
		cleanup, err := b.injectHostFile(path)
		if err != nil {
			for i := len(cleanupFns) - 1; i >= 0; i-- {
				cleanupFns[i]()
			}
			return nil, err
		}
		cleanupFns = append(cleanupFns, cleanup)
	}
	return cleanupFns, nil
}

func writeIsolatedConfig(spec isolatedRunSpec) (string, error) {
	dir, err := os.MkdirTemp("", "gocracker-runner-*")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "config.json")
	data, err := json.Marshal(spec)
	if err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	return path, nil
}

// resolveSecretSource looks up a build-time secret by id under the directory
// pointed to by GOCRACKER_BUILD_SECRETS_DIR. It returns "" without error when
// the file is missing so the caller can decide based on the mount's
// Required=true|false. Env-var-based secret resolution is intentionally NOT
// supported because env vars leak through /proc/<pid>/environ to other
// processes of the same UID; secrets must be materialized to a file with
// 0600 permissions instead.
func resolveSecretSource(id string) (string, error) {
	if id == "" {
		return "", nil
	}
	// Reject path traversal: id must be a single filename component.
	if strings.ContainsAny(id, "/\\") || id == "." || id == ".." {
		return "", fmt.Errorf("invalid secret id %q", id)
	}
	dir := os.Getenv("GOCRACKER_BUILD_SECRETS_DIR")
	if dir == "" {
		return "", nil
	}
	if dirInfo, err := os.Stat(dir); err == nil {
		if mode := dirInfo.Mode().Perm(); mode&0o077 != 0 {
			return "", fmt.Errorf("GOCRACKER_BUILD_SECRETS_DIR %s has insecure permissions %#o (must be at most 0700)", dir, mode)
		}
	}
	candidate := filepath.Join(dir, id)
	info, err := os.Stat(candidate)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("secret %q must be a file, found directory", id)
	}
	// Refuse world- or group-readable secret files; this prevents an
	// accidentally-permissive secret from being mounted into a build that
	// then leaks it through layer caches.
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		return "", fmt.Errorf("secret %q has insecure permissions %#o (must be at most 0600)", id, mode)
	}
	return candidate, nil
}

func (b *builder) resolveIsolatedRunMounts(mounts []RunMount) ([]isolatedRunMount, error) {
	if len(mounts) == 0 {
		return nil, nil
	}
	out := make([]isolatedRunMount, 0, len(mounts))
	for _, mount := range mounts {
		target := b.resolveContainerPath(b.expand(mount.Target))
		switch mount.Type {
		case string(dfinstructions.MountTypeBind):
			// BuildKit allows omitting target=; the default is the same path
			// as source= (or "/" when source is missing). Mirror that here so
			// `RUN --mount=type=bind,source=foo cmd` works.
			if mount.Target == "" {
				if mount.Source != "" {
					mount.Target = mount.Source
				} else {
					mount.Target = "/"
				}
				target = b.resolveContainerPath(b.expand(mount.Target))
			}
			source, err := b.resolveBindRunMountSource(mount)
			if err != nil {
				return nil, err
			}
			info, err := os.Stat(source)
			if err != nil {
				return nil, err
			}
			out = append(out, isolatedRunMount{
				Type:     mount.Type,
				Source:   source,
				Target:   target,
				ReadOnly: mount.ReadOnly,
				IsDir:    info.IsDir(),
			})
		case string(dfinstructions.MountTypeCache):
			if mount.Target == "" {
				return nil, fmt.Errorf("RUN --mount=type=cache requires target=")
			}
			cacheKey := mount.CacheID
			if cacheKey == "" {
				cacheKey = b.expand(mount.Target)
			}
			cacheDir := filepath.Join(b.runCacheRoot, sanitizeRunMountCacheKey(cacheKey))
			if err := os.MkdirAll(cacheDir, 0755); err != nil {
				return nil, err
			}
			out = append(out, isolatedRunMount{
				Type:     mount.Type,
				Source:   cacheDir,
				Target:   target,
				ReadOnly: mount.ReadOnly,
				IsDir:    true,
			})
		case string(dfinstructions.MountTypeTmpfs):
			if mount.Target == "" {
				return nil, fmt.Errorf("RUN --mount=type=tmpfs requires target=")
			}
			out = append(out, isolatedRunMount{
				Type:      mount.Type,
				Target:    target,
				SizeLimit: mount.SizeLimit,
				IsDir:     true,
			})
		case string(dfinstructions.MountTypeSecret):
			// Resolve secret to a real file:
			//   1. $GOCRACKER_BUILD_SECRETS_DIR/<id> if set
			//   2. Otherwise mount an empty file at the target so optional
			//      secrets do not break the build (BuildKit's `required=false`
			//      semantics).
			//   3. If `required=true` AND no source file is found, fail loudly.
			//
			// Default target: /run/secrets/<id> (matches BuildKit).
			id := mount.SecretID
			if id == "" {
				id = filepath.Base(strings.TrimSpace(mount.Target))
			}
			if mount.Target == "" {
				if id == "" {
					return nil, fmt.Errorf("RUN --mount=type=secret requires either id= or target=")
				}
				mount.Target = "/run/secrets/" + id
				target = b.resolveContainerPath(b.expand(mount.Target))
			}
			source, err := resolveSecretSource(id)
			if err != nil {
				return nil, err
			}
			if source == "" {
				if mount.Required {
					return nil, fmt.Errorf("RUN --mount=type=secret id=%q is required but no source file found in $GOCRACKER_BUILD_SECRETS_DIR", id)
				}
				// Materialize an empty file in the run cache root so the
				// bind mount has something to point at. Mode 0400 mirrors
				// BuildKit's default secret permissions.
				emptyDir := filepath.Join(b.runCacheRoot, "_secrets-empty")
				if err := os.MkdirAll(emptyDir, 0o755); err != nil {
					return nil, err
				}
				emptyFile := filepath.Join(emptyDir, sanitizeRunMountCacheKey(id))
				if err := os.WriteFile(emptyFile, nil, 0o400); err != nil {
					return nil, err
				}
				source = emptyFile
			}
			info, err := os.Stat(source)
			if err != nil {
				return nil, err
			}
			out = append(out, isolatedRunMount{
				Type:     string(dfinstructions.MountTypeBind),
				Source:   source,
				Target:   target,
				ReadOnly: true,
				IsDir:    info.IsDir(),
			})
		case string(dfinstructions.MountTypeSSH):
			// SSH agent forwarding is not supported. If the build does not
			// strictly require it (Required=false) we mount an empty file
			// so the RUN can short-circuit gracefully.
			if mount.Required {
				return nil, fmt.Errorf("RUN --mount=type=ssh is not supported in this builder")
			}
			if mount.Target == "" {
				mount.Target = "/run/buildkit/ssh_agent.0"
				target = b.resolveContainerPath(b.expand(mount.Target))
			}
			emptyDir := filepath.Join(b.runCacheRoot, "_ssh-empty")
			if err := os.MkdirAll(emptyDir, 0o755); err != nil {
				return nil, err
			}
			emptyFile := filepath.Join(emptyDir, "sock")
			if err := os.WriteFile(emptyFile, nil, 0o400); err != nil {
				return nil, err
			}
			out = append(out, isolatedRunMount{
				Type:     string(dfinstructions.MountTypeBind),
				Source:   emptyFile,
				Target:   target,
				ReadOnly: true,
				IsDir:    false,
			})
		default:
			return nil, fmt.Errorf("RUN --mount type %q is not supported yet", mount.Type)
		}
	}
	return out, nil
}

func runRootlessHelper() (int, error) {
	if err := hostguard.CheckHostDevices(hostguard.DeviceRequirements{}); err != nil {
		return 1, fmt.Errorf("host device preflight: %w", err)
	}
	cfgPath := os.Getenv(rootlessHelperConfigEnv)
	if cfgPath == "" {
		return 1, fmt.Errorf("missing %s", rootlessHelperConfigEnv)
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return 1, err
	}
	var spec isolatedRunSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		return 1, err
	}
	if len(spec.Args) == 0 {
		return 1, fmt.Errorf("empty rootless RUN command")
	}

	if err := prepareIsolatedMountNamespace(); err != nil {
		return 1, err
	}

	for _, path := range []string{"/proc", "/dev", "/tmp"} {
		if err := os.MkdirAll(rootfsPath(spec.Rootfs, path), 0755); err != nil {
			return 1, err
		}
	}
	if err := unix.Mount("proc", rootfsPath(spec.Rootfs, "/proc"), "proc", 0, ""); err != nil {
		if bindErr := bindMount("/proc", rootfsPath(spec.Rootfs, "/proc")); bindErr != nil {
			return 1, fmt.Errorf("mount proc: %w (bind fallback: %v)", err, bindErr)
		}
	}
	if err := bindMount("/dev", rootfsPath(spec.Rootfs, "/dev")); err != nil {
		return 1, fmt.Errorf("bind /dev: %w", err)
	}
	if err := bindMount("/tmp", rootfsPath(spec.Rootfs, "/tmp")); err != nil {
		return 1, fmt.Errorf("bind /tmp: %w", err)
	}

	if err := pivotIntoRoot(spec.Rootfs); err != nil {
		return 1, err
	}

	cmd := exec.Command(spec.Args[0], spec.Args[1:]...)
	cmd.Env = spec.Env
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err = cmd.Run()
	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), nil
	}
	return 1, err
}

func runPrivilegedHelper() (int, error) {
	if err := hostguard.CheckHostDevices(hostguard.DeviceRequirements{}); err != nil {
		return 1, fmt.Errorf("host device preflight: %w", err)
	}
	cfgPath := os.Getenv(privilegedHelperConfig)
	if cfgPath == "" {
		return 1, fmt.Errorf("missing %s", privilegedHelperConfig)
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return 1, err
	}
	var spec isolatedRunSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		return 1, err
	}
	if len(spec.Args) == 0 {
		return 1, fmt.Errorf("empty privileged RUN command")
	}

	if err := prepareIsolatedMountNamespace(); err != nil {
		return 1, err
	}
	if err := applyIsolatedRunMounts(spec.Rootfs, spec.Mounts); err != nil {
		return 1, err
	}

	var resolved *usercfg.Resolved
	if spec.User != "" {
		resolvedValue, resolveErr := usercfg.Resolve(spec.Rootfs, spec.User)
		resolved = &resolvedValue
		err = resolveErr
		if err != nil {
			return 1, err
		}
	}

	if err := pivotIntoRoot(spec.Rootfs); err != nil {
		return 1, err
	}
	if err := mountPrivilegedBuildFS("/"); err != nil {
		return 1, err
	}
	env := append([]string(nil), spec.Env...)
	if resolved != nil {
		if len(resolved.Groups) > 0 {
			groups := make([]int, len(resolved.Groups))
			for i, group := range resolved.Groups {
				groups[i] = int(group)
			}
			if err := unix.Setgroups(groups); err != nil {
				return 1, fmt.Errorf("setgroups: %w", err)
			}
		}
		if err := unix.Setgid(int(resolved.GID)); err != nil {
			return 1, fmt.Errorf("setgid: %w", err)
		}
		if err := unix.Setuid(int(resolved.UID)); err != nil {
			return 1, fmt.Errorf("setuid: %w", err)
		}
		// Mirror docker/buildkit: when USER switches to a non-root user the
		// build environment should reflect that user's HOME/USER/LOGNAME so
		// tools like uv, pip, cargo, npm, go modules and friends do not try
		// to write under /root. Without this fix, every Dockerfile that
		// runs `USER python` then `RUN pip install` (the entire nickjj
		// django/flask/rails template family) errors with permission denied.
		home := resolved.Home
		if home == "" {
			home = "/"
		}
		name := resolved.Name
		if name == "" {
			name = strconv.Itoa(int(resolved.UID))
		}
		env = upsertEnv(env, "HOME", home)
		env = upsertEnv(env, "USER", name)
		env = upsertEnv(env, "LOGNAME", name)
		// chdir into HOME if it exists, otherwise stay where the WORKDIR
		// or `RUN cd /xxx` previously placed us. Most builds set WORKDIR
		// explicitly so this only matters when they don't.
		if home != "" {
			if info, err := os.Stat(home); err == nil && info.IsDir() {
				_ = os.Chdir(home)
			}
		}
	}

	cmd := exec.Command(spec.Args[0], spec.Args[1:]...)
	cmd.Env = env
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err = cmd.Run()
	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), nil
	}
	return 1, err
}

func prepareIsolatedMountNamespace() error {
	if err := unix.Mount("", "/", "", unix.MS_SLAVE|unix.MS_REC, ""); err != nil {
		return fmt.Errorf("make mounts slave: %w", err)
	}
	return nil
}

func pivotIntoRoot(rootfs string) error {
	if err := unix.Mount(rootfs, rootfs, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
		return fmt.Errorf("bind rootfs on itself: %w", err)
	}
	if err := os.Chdir(rootfs); err != nil {
		return fmt.Errorf("chdir rootfs: %w", err)
	}
	if err := os.MkdirAll("old_root", 0700); err != nil {
		return fmt.Errorf("mkdir old_root: %w", err)
	}
	if err := unix.PivotRoot(".", "old_root"); err != nil {
		return fmt.Errorf("pivot_root: %w", err)
	}
	if err := os.Chdir("/"); err != nil {
		return fmt.Errorf("chdir /: %w", err)
	}
	if err := unix.Unmount("/old_root", unix.MNT_DETACH); err != nil {
		return fmt.Errorf("umount old_root: %w", err)
	}
	if err := os.Remove("/old_root"); err != nil {
		return fmt.Errorf("remove old_root: %w", err)
	}
	return nil
}

func mountPrivilegedBuildFS(rootfs string) error {
	if err := ensureReadOnlyProc(rootfsPath(rootfs, "/proc")); err != nil {
		return fmt.Errorf("mount readonly /proc: %w", err)
	}
	if err := ensureReadOnlySysfs(rootfsPath(rootfs, "/sys")); err != nil {
		return fmt.Errorf("mount readonly /sys: %w", err)
	}
	if err := ensureMinimalBuildDev(rootfsPath(rootfs, "/dev")); err != nil {
		return fmt.Errorf("mount private /dev: %w", err)
	}
	return nil
}

func ensureReadOnlyProc(target string) error {
	if err := os.MkdirAll(target, 0555); err != nil {
		return err
	}
	flags := uintptr(unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC)
	if err := unix.Mount("proc", target, "proc", flags, ""); err != nil {
		return err
	}
	return unix.Mount("", target, "", flags|unix.MS_REMOUNT|unix.MS_RDONLY, "")
}

func ensureReadOnlySysfs(target string) error {
	if err := os.MkdirAll(target, 0555); err != nil {
		return err
	}
	flags := uintptr(unix.MS_RDONLY | unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC)
	return unix.Mount("sysfs", target, "sysfs", flags, "")
}

func ensureMinimalBuildDev(target string) error {
	if err := os.MkdirAll(target, 0755); err != nil {
		return err
	}
	if err := unix.Mount("tmpfs", target, "tmpfs", unix.MS_NOSUID|unix.MS_NOEXEC, "mode=0755,size=16m"); err != nil {
		return err
	}
	devPts := rootfsPath(filepath.Dir(target), "/dev/pts")
	if err := os.MkdirAll(devPts, 0755); err != nil {
		return err
	}
	if err := unix.Mount("devpts", devPts, "devpts", 0, "newinstance,gid=5,mode=620,ptmxmode=666"); err != nil {
		return err
	}
	for _, device := range []struct {
		path  string
		major uint32
		minor uint32
		mode  uint32
	}{
		{path: rootfsPath(filepath.Dir(target), "/dev/null"), major: 1, minor: 3, mode: 0666},
		{path: rootfsPath(filepath.Dir(target), "/dev/zero"), major: 1, minor: 5, mode: 0666},
		{path: rootfsPath(filepath.Dir(target), "/dev/full"), major: 1, minor: 7, mode: 0666},
		{path: rootfsPath(filepath.Dir(target), "/dev/random"), major: 1, minor: 8, mode: 0666},
		{path: rootfsPath(filepath.Dir(target), "/dev/urandom"), major: 1, minor: 9, mode: 0666},
		{path: rootfsPath(filepath.Dir(target), "/dev/tty"), major: 5, minor: 0, mode: 0666},
	} {
		if err := makeMinimalCharDevice(device.path, device.major, device.minor, device.mode); err != nil {
			return err
		}
	}
	for _, link := range []struct {
		path   string
		target string
	}{
		{path: rootfsPath(filepath.Dir(target), "/dev/ptmx"), target: "/dev/pts/ptmx"},
		{path: rootfsPath(filepath.Dir(target), "/dev/fd"), target: "/proc/self/fd"},
		{path: rootfsPath(filepath.Dir(target), "/dev/stdin"), target: "/proc/self/fd/0"},
		{path: rootfsPath(filepath.Dir(target), "/dev/stdout"), target: "/proc/self/fd/1"},
		{path: rootfsPath(filepath.Dir(target), "/dev/stderr"), target: "/proc/self/fd/2"},
	} {
		if err := ensureSymlink(link.path, link.target); err != nil {
			return err
		}
	}
	return nil
}

func makeMinimalCharDevice(path string, major, minor uint32, mode uint32) error {
	if err := os.RemoveAll(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := unix.Mknod(path, unix.S_IFCHR|mode, int(unix.Mkdev(major, minor))); err != nil {
		return err
	}
	return os.Chmod(path, os.FileMode(mode))
}

func applyIsolatedRunMounts(rootfs string, mounts []isolatedRunMount) error {
	for _, mount := range mounts {
		target, err := prepareIsolatedMountTarget(rootfs, mount.Target, mount.IsDir)
		if err != nil {
			return err
		}
		switch mount.Type {
		case string(dfinstructions.MountTypeBind), string(dfinstructions.MountTypeCache):
			if err := unix.Mount(mount.Source, target, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
				return fmt.Errorf("mount %s -> %s: %w", mount.Source, mount.Target, err)
			}
			if mount.ReadOnly {
				if err := unix.Mount("", target, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY, ""); err != nil {
					return fmt.Errorf("remount %s readonly: %w", mount.Target, err)
				}
			}
		case string(dfinstructions.MountTypeTmpfs):
			data := "mode=1777"
			if mount.SizeLimit > 0 {
				data = fmt.Sprintf("%s,size=%d", data, mount.SizeLimit)
			}
			if err := unix.Mount("tmpfs", target, "tmpfs", 0, data); err != nil {
				return fmt.Errorf("tmpfs mount %s: %w", mount.Target, err)
			}
		default:
			return fmt.Errorf("RUN --mount type %q is not supported yet", mount.Type)
		}
	}
	return nil
}

func prepareIsolatedMountTarget(rootfs, target string, dir bool) (string, error) {
	hostPath := rootfsPath(rootfs, target)
	if dir {
		if err := os.MkdirAll(hostPath, 0755); err != nil {
			return "", err
		}
		return hostPath, nil
	}
	if err := os.MkdirAll(filepath.Dir(hostPath), 0755); err != nil {
		return "", err
	}
	f, err := os.OpenFile(hostPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return "", err
	}
	_ = f.Close()
	return hostPath, nil
}

func bindMount(src, dst string) error {
	if err := unix.Mount(src, dst, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
		return err
	}
	return nil
}

func rootlessErrorMessage(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, errRootlessUserUnsupported) {
		return err.Error()
	}
	return strings.TrimSpace(err.Error())
}
