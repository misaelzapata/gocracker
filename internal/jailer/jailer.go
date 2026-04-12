//go:build linux

package jailer

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

const (
	defaultChrootBaseDir = "/srv/jailer"
	maxVMIDLen           = 64
)

type multiFlag []string

func (f *multiFlag) String() string { return strings.Join(*f, ",") }
func (f *multiFlag) Set(value string) error {
	*f = append(*f, value)
	return nil
}

type Config struct {
	ID             string
	ExecFile       string
	UID            int
	GID            int
	ChrootBaseDir  string
	Mounts         []string
	Env            []string
	CgroupVersion  int
	Cgroups        []string
	ParentCgroup   string
	NetNS          string
	ResourceLimits []string
	Daemonize      bool
	NewPIDNS       bool
	ExtraArgs      []string
}

func RunCLI(args []string) error {
	fs := flag.NewFlagSet("jailer", flag.ContinueOnError)
	var cgroups multiFlag
	var limits multiFlag
	var mounts multiFlag
	var env multiFlag

	cfg := Config{}
	fs.StringVar(&cfg.ID, "id", "", "microVM identifier")
	fs.StringVar(&cfg.ExecFile, "exec-file", "", "path to executable to run inside the jail")
	fs.IntVar(&cfg.UID, "uid", -1, "uid to switch to before exec")
	fs.IntVar(&cfg.GID, "gid", -1, "gid to switch to before exec")
	fs.StringVar(&cfg.ChrootBaseDir, "chroot-base-dir", defaultChrootBaseDir, "base directory used for chroot jails")
	fs.IntVar(&cfg.CgroupVersion, "cgroup-version", 2, "cgroup hierarchy version (only 2 is supported)")
	fs.Var(&mounts, "mount", "bind mount host paths into the jail as ro:/abs/src:/dest or rw:/abs/src:/dest (repeatable)")
	fs.Var(&env, "env", "environment variable KEY=VALUE passed to the jailed process (repeatable)")
	fs.Var(&cgroups, "cgroup", "cgroupv2 key=value (repeatable)")
	fs.StringVar(&cfg.ParentCgroup, "parent-cgroup", "", "parent cgroup path relative to /sys/fs/cgroup")
	fs.StringVar(&cfg.NetNS, "netns", "", "path to a network namespace handle")
	fs.Var(&limits, "resource-limit", "resource=value (repeatable, e.g. no-file=1024)")
	fs.BoolVar(&cfg.Daemonize, "daemonize", false, "detach and redirect stdio to /dev/null")
	fs.BoolVar(&cfg.NewPIDNS, "new-pid-ns", false, "launch the target inside a new PID namespace")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg.Mounts = append([]string(nil), mounts...)
	cfg.Env = append([]string(nil), env...)
	cfg.Cgroups = append([]string(nil), cgroups...)
	cfg.ResourceLimits = append([]string(nil), limits...)
	cfg.ExtraArgs = append([]string(nil), fs.Args()...)
	return Run(cfg)
}

func Run(cfg Config) error {
	if err := cfg.validate(); err != nil {
		return err
	}

	chrootDir := cfg.chrootDir()
	execName := filepath.Base(cfg.ExecFile)
	execPathInJail := "/" + execName
	mounts := append([]string(nil), cfg.Mounts...)
	depMounts, err := binaryDependencyMounts(cfg.ExecFile)
	if err != nil {
		return err
	}
	mounts = appendUniqueStrings(mounts, depMounts...)

	netnsFD := -1
	if cfg.NetNS != "" {
		fd, err := unix.Open(cfg.NetNS, unix.O_RDONLY|unix.O_CLOEXEC, 0)
		if err != nil {
			return fmt.Errorf("open netns %s: %w", cfg.NetNS, err)
		}
		netnsFD = fd
		defer unix.Close(netnsFD)
	}

	// Clean any stale mounts and remnants from a previous crash at this
	// chroot path. Without this, leftover bind mounts accumulate and
	// cause ENOENT on subsequent runs.
	cleanStaleMounts(chrootDir)
	_ = os.RemoveAll(chrootDir)

	if err := mkdirAllNoSymlink(chrootDir, 0755); err != nil {
		return fmt.Errorf("create chroot dir %s: %w", chrootDir, err)
	}
	if err := copyRegularFile(cfg.ExecFile, filepath.Join(chrootDir, execName), 0755); err != nil {
		return fmt.Errorf("copy exec-file into jail: %w", err)
	}
	if err := applyResourceLimits(cfg.ResourceLimits); err != nil {
		return err
	}
	if err := applyCgroupV2(cfg); err != nil {
		return err
	}
	if err := prepareMountNamespace(chrootDir, mounts); err != nil {
		return err
	}
	if err := prepareJailFilesystem(cfg); err != nil {
		return err
	}
	if netnsFD >= 0 {
		if err := unix.Setns(netnsFD, unix.CLONE_NEWNET); err != nil {
			return fmt.Errorf("setns %s: %w", cfg.NetNS, err)
		}
	}

	if cfg.NewPIDNS || cfg.Daemonize {
		return spawnChild(execPathInJail, execName, cfg)
	}
	if err := dropPrivileges(cfg.UID, cfg.GID); err != nil {
		return err
	}
	// Verify the binary exists before exec — syscall.Exec returns a raw
	// errno ("no such file or directory") without any path context.
	if _, err := os.Stat(execPathInJail); err != nil {
		return fmt.Errorf("exec binary not found in jail: stat %s: %w", execPathInJail, err)
	}
	if err := syscall.Exec(execPathInJail, append([]string{execName}, cfg.ExtraArgs...), cfg.execEnv()); err != nil {
		return fmt.Errorf("exec %s: %w", execPathInJail, err)
	}
	return nil // unreachable after successful exec
}

func (cfg Config) validate() error {
	if cfg.ID == "" {
		return errors.New("--id is required")
	}
	if len(cfg.ID) > maxVMIDLen {
		return fmt.Errorf("--id exceeds %d characters", maxVMIDLen)
	}
	for _, ch := range cfg.ID {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-') {
			return fmt.Errorf("--id %q contains unsupported character %q", cfg.ID, ch)
		}
	}
	if cfg.ExecFile == "" {
		return errors.New("--exec-file is required")
	}
	if !filepath.IsAbs(cfg.ExecFile) {
		return fmt.Errorf("--exec-file must be absolute: %s", cfg.ExecFile)
	}
	info, err := os.Stat(cfg.ExecFile)
	if err != nil {
		return fmt.Errorf("stat exec-file %s: %w", cfg.ExecFile, err)
	}
	if info.IsDir() {
		return fmt.Errorf("--exec-file must be a file: %s", cfg.ExecFile)
	}
	if cfg.UID < 0 {
		return errors.New("--uid is required")
	}
	if cfg.GID < 0 {
		return errors.New("--gid is required")
	}
	if cfg.CgroupVersion != 0 && cfg.CgroupVersion != 2 {
		return fmt.Errorf("only cgroup v2 is supported, got %d", cfg.CgroupVersion)
	}
	if cfg.ChrootBaseDir == "" {
		cfg.ChrootBaseDir = defaultChrootBaseDir
	}
	for _, entry := range cfg.Mounts {
		if _, err := parseMount(entry); err != nil {
			return err
		}
	}
	for _, entry := range cfg.Env {
		if !strings.Contains(entry, "=") {
			return fmt.Errorf("invalid --env %q", entry)
		}
	}
	return nil
}

func (cfg Config) chrootDir() string {
	base := cfg.ChrootBaseDir
	if base == "" {
		base = defaultChrootBaseDir
	}
	return filepath.Join(base, filepath.Base(cfg.ExecFile), cfg.ID, "root")
}

func applyResourceLimits(values []string) error {
	if len(values) == 0 {
		return applySingleResourceLimit("no-file=2048")
	}
	for _, value := range values {
		if err := applySingleResourceLimit(value); err != nil {
			return err
		}
	}
	return nil
}

func applySingleResourceLimit(value string) error {
	parts := strings.SplitN(value, "=", 2)
	if len(parts) != 2 {
		return fmt.Errorf("invalid --resource-limit %q", value)
	}
	n, err := strconv.ParseUint(parts[1], 10, 64)
	if err != nil {
		return fmt.Errorf("parse --resource-limit %q: %w", value, err)
	}
	lim := &unix.Rlimit{Cur: n, Max: n}
	switch parts[0] {
	case "no-file":
		return unix.Setrlimit(unix.RLIMIT_NOFILE, lim)
	case "fsize":
		return unix.Setrlimit(unix.RLIMIT_FSIZE, lim)
	default:
		return fmt.Errorf("unsupported --resource-limit %q", parts[0])
	}
}

func applyCgroupV2(cfg Config) error {
	base := "/sys/fs/cgroup"
	parent := strings.Trim(cfg.ParentCgroup, "/")
	if parent == "" {
		parent = filepath.Base(cfg.ExecFile)
	}
	target := filepath.Join(base, parent)
	if len(cfg.Cgroups) > 0 {
		target = filepath.Join(target, cfg.ID)
		if err := os.MkdirAll(target, 0755); err != nil {
			return fmt.Errorf("create cgroup %s: %w", target, err)
		}
	}
	if _, err := os.Stat(target); err != nil {
		if len(cfg.Cgroups) == 0 && errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("stat cgroup %s: %w", target, err)
	}
	if err := os.WriteFile(filepath.Join(target, "cgroup.procs"), []byte(strconv.Itoa(os.Getpid())), 0644); err != nil {
		return fmt.Errorf("move process to cgroup %s: %w", target, err)
	}
	for _, entry := range cfg.Cgroups {
		parts := strings.SplitN(entry, "=", 2)
		if len(parts) != 2 {
			return fmt.Errorf("invalid --cgroup %q", entry)
		}
		path := filepath.Join(target, parts[0])
		if err := os.WriteFile(path, []byte(parts[1]), 0644); err != nil {
			return fmt.Errorf("write cgroup %s: %w", path, err)
		}
	}
	return nil
}

type mountSpec struct {
	readOnly bool
	source   string
	target   string
}

func prepareMountNamespace(chrootDir string, entries []string) error {
	if err := unix.Unshare(unix.CLONE_NEWNS); err != nil {
		return fmt.Errorf("unshare mount namespace: %w", err)
	}
	if err := unix.Mount("", "/", "", unix.MS_SLAVE|unix.MS_REC, ""); err != nil {
		return fmt.Errorf("make mounts slave: %w", err)
	}
	if err := unix.Mount(chrootDir, chrootDir, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
		return fmt.Errorf("bind jail root on itself: %w", err)
	}
	for _, entry := range entries {
		spec, err := parseMount(entry)
		if err != nil {
			return err
		}
		if err := bindMountIntoJail(chrootDir, spec); err != nil {
			return err
		}
	}
	if err := os.Chdir(chrootDir); err != nil {
		return fmt.Errorf("chdir jail root: %w", err)
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

func parseMount(entry string) (mountSpec, error) {
	parts := strings.SplitN(entry, ":", 3)
	if len(parts) != 3 {
		return mountSpec{}, fmt.Errorf("invalid --mount %q", entry)
	}
	mode := strings.ToLower(strings.TrimSpace(parts[0]))
	switch mode {
	case "ro", "rw":
	default:
		return mountSpec{}, fmt.Errorf("invalid --mount mode %q", parts[0])
	}
	source := strings.TrimSpace(parts[1])
	target := strings.TrimSpace(parts[2])
	if !filepath.IsAbs(source) {
		return mountSpec{}, fmt.Errorf("--mount source must be absolute: %s", source)
	}
	if !filepath.IsAbs(target) {
		return mountSpec{}, fmt.Errorf("--mount target must be absolute: %s", target)
	}
	if target == "/" {
		return mountSpec{}, fmt.Errorf("--mount target / is not allowed")
	}
	return mountSpec{
		readOnly: mode == "ro",
		source:   source,
		target:   filepath.Clean(target),
	}, nil
}

func bindMountIntoJail(chrootDir string, spec mountSpec) error {
	info, err := os.Stat(spec.source)
	if err != nil {
		return fmt.Errorf("stat mount source %s: %w", spec.source, err)
	}
	targetPath := filepath.Join(chrootDir, strings.TrimPrefix(spec.target, "/"))
	if err := prepareMountTarget(targetPath, info.IsDir()); err != nil {
		return fmt.Errorf("prepare mount target %s: %w", targetPath, err)
	}
	flags := uintptr(unix.MS_BIND)
	if info.IsDir() {
		flags |= unix.MS_REC
	}
	if err := unix.Mount(spec.source, targetPath, "", flags, ""); err != nil {
		return fmt.Errorf("bind mount %s -> %s: %w", spec.source, targetPath, err)
	}
	if spec.readOnly {
		remountFlags := uintptr(unix.MS_BIND | unix.MS_REMOUNT | unix.MS_RDONLY)
		if info.IsDir() {
			remountFlags |= unix.MS_REC
		}
		if err := unix.Mount("", targetPath, "", remountFlags, ""); err != nil {
			return fmt.Errorf("remount readonly %s: %w", targetPath, err)
		}
	}
	return nil
}

func prepareMountTarget(path string, isDir bool) error {
	if isDir {
		return mkdirAllNoSymlink(path, 0755)
	}
	if err := mkdirAllNoSymlink(filepath.Dir(path), 0755); err != nil {
		return err
	}
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0644)
	if err != nil {
		return err
	}
	return unix.Close(fd)
}

func binaryDependencyMounts(execFile string) ([]string, error) {
	cmd := exec.Command("ldd", execFile)
	output, err := cmd.Output()
	if err != nil {
		// Static binaries are fine; if ldd cannot inspect the file we do not fail hard.
		return nil, nil
	}
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	var mounts []string
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		path := ""
		if idx := strings.Index(line, "=>"); idx >= 0 {
			rest := strings.TrimSpace(line[idx+2:])
			if fields := strings.Fields(rest); len(fields) > 0 && strings.HasPrefix(fields[0], "/") {
				path = fields[0]
			}
		} else {
			fields := strings.Fields(line)
			if len(fields) > 0 && strings.HasPrefix(fields[0], "/") {
				path = fields[0]
			}
		}
		if path == "" {
			continue
		}
		if _, err := os.Stat(path); err != nil {
			continue
		}
		mounts = append(mounts, "ro:"+path+":"+path)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return mounts, nil
}

func appendUniqueStrings(dst []string, values ...string) []string {
	seen := make(map[string]struct{}, len(dst))
	for _, value := range dst {
		seen[value] = struct{}{}
	}
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		dst = append(dst, value)
		seen[value] = struct{}{}
	}
	return dst
}

func prepareJailFilesystem(cfg Config) error {
	for _, dir := range []string{"/proc", "/dev", "/dev/net", "/dev/pts", "/run", "/tmp"} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}
	if err := unix.Mount("proc", "/proc", "proc", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""); err != nil {
		return fmt.Errorf("mount /proc: %w", err)
	}
	if err := unix.Mount("tmpfs", "/dev", "tmpfs", unix.MS_NOSUID|unix.MS_NOEXEC, "mode=0755,size=16m"); err != nil {
		return fmt.Errorf("mount /dev tmpfs: %w", err)
	}
	if err := os.MkdirAll("/dev/net", 0755); err != nil {
		return err
	}
	if err := os.MkdirAll("/dev/pts", 0755); err != nil {
		return err
	}
	if err := unix.Mount("devpts", "/dev/pts", "devpts", 0, "newinstance,gid=5,mode=620,ptmxmode=666"); err != nil {
		return fmt.Errorf("mount /dev/pts: %w", err)
	}
	for _, device := range []struct {
		path  string
		major uint32
		minor uint32
		mode  uint32
	}{
		{path: "/dev/null", major: 1, minor: 3, mode: 0666},
		{path: "/dev/zero", major: 1, minor: 5, mode: 0666},
		{path: "/dev/full", major: 1, minor: 7, mode: 0666},
		{path: "/dev/random", major: 1, minor: 8, mode: 0666},
		{path: "/dev/urandom", major: 1, minor: 9, mode: 0666},
		{path: "/dev/kvm", major: 10, minor: 232, mode: 0660},
		{path: "/dev/net/tun", major: 10, minor: 200, mode: 0666},
		{path: "/dev/tty", major: 5, minor: 0, mode: 0666},
	} {
		if err := makeCharDevice(device.path, device.major, device.minor, device.mode); err != nil {
			return fmt.Errorf("create %s: %w", device.path, err)
		}
		if err := os.Chown(device.path, cfg.UID, cfg.GID); err != nil {
			return fmt.Errorf("chown %s: %w", device.path, err)
		}
	}
	for _, link := range []struct {
		path   string
		target string
	}{
		{path: "/dev/ptmx", target: "pts/ptmx"},
		{path: "/dev/fd", target: "/proc/self/fd"},
		{path: "/dev/stdin", target: "/proc/self/fd/0"},
		{path: "/dev/stdout", target: "/proc/self/fd/1"},
		{path: "/dev/stderr", target: "/proc/self/fd/2"},
	} {
		if err := ensureSymlink(link.path, link.target); err != nil {
			return fmt.Errorf("symlink %s -> %s: %w", link.path, link.target, err)
		}
	}
	return os.Chown("/", cfg.UID, cfg.GID)
}

func makeCharDevice(path string, major, minor uint32, mode uint32) error {
	if err := os.RemoveAll(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return unix.Mknod(path, unix.S_IFCHR|mode, int(unix.Mkdev(major, minor)))
}

func ensureSymlink(path, target string) error {
	if current, err := os.Readlink(path); err == nil {
		if current == target {
			return nil
		}
	}
	_ = os.RemoveAll(path)
	return os.Symlink(target, path)
}

func dropPrivileges(uid, gid int) error {
	if err := unix.Setgroups(nil); err != nil {
		return fmt.Errorf("setgroups: %w", err)
	}
	if err := unix.Setgid(gid); err != nil {
		return fmt.Errorf("setgid: %w", err)
	}
	if err := unix.Setuid(uid); err != nil {
		return fmt.Errorf("setuid: %w", err)
	}
	return nil
}

func spawnChild(execPath, execName string, cfg Config) error {
	var devNull *os.File
	var err error
	if cfg.Daemonize {
		devNull, err = os.OpenFile("/dev/null", os.O_RDWR, 0)
		if err != nil {
			return fmt.Errorf("open jailed /dev/null: %w", err)
		}
		defer devNull.Close()
	}

	cmd := exec.Command(execPath, cfg.ExtraArgs...)
	cmd.Env = cfg.execEnv()
	cmd.Dir = "/"
	if cfg.Daemonize {
		cmd.Stdin = devNull
		cmd.Stdout = devNull
		cmd.Stderr = devNull
	} else {
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: uint32(cfg.UID), Gid: uint32(cfg.GID)},
		Setsid:     cfg.Daemonize,
	}
	if cfg.NewPIDNS {
		cmd.SysProcAttr.Cloneflags |= unix.CLONE_NEWPID
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start jailed child: %w", err)
	}
	if cfg.NewPIDNS {
		pidPath := "/" + execName + ".pid"
		if err := os.WriteFile(pidPath, []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0644); err != nil {
			return fmt.Errorf("write %s: %w", pidPath, err)
		}
	}
	if cfg.Daemonize || cfg.NewPIDNS {
		return nil
	}
	if err := cmd.Wait(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			os.Exit(exitErr.ExitCode())
		}
		return err
	}
	return nil
}

func (cfg Config) execEnv() []string {
	if len(cfg.Env) == 0 {
		return []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}
	}
	out := make([]string, 0, len(cfg.Env)+1)
	hasPath := false
	for _, entry := range cfg.Env {
		if strings.HasPrefix(entry, "PATH=") {
			hasPath = true
		}
		out = append(out, entry)
	}
	if !hasPath {
		out = append(out, "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	}
	return out
}

func copyRegularFile(src, dst string, mode os.FileMode) error {
	srcInfo, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if srcInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("source %s must not be a symlink", src)
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := mkdirAllNoSymlink(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	if info, err := os.Lstat(dst); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("destination %s must not be a symlink", dst)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	fd, err := unix.Open(dst, unix.O_CREAT|unix.O_TRUNC|unix.O_WRONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, uint32(mode.Perm()))
	if err != nil {
		return err
	}
	out := os.NewFile(uintptr(fd), dst)
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}

// cleanStaleMounts recursively unmounts any leftover bind mounts inside dir.
// This handles the case where a previous jailer session crashed before
// pivot_root, leaving host-visible mounts that prevent reuse of the path.
func cleanStaleMounts(dir string) {
	dir = filepath.Clean(dir)
	if dir == "" || dir == "/" {
		return
	}
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return
	}
	defer f.Close()

	var targets []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 5 {
			continue
		}
		mountpoint := fields[4]
		if strings.HasPrefix(mountpoint, dir+"/") || mountpoint == dir {
			targets = append(targets, mountpoint)
		}
	}
	// Unmount deepest first with MNT_DETACH so we don't block.
	for i := len(targets) - 1; i >= 0; i-- {
		_ = unix.Unmount(targets[i], unix.MNT_DETACH)
	}
}

func mkdirAllNoSymlink(path string, perm os.FileMode) error {
	path = filepath.Clean(path)
	if path == "." || path == "" {
		return nil
	}
	if !filepath.IsAbs(path) {
		abs, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		path = abs
	}
	current := string(os.PathSeparator)
	trimmed := strings.TrimPrefix(path, string(os.PathSeparator))
	if trimmed == "" {
		return nil
	}
	for _, part := range strings.Split(trimmed, string(os.PathSeparator)) {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if err := os.Mkdir(current, perm); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
			info, err = os.Lstat(current)
			if err != nil {
				return err
			}
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("path %s contains symlink component", current)
		}
		if !info.IsDir() {
			return fmt.Errorf("path %s is not a directory", current)
		}
	}
	return nil
}
