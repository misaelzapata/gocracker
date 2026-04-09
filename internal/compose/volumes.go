package compose

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/gocracker/gocracker/pkg/container"
)

type volumeMount struct {
	Source         string
	Target         string
	ReadOnly       bool
	Populate       bool
	IsDir          bool
	SyncBack       bool
	Shared         bool
	Consistency    string
	Propagation    string
	UnmountOnClose bool
	RemoveOnClose  bool
}

func isEphemeralGuestPath(target string) bool {
	cleaned := filepath.Clean(target)
	switch {
	case cleaned == "/run", strings.HasPrefix(cleaned, "/run/"):
		return true
	case cleaned == "/tmp", strings.HasPrefix(cleaned, "/tmp/"):
		return true
	case cleaned == "/dev", strings.HasPrefix(cleaned, "/dev/"):
		return true
	case cleaned == "/var/run", strings.HasPrefix(cleaned, "/var/run/"):
		return true
	default:
		return false
	}
}

type volumeUse struct {
	writers map[string]struct{}
}

func planServiceVolumes(services map[string]Service, declared map[string]Volume, contextDir, project string) (map[string][]volumeMount, error) {
	baseDir := filepath.Join(os.TempDir(), "gocracker-compose-volumes", project)
	if err := os.MkdirAll(baseDir, 0755); err != nil {
		return nil, err
	}

	uses := map[string]*volumeUse{}
	planned := make(map[string][]volumeMount, len(services))
	for serviceName, svc := range services {
		specs, err := parseVolumeSpecs(svc.Volumes)
		if err != nil {
			return nil, fmt.Errorf("service %s volumes: %w", serviceName, err)
		}
		for idx, spec := range specs {
			mount, key, err := planVolumeMount(spec, contextDir, baseDir, serviceName, idx, declared)
			if err != nil {
				return nil, fmt.Errorf("service %s volume %q: %w", serviceName, spec.Target, err)
			}
			planned[serviceName] = append(planned[serviceName], mount)
			if mount.ReadOnly {
				continue
			}
			if uses[key] == nil {
				uses[key] = &volumeUse{writers: map[string]struct{}{}}
			}
			uses[key].writers[serviceName] = struct{}{}
		}
	}

	for serviceName, mounts := range planned {
		for idx, mount := range mounts {
			key := mount.Source
			if mount.ReadOnly || uses[key] == nil || len(uses[key].writers) <= 1 {
				continue
			}
			if !mount.IsDir {
				mount = promoteSharedFileMount(mount)
			}
			mount.Shared = true
			mount.SyncBack = false
			mounts[idx] = mount
		}
		planned[serviceName] = mounts
	}

	return planned, nil
}

func promoteSharedFileMount(mount volumeMount) volumeMount {
	sourceDir := filepath.Dir(mount.Source)
	targetDir := filepath.Dir(mount.Target)
	if sourceDir == "." || sourceDir == "" {
		sourceDir = mount.Source
	}
	if targetDir == "." || targetDir == "" {
		targetDir = mount.Target
	}
	mount.Source = sourceDir
	mount.Target = targetDir
	mount.IsDir = true
	mount.Populate = false
	return mount
}

func planVolumeMount(spec volumeSpec, contextDir, baseDir, serviceName string, index int, declared map[string]Volume) (volumeMount, string, error) {
	switch spec.Type {
	case "tmpfs":
		source := filepath.Join(baseDir, fmt.Sprintf("%s-tmpfs-%d", serviceName, index))
		if err := os.MkdirAll(source, 0755); err != nil {
			return volumeMount{}, "", err
		}
		if err := mountTmpfs(source, spec.Tmpfs); err != nil {
			return volumeMount{}, "", err
		}
		return volumeMount{
			Source:         source,
			Target:         spec.Target,
			ReadOnly:       spec.ReadOnly,
			IsDir:          true,
			SyncBack:       false,
			Consistency:    spec.Consistency,
			UnmountOnClose: true,
			RemoveOnClose:  true,
		}, source, nil
	case "bind":
		source := spec.Source
		if !filepath.IsAbs(source) {
			source = filepath.Join(contextDir, source)
		}
		info, err := os.Stat(source)
		if err != nil {
			if os.IsNotExist(err) {
				if !spec.Bind.CreateHostPath {
					return volumeMount{}, "", fmt.Errorf("bind source %s does not exist and bind.create_host_path=false", source)
				}
				if err := os.MkdirAll(source, 0755); err != nil {
					return volumeMount{}, "", err
				}
				info, err = os.Stat(source)
			}
			if err != nil {
				return volumeMount{}, "", err
			}
		}
		// Single-writer bind mounts use the materialized path (copy into the
		// disk image, sync back on stop). The shared/virtiofs path is only
		// chosen later (planServiceVolumes loop) when multiple services write
		// to the same source. Auto-promoting single-writer directories to
		// virtio-fs broke compose-volume because virtiofs probe is not
		// reliable yet — kernel never registers the FS tag and the guest
		// init mount loops forever.
		shared := false
		syncBack := !spec.ReadOnly && !isEphemeralGuestPath(spec.Target)
		return volumeMount{
			Source:      source,
			Target:      spec.Target,
			ReadOnly:    spec.ReadOnly,
			IsDir:       info.IsDir(),
			SyncBack:    syncBack,
			Shared:      shared,
			Consistency: spec.Consistency,
			Propagation: spec.Bind.Propagation,
		}, source, nil
	case "volume":
		resolved, err := resolveNamedVolumeSource(spec.Source, contextDir, baseDir, declared)
		if err != nil {
			return volumeMount{}, "", err
		}
		source := resolved.Path
		if spec.Volume.Subpath != "" {
			source, err = resolveVolumeSubpath(source, spec.Volume.Subpath)
			if err != nil {
				return volumeMount{}, "", err
			}
		}
		info, err := os.Stat(source)
		if err != nil {
			return volumeMount{}, "", err
		}
		return volumeMount{
			Source:         source,
			Target:         spec.Target,
			ReadOnly:       spec.ReadOnly,
			Populate:       resolved.Populate && !spec.Volume.NoCopy,
			IsDir:          info.IsDir(),
			SyncBack:       !spec.ReadOnly && !isEphemeralGuestPath(spec.Target),
			Consistency:    spec.Consistency,
			UnmountOnClose: resolved.UnmountOnClose,
			RemoveOnClose:  resolved.RemoveOnClose,
		}, source, nil
	default:
		return volumeMount{}, "", fmt.Errorf("volume type %q is not supported", spec.Type)
	}
}

type resolvedVolumeSource struct {
	Path           string
	Populate       bool
	UnmountOnClose bool
	RemoveOnClose  bool
}

func resolveNamedVolumeSource(sourceSpec, contextDir, baseDir string, declared map[string]Volume) (resolvedVolumeSource, error) {
	if sourceSpec == "" {
		source := filepath.Join(baseDir, "anonymous")
		if err := os.MkdirAll(source, 0755); err != nil {
			return resolvedVolumeSource{}, err
		}
		candidate, err := os.MkdirTemp(source, "volume-*")
		if err != nil {
			return resolvedVolumeSource{}, err
		}
		return resolvedVolumeSource{
			Path:          candidate,
			Populate:      true,
			RemoveOnClose: true,
		}, nil
	}

	if volume, ok := declared[sourceSpec]; ok {
		if err := validateNamedVolumeConfig(sourceSpec, volume, contextDir); err != nil {
			return resolvedVolumeSource{}, err
		}
		if source, ok := localDriverSource(volume, contextDir); ok {
			if err := os.MkdirAll(source, 0755); err != nil {
				return resolvedVolumeSource{}, err
			}
			return resolvedVolumeSource{Path: source}, nil
		}
		if source, err := mountNamedVolume(volume, sourceSpec, baseDir); err != nil {
			return resolvedVolumeSource{}, err
		} else if source.Path != "" {
			return source, nil
		}
	}

	source := filepath.Join(baseDir, sourceSpec)
	if err := os.MkdirAll(source, 0755); err != nil {
		return resolvedVolumeSource{}, err
	}
	return resolvedVolumeSource{Path: source, Populate: true}, nil
}

func validateNamedVolumeConfig(sourceSpec string, volume Volume, contextDir string) error {
	driver := strings.TrimSpace(volume.Driver)
	if driver == "" || driver == "local" {
		if len(volume.DriverOpts) == 0 {
			return nil
		}
		if _, ok := localDriverSource(volume, contextDir); ok {
			return nil
		}
		if _, _, _, ok := nfsDriverConfig(volume); ok {
			return nil
		}
		return fmt.Errorf("local volume %q uses unsupported driver_opts", sourceSpec)
	}
	return fmt.Errorf("volume driver %q is not supported", volume.Driver)
}

func mountNamedVolume(volume Volume, sourceSpec, baseDir string) (resolvedVolumeSource, error) {
	fstype, device, options, ok := nfsDriverConfig(volume)
	if !ok {
		return resolvedVolumeSource{}, nil
	}
	name := strings.NewReplacer("/", "-", string(filepath.Separator), "-").Replace(sourceSpec)
	target := filepath.Join(baseDir, "mounted-"+name)
	if err := os.MkdirAll(target, 0755); err != nil {
		return resolvedVolumeSource{}, err
	}

	args := []string{"-t", fstype}
	if options != "" {
		args = append(args, "-o", options)
	}
	args = append(args, device, target)
	cmd := exec.Command("mount", args...)
	if output, err := cmd.CombinedOutput(); err != nil {
		return resolvedVolumeSource{}, fmt.Errorf("mount %s volume %s: %v: %s", fstype, sourceSpec, err, strings.TrimSpace(string(output)))
	}
	return resolvedVolumeSource{
		Path:           target,
		UnmountOnClose: true,
		RemoveOnClose:  true,
	}, nil
}

func nfsDriverConfig(volume Volume) (fstype, device, options string, ok bool) {
	driver := strings.TrimSpace(volume.Driver)
	if driver != "" && driver != "local" {
		return "", "", "", false
	}
	fstype = strings.TrimSpace(volume.DriverOpts["type"])
	if fstype == "" {
		return "", "", "", false
	}
	switch fstype {
	case "nfs", "nfs4":
	default:
		return "", "", "", false
	}
	device = strings.TrimSpace(volume.DriverOpts["device"])
	if device == "" {
		return "", "", "", false
	}
	options = strings.TrimSpace(volume.DriverOpts["o"])
	return fstype, device, options, true
}

func resolveVolumeSubpath(source, subpath string) (string, error) {
	cleaned := filepath.Clean(subpath)
	if cleaned == "." || cleaned == "" {
		return source, nil
	}
	if filepath.IsAbs(cleaned) || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("volume subpath %q escapes source", subpath)
	}
	resolved := filepath.Join(source, cleaned)
	if err := os.MkdirAll(resolved, 0755); err != nil {
		return "", err
	}
	return resolved, nil
}

func isBindSource(value string) bool {
	return filepath.IsAbs(value) || strings.HasPrefix(value, ".") || strings.HasPrefix(value, "..") || strings.Contains(value, string(filepath.Separator))
}

func toContainerMounts(mounts []volumeMount) []container.Mount {
	if len(mounts) == 0 {
		return nil
	}
	out := make([]container.Mount, 0, len(mounts))
	for _, mount := range mounts {
		out = append(out, container.Mount{
			Source:   mount.Source,
			Target:   mount.Target,
			ReadOnly: mount.ReadOnly,
			Populate: mount.Populate,
			Backend:  sharedFSBackend(mount),
		})
	}
	return out
}

func sharedFSBackend(mount volumeMount) container.MountBackend {
	if mount.Shared {
		return container.MountBackendVirtioFS
	}
	return container.MountBackendMaterialized
}

func syncVolumesFromDisk(service *ServiceVM) error {
	if service == nil || service.Result == nil || len(service.volumes) == 0 {
		return nil
	}
	needsSync := false
	for _, mount := range service.volumes {
		if mount.ReadOnly || !mount.SyncBack {
			continue
		}
		needsSync = true
		break
	}
	if !needsSync {
		return nil
	}
	return withMountedDisk(service.Result.DiskPath, func(root string) error {
		for _, mount := range service.volumes {
			if mount.ReadOnly || !mount.SyncBack {
				continue
			}
			if mount.IsDir {
				if err := syncDirVolumeFromMount(root, mount.Target, mount.Source); err != nil {
					return err
				}
				continue
			}
			if err := extractFileFromMount(root, mount.Target, mount.Source); err != nil {
				return err
			}
		}
		return nil
	})
}

func cleanupVolumeSources(service *ServiceVM, seen map[string]struct{}) error {
	if service == nil || len(service.volumes) == 0 {
		return nil
	}
	for _, mount := range service.volumes {
		if _, ok := seen[mount.Source]; ok {
			continue
		}
		seen[mount.Source] = struct{}{}
		if mount.UnmountOnClose {
			umountCmd := exec.Command("umount", mount.Source)
			if output, err := umountCmd.CombinedOutput(); err != nil {
				return fmt.Errorf("umount %s: %v: %s", mount.Source, err, strings.TrimSpace(string(output)))
			}
		}
		if mount.RemoveOnClose {
			if err := os.RemoveAll(mount.Source); err != nil {
				return err
			}
		}
	}
	return nil
}

func withMountedDisk(diskPath string, fn func(root string) error) error {
	mountDir, err := os.MkdirTemp("", "gocracker-compose-mount-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(mountDir)

	mountCmd := exec.Command("mount", "-t", "ext4", "-o", "loop", diskPath, mountDir)
	if output, err := mountCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("mount %s: %v: %s", diskPath, err, strings.TrimSpace(string(output)))
	}
	defer func() {
		umountCmd := exec.Command("umount", mountDir)
		if output, err := umountCmd.CombinedOutput(); err != nil {
			_ = exec.Command("umount", "-l", mountDir).Run()
			_ = output
		}
	}()

	return fn(mountDir)
}

func syncDirVolumeFromMount(root, guestPath, hostPath string) error {
	extracted := mountedGuestPath(root, guestPath)
	info, err := os.Stat(extracted)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("expected mounted volume path %s to be a directory", extracted)
	}

	if err := os.RemoveAll(hostPath); err != nil {
		return err
	}
	if err := os.MkdirAll(hostPath, 0755); err != nil {
		return err
	}
	return copyDirContents(extracted, hostPath)
}

func extractFileFromMount(root, guestPath, hostPath string) error {
	extracted := mountedGuestPath(root, guestPath)
	return copyFileOrSymlink(extracted, hostPath)
}

func mountedGuestPath(root, guestPath string) string {
	cleaned := strings.TrimPrefix(filepath.Clean(guestPath), string(filepath.Separator))
	if cleaned == "" || cleaned == "." {
		return root
	}
	return filepath.Join(root, cleaned)
}

func extractedVolumePath(root, guestPath string) string {
	return mountedGuestPath(root, guestPath)
}

func copyDir(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, info.Mode().Perm()); err != nil {
		return err
	}
	return copyDirContents(src, dst)
}

func copyDirContents(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())
		if entry.IsDir() {
			if err := copyDir(srcPath, dstPath); err != nil {
				return err
			}
			continue
		}
		if err := copyFileOrSymlink(srcPath, dstPath); err != nil {
			return err
		}
	}
	return nil
}

func copyFileOrSymlink(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if shouldSkipSpecialFile(info.Mode()) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		link, err := os.Readlink(src)
		if err != nil {
			return err
		}
		_ = os.RemoveAll(dst)
		return os.Symlink(link, dst)
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return nil
}

func shouldSkipSpecialFile(mode os.FileMode) bool {
	return !mode.IsRegular() && mode&os.ModeSymlink == 0 && !mode.IsDir()
}

func mountTmpfs(target string, spec tmpfsVolumeSpec) error {
	args := []string{"-t", "tmpfs"}
	options := make([]string, 0, 2)
	if spec.Size > 0 {
		options = append(options, fmt.Sprintf("size=%d", spec.Size))
	}
	if spec.Mode != 0 {
		options = append(options, fmt.Sprintf("mode=%#o", uint32(spec.Mode.Perm())))
	}
	if len(options) > 0 {
		args = append(args, "-o", strings.Join(options, ","))
	}
	args = append(args, "tmpfs", target)
	cmd := exec.Command("mount", args...)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("mount tmpfs %s: %v: %s", target, err, strings.TrimSpace(string(output)))
	}
	return nil
}
