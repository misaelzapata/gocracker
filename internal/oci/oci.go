// Package oci pulls OCI images from registries and builds ext4 disk images.
package oci

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/Microsoft/hcsshim/ext4/tar2ext4"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

type tarDirMetadata struct {
	path string
	hdr  tar.Header
}

const (
	ext4SuperblockOffset                = 1024
	ext4InodesCountOffset               = ext4SuperblockOffset + 0x00
	ext4BlocksCountLowOffset            = ext4SuperblockOffset + 0x04
	ext4FreeBlocksCountLowOffset        = ext4SuperblockOffset + 0x0C
	ext4FreeInodesCountOffset           = ext4SuperblockOffset + 0x10
	ext4LogBlockSizeOffset              = ext4SuperblockOffset + 0x18
	ext4BlocksPerGroupOffset            = ext4SuperblockOffset + 0x20
	ext4InodesPerGroupOffset            = ext4SuperblockOffset + 0x28
	ext4InodeSizeOffset                 = ext4SuperblockOffset + 0x58
	ext4FeatureRoCompatOffset           = ext4SuperblockOffset + 0x64
	ext4RoCompatReadonly         uint32 = 0x1000
	ext4GroupDescTableOffset            = 4096
	ext4GroupDescSize                   = 32
	maxUint16                    uint32 = 0xffff
	maxUint32                    uint64 = 0xffffffff
)

// ImageConfig holds the OCI image config fields relevant to VM boot.
type Healthcheck struct {
	Test          []string
	Interval      time.Duration
	Timeout       time.Duration
	StartPeriod   time.Duration
	StartInterval time.Duration
	Retries       int
}

type ImageConfig struct {
	Entrypoint   []string
	Cmd          []string
	Env          []string
	WorkingDir   string
	User         string
	ExposedPorts []string
	Volumes      []string
	Labels       map[string]string
	StopSignal   string
	Shell        []string
	Healthcheck  *Healthcheck
}

// PullOptions configures an image pull.
type PullOptions struct {
	Ref      string // e.g. "ubuntu:22.04", "ghcr.io/org/img:tag"
	OS       string // default: linux
	Arch     string // default: amd64
	Username string
	Password string
	CacheDir string
}

// PulledImage is a fetched OCI image ready for extraction.
type PulledImage struct {
	Config ImageConfig
	img    v1.Image
	ref    name.Reference
}

type cachedImageMetadata struct {
	Ref      string    `json:"ref"`
	OS       string    `json:"os"`
	Arch     string    `json:"arch"`
	PulledAt time.Time `json:"pulled_at"`
}

var remoteImage = func(ref name.Reference, options ...remote.Option) (v1.Image, error) {
	return remote.Image(ref, options...)
}

func registryPullLockPath(cacheDir string, ref name.Reference) string {
	registry := ref.Context().RegistryStr()
	if registry != name.DefaultRegistry && registry != "registry-1.docker.io" && registry != "docker.io" {
		return ""
	}
	baseDir := strings.TrimSpace(cacheDir)
	if baseDir == "" {
		baseDir = filepath.Join(os.TempDir(), "gocracker-oci")
	}
	return filepath.Join(baseDir, "dockerhub.lock")
}

func withRegistryPullLock(cacheDir string, ref name.Reference, fn func() (v1.Image, error)) (v1.Image, error) {
	lockPath := registryPullLockPath(cacheDir, ref)
	if lockPath == "" {
		return fn()
	}
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return nil, fmt.Errorf("prepare docker hub pull lock: %w", err)
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open docker hub pull lock: %w", err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return nil, fmt.Errorf("lock docker hub pull: %w", err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}

// Pull fetches an OCI image from a registry.
func Pull(opts PullOptions) (*PulledImage, error) {
	if opts.OS == "" {
		opts.OS = "linux"
	}
	if opts.Arch == "" {
		opts.Arch = runtime.GOARCH
	}

	ref, err := name.ParseReference(opts.Ref)
	if err != nil {
		return nil, fmt.Errorf("parse ref %q: %w", opts.Ref, err)
	}

	platform := v1.Platform{OS: opts.OS, Architecture: opts.Arch}

	var auth authn.Authenticator = authn.Anonymous
	if opts.Username != "" {
		auth = authn.FromConfig(authn.AuthConfig{
			Username: opts.Username,
			Password: opts.Password,
		})
	} else if kc, err := authn.DefaultKeychain.Resolve(ref.Context()); err == nil {
		auth = kc
	}

	if cached, err := loadCachedImage(opts, ref); err == nil && cached != nil {
		fmt.Printf("[oci] cache hit %s (%s/%s)\n", ref, opts.OS, opts.Arch)
		cfgFile, err := cached.ConfigFile()
		if err != nil {
			return nil, fmt.Errorf("config from cache: %w", err)
		}
		return &PulledImage{
			Config: imageConfigFromV1(cfgFile),
			img:    cached,
			ref:    ref,
		}, nil
	}
	fmt.Printf("[oci] pulling %s (%s/%s)...\n", ref, opts.OS, opts.Arch)

	img, err := withRegistryPullLock(opts.CacheDir, ref, func() (v1.Image, error) {
		return remoteImage(ref,
			remote.WithAuth(auth),
			remote.WithPlatform(platform),
		)
	})
	if err != nil {
		return nil, fmt.Errorf("pull %s: %w", ref, err)
	}

	cfgFile, err := img.ConfigFile()
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if err := storeCachedImage(opts, ref, img); err != nil {
		fmt.Printf("[oci] cache store skipped for %s: %v\n", ref, err)
	}

	return &PulledImage{
		Config: imageConfigFromV1(cfgFile),
		img:    img,
		ref:    ref,
	}, nil
}

func imageConfigFromV1(cfgFile *v1.ConfigFile) ImageConfig {
	return ImageConfig{
		Entrypoint:   cfgFile.Config.Entrypoint,
		Cmd:          cfgFile.Config.Cmd,
		Env:          cfgFile.Config.Env,
		WorkingDir:   cfgFile.Config.WorkingDir,
		User:         cfgFile.Config.User,
		ExposedPorts: mapKeys(cfgFile.Config.ExposedPorts),
		Volumes:      mapKeys(cfgFile.Config.Volumes),
		Labels:       cloneStringMap(cfgFile.Config.Labels),
		StopSignal:   cfgFile.Config.StopSignal,
		Shell:        append([]string(nil), cfgFile.Config.Shell...),
		Healthcheck:  healthcheckFromOCI(cfgFile.Config.Healthcheck),
	}
}

// ImageConfigFromJSON decodes an OCI config JSON payload into the subset used by gocracker.
func ImageConfigFromJSON(data []byte) (ImageConfig, error) {
	var cfgFile v1.ConfigFile
	if err := json.Unmarshal(data, &cfgFile); err != nil {
		return ImageConfig{}, fmt.Errorf("decode image config: %w", err)
	}
	return imageConfigFromV1(&cfgFile), nil
}

// LoadLayoutImage opens a local OCI layout and returns its first image.
func LoadLayoutImage(dir string) (*PulledImage, error) {
	lp, err := layout.FromPath(dir)
	if err != nil {
		return nil, fmt.Errorf("open oci layout %s: %w", dir, err)
	}
	index, err := lp.ImageIndex()
	if err != nil {
		return nil, fmt.Errorf("read oci index %s: %w", dir, err)
	}
	manifest, err := index.IndexManifest()
	if err != nil {
		return nil, fmt.Errorf("read oci manifest %s: %w", dir, err)
	}
	if len(manifest.Manifests) == 0 {
		return nil, fmt.Errorf("oci layout %s is empty", dir)
	}
	img, err := index.Image(manifest.Manifests[0].Digest)
	if err != nil {
		return nil, fmt.Errorf("open oci image %s: %w", dir, err)
	}
	cfgFile, err := img.ConfigFile()
	if err != nil {
		return nil, fmt.Errorf("read oci config %s: %w", dir, err)
	}
	return &PulledImage{
		Config: imageConfigFromV1(cfgFile),
		img:    img,
	}, nil
}

func loadCachedImage(opts PullOptions, ref name.Reference) (v1.Image, error) {
	cachePath := cacheEntryPath(opts, ref)
	if cachePath == "" {
		return nil, nil
	}
	lockFile, err := cacheLock(cachePath)
	if err != nil {
		return nil, err
	}
	defer lockFile.Close()
	defer syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
	meta, err := readCachedMetadata(cachePath)
	if err != nil {
		_ = os.RemoveAll(cachePath)
		return nil, err
	}
	if meta.Ref != ref.Name() || meta.OS != opts.OS || meta.Arch != opts.Arch {
		_ = os.RemoveAll(cachePath)
		return nil, fmt.Errorf("cache metadata mismatch")
	}
	lp, err := layout.FromPath(cachePath)
	if err != nil {
		_ = os.RemoveAll(cachePath)
		return nil, err
	}
	index, err := lp.ImageIndex()
	if err != nil {
		_ = os.RemoveAll(cachePath)
		return nil, err
	}
	manifest, err := index.IndexManifest()
	if err != nil || len(manifest.Manifests) == 0 {
		_ = os.RemoveAll(cachePath)
		if err == nil {
			err = fmt.Errorf("cache index is empty")
		}
		return nil, err
	}
	return index.Image(manifest.Manifests[0].Digest)
}

func storeCachedImage(opts PullOptions, ref name.Reference, img v1.Image) error {
	cachePath := cacheEntryPath(opts, ref)
	if cachePath == "" {
		return nil
	}
	lockFile, err := cacheLock(cachePath)
	if err != nil {
		return err
	}
	defer lockFile.Close()
	defer syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)

	tmpPath := cachePath + ".tmp"
	if err := os.RemoveAll(tmpPath); err != nil {
		return err
	}
	lp, err := layout.Write(tmpPath, empty.Index)
	if err != nil {
		return err
	}
	if err := lp.AppendImage(img); err != nil {
		_ = os.RemoveAll(tmpPath)
		return err
	}
	meta := cachedImageMetadata{
		Ref:      ref.Name(),
		OS:       opts.OS,
		Arch:     opts.Arch,
		PulledAt: time.Now().UTC(),
	}
	if err := writeCachedMetadata(tmpPath, meta); err != nil {
		_ = os.RemoveAll(tmpPath)
		return err
	}
	if err := os.RemoveAll(cachePath); err != nil {
		_ = os.RemoveAll(tmpPath)
		return err
	}
	return os.Rename(tmpPath, cachePath)
}

func cacheEntryPath(opts PullOptions, ref name.Reference) string {
	if strings.TrimSpace(opts.CacheDir) == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(ref.Name() + "\x00" + opts.OS + "\x00" + opts.Arch))
	return filepath.Join(opts.CacheDir, hex.EncodeToString(sum[:]))
}

func cacheLock(cachePath string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
		return nil, err
	}
	lockPath := cachePath + ".lock"
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func readCachedMetadata(cachePath string) (cachedImageMetadata, error) {
	data, err := os.ReadFile(filepath.Join(cachePath, "gocracker-cache.json"))
	if err != nil {
		return cachedImageMetadata{}, err
	}
	var meta cachedImageMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return cachedImageMetadata{}, err
	}
	return meta, nil
}

func writeCachedMetadata(cachePath string, meta cachedImageMetadata) error {
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(cachePath, "gocracker-cache.json"), data, 0o644)
}

// ExtractToDir extracts all image layers into dir, applying OCI whiteouts.
func (p *PulledImage) ExtractToDir(dir string) error {
	layers, err := p.img.Layers()
	if err != nil {
		return fmt.Errorf("layers: %w", err)
	}
	fmt.Printf("[oci] extracting %d layers → %s\n", len(layers), dir)
	for i, layer := range layers {
		digest, _ := layer.Digest()
		fmt.Printf("[oci]   [%d/%d] %s\n", i+1, len(layers), digest.Hex[:12])
		rc, err := layer.Uncompressed()
		if err != nil {
			return fmt.Errorf("layer %d: %w", i, err)
		}
		if err := applyTar(dir, rc); err != nil {
			rc.Close()
			return fmt.Errorf("layer %d apply: %w", i, err)
		}
		rc.Close()
	}
	return nil
}

// applyTar applies an OCI tar layer with whiteout handling.
func applyTar(dir string, r io.Reader) error {
	tr := tar.NewReader(r)
	var dirs []tarDirMetadata
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		rel, err := cleanLayerEntryName(hdr.Name)
		if err != nil {
			return err
		}
		if rel == "." {
			continue
		}
		target, err := resolveLayerEntryPath(dir, rel)
		if err != nil {
			return err
		}
		base := filepath.Base(rel)
		parentDir := filepath.Dir(target)

		// OCI whiteout
		if strings.HasPrefix(base, ".wh.") {
			if base == ".wh..wh..opq" {
				entries, _ := os.ReadDir(parentDir)
				for _, e := range entries {
					os.RemoveAll(filepath.Join(parentDir, e.Name()))
				}
			} else {
				os.RemoveAll(filepath.Join(parentDir, strings.TrimPrefix(base, ".wh.")))
			}
			continue
		}

		if err := os.MkdirAll(parentDir, 0755); err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := prepareTarEntryTarget(target, hdr.Typeflag); err != nil {
				return err
			}
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)|0111); err != nil {
				return err
			}
			dirs = append(dirs, tarDirMetadata{path: target, hdr: *hdr})
		case tar.TypeReg, tar.TypeRegA:
			if err := prepareTarEntryTarget(target, hdr.Typeflag); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)|0200)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, tr)
			f.Close()
			if err != nil {
				return err
			}
			if err := applyTarEntryMetadata(target, hdr); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if _, err := resolveLayerLinkPath(dir, rel, hdr.Linkname); err != nil {
				return err
			}
			if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
				return err
			}
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
			if err := applyTarEntryMetadata(target, hdr); err != nil {
				return err
			}
		case tar.TypeLink:
			if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
				return err
			}
			linkTarget, err := resolveLayerLinkPath(dir, rel, hdr.Linkname)
			if err != nil {
				return err
			}
			if err := os.Link(linkTarget, target); err != nil {
				return err
			}
			if err := applyTarEntryMetadata(target, hdr); err != nil {
				return err
			}
		}
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := applyTarEntryMetadata(dirs[i].path, &dirs[i].hdr); err != nil {
			return err
		}
	}
	return nil
}

func prepareTarEntryTarget(target string, typeflag byte) error {
	info, err := os.Lstat(target)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	switch typeflag {
	case tar.TypeDir:
		if info.IsDir() {
			return nil
		}
	case tar.TypeReg, tar.TypeRegA:
		if info.Mode().IsRegular() {
			return nil
		}
	}
	return os.RemoveAll(target)
}

func cleanLayerEntryName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return ".", nil
	}
	cleaned := path.Clean("/" + filepath.ToSlash(name))
	if cleaned == "/" {
		return ".", nil
	}
	return strings.TrimPrefix(cleaned, "/"), nil
}

func resolveLayerEntryPath(root, rel string) (string, error) {
	cleaned := filepath.ToSlash(filepath.Clean(rel))
	if cleaned == "." {
		return root, nil
	}
	parentHost, err := resolveLayerParentDir(root, path.Dir("/"+cleaned))
	if err != nil {
		return "", err
	}
	target := filepath.Join(parentHost, filepath.Base(cleaned))
	if err := ensurePathWithinRoot(root, target); err != nil {
		return "", err
	}
	return target, nil
}

func resolveLayerLinkPath(root, entryRel, linkname string) (string, error) {
	if path.IsAbs(linkname) {
		return resolveLayerEntryPath(root, path.Clean(linkname))
	}

	cleanLink := filepath.ToSlash(filepath.Clean(linkname))
	rootRelative, err := resolveLayerEntryPath(root, cleanLink)
	if err != nil {
		return "", err
	}
	if _, err := os.Lstat(rootRelative); err == nil {
		return rootRelative, nil
	} else if err != nil && !os.IsNotExist(err) {
		return "", err
	}

	entryDir := path.Dir("/" + filepath.ToSlash(filepath.Clean(entryRel)))
	return resolveLayerEntryPath(root, path.Clean(path.Join(entryDir, cleanLink)))
}

func resolveLayerParentDir(root, containerDir string) (string, error) {
	containerDir = path.Clean(containerDir)
	if containerDir == "." || containerDir == "/" {
		return root, nil
	}
	parts := strings.Split(strings.TrimPrefix(containerDir, "/"), "/")
	hostDir := root
	containerCursor := "/"
	for _, part := range parts {
		if part == "" || part == "." {
			continue
		}
		nextLiteral := filepath.Join(hostDir, part)
		info, err := os.Lstat(nextLiteral)
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(nextLiteral)
			if err != nil {
				return "", err
			}
			var containerTarget string
			if path.IsAbs(target) {
				containerTarget = path.Clean(target)
			} else {
				containerTarget = path.Clean(path.Join(containerCursor, target))
			}
			hostDir = filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(containerTarget, "/")))
			containerCursor = containerTarget
			if err := ensurePathWithinRoot(root, hostDir); err != nil {
				return "", err
			}
			continue
		}
		if err != nil && !os.IsNotExist(err) {
			return "", err
		}
		hostDir = nextLiteral
		containerCursor = path.Join(containerCursor, part)
		if err := ensurePathWithinRoot(root, hostDir); err != nil {
			return "", err
		}
	}
	return hostDir, nil
}

func ensurePathWithinRoot(root, target string) error {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	targetAbs, err := filepath.Abs(target)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(rootAbs, targetAbs)
	if err != nil {
		return err
	}
	if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))) {
		return nil
	}
	return fmt.Errorf("layer entry %q escapes rootfs %q", targetAbs, rootAbs)
}

func applyTarEntryMetadata(path string, hdr *tar.Header) error {
	if hdr == nil {
		return nil
	}
	// Lchown FIRST: the Linux kernel clears setuid/setgid bits whenever a
	// non-root process changes ownership of a file (S_ISUID/S_ISGID strip).
	// If we chmod first then chown, suid binaries like /usr/bin/sudo lose
	// their setuid bit before they ever reach the rootfs.
	if err := os.Lchown(path, hdr.Uid, hdr.Gid); err != nil && !os.IsPermission(err) && err != syscall.EPERM {
		return err
	}
	if hdr.Typeflag != tar.TypeSymlink {
		// Use syscall.Chmod directly with the raw unix mode bits so the
		// setuid (0o4000), setgid (0o2000) and sticky (0o1000) bits from
		// the tar header are preserved. os.Chmod translates through
		// os.FileMode and silently drops those unix-only bits.
		if err := syscall.Chmod(path, uint32(hdr.Mode)&0o7777); err != nil && !os.IsPermission(err) {
			return err
		}
		atime := hdr.AccessTime
		if atime.IsZero() {
			atime = hdr.ModTime
		}
		mtime := hdr.ModTime
		if mtime.IsZero() {
			mtime = atime
		}
		if !atime.IsZero() || !mtime.IsZero() {
			if err := os.Chtimes(path, atime, mtime); err != nil && !os.IsPermission(err) {
				return err
			}
		}
	}
	return nil
}

// BuildExt4 creates a raw ext4 disk image from a rootfs directory.
func BuildExt4(rootfsDir, imagePath string, sizeMB int) error {
	fmt.Printf("[oci] building ext4 %s (%d MiB)\n", imagePath, sizeMB)

	f, err := os.OpenFile(imagePath, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	maxDiskSize := int64(sizeMB) * 1024 * 1024
	if err := f.Truncate(maxDiskSize); err != nil {
		return err
	}

	pr, pw := io.Pipe()
	go func() {
		pw.CloseWithError(writeRootfsTar(rootfsDir, pw)) //nolint:errcheck
	}()
	if err := tar2ext4.Convert(pr, f, tar2ext4.MaximumDiskSize(maxDiskSize)); err != nil {
		return fmt.Errorf("tar2ext4: %w", err)
	}
	if err := clearReadonlyCompatFeature(f); err != nil {
		return fmt.Errorf("ext4 superblock: %w", err)
	}
	if err := expandExt4WithinGroups(f, maxDiskSize); err != nil {
		return fmt.Errorf("ext4 grow: %w", err)
	}
	if err := f.Truncate(maxDiskSize); err != nil {
		return err
	}
	return nil
}

func clearReadonlyCompatFeature(f *os.File) error {
	var buf [4]byte
	if _, err := f.ReadAt(buf[:], ext4FeatureRoCompatOffset); err != nil {
		return err
	}
	features := binary.LittleEndian.Uint32(buf[:])
	features &^= ext4RoCompatReadonly
	binary.LittleEndian.PutUint32(buf[:], features)
	if _, err := f.WriteAt(buf[:], ext4FeatureRoCompatOffset); err != nil {
		return err
	}
	return f.Sync()
}

func expandExt4WithinGroups(f *os.File, maxDiskSize int64) error {
	geo, err := readExt4Geometry(f)
	if err != nil {
		return err
	}
	if geo.blockSize == 0 || geo.blocksPerGroup == 0 || geo.blocksCount == 0 {
		return nil
	}

	maxBlocks := maxAddressableBlocks(maxDiskSize, geo.blockSize)
	groupCount := blocksToGroups(geo.blocksCount, geo.blocksPerGroup)
	groupCapacity := groupCount * geo.blocksPerGroup
	targetBlocks := minUint32(maxBlocks, groupCapacity)
	if targetBlocks > geo.blocksCount {
		delta, err := growExistingLastGroup(f, geo, groupCount-1, targetBlocks)
		if err != nil {
			return err
		}
		geo.blocksCount += delta
		geo.freeBlocks += delta
	}

	if maxBlocks > geo.blocksCount {
		addedFreeBlocks, addedInodes, err := addExt4Groups(f, geo, maxBlocks)
		if err != nil {
			return err
		}
		geo.blocksCount = maxBlocks
		geo.freeBlocks += addedFreeBlocks
		geo.inodesCount += addedInodes
		geo.freeInodes += addedInodes
	}

	if err := writeUint32At(f, ext4BlocksCountLowOffset, geo.blocksCount); err != nil {
		return err
	}
	if err := writeUint32At(f, ext4FreeBlocksCountLowOffset, geo.freeBlocks); err != nil {
		return err
	}
	if err := writeUint32At(f, ext4InodesCountOffset, geo.inodesCount); err != nil {
		return err
	}
	if err := writeUint32At(f, ext4FreeInodesCountOffset, geo.freeInodes); err != nil {
		return err
	}
	return f.Sync()
}

type ext4Geometry struct {
	blockSize      uint32
	blocksPerGroup uint32
	blocksCount    uint32
	freeBlocks     uint32
	inodesPerGroup uint32
	inodesCount    uint32
	freeInodes     uint32
	inodeSize      uint32
}

func readExt4Geometry(f *os.File) (ext4Geometry, error) {
	logBlockSize, err := readUint32At(f, ext4LogBlockSizeOffset)
	if err != nil {
		return ext4Geometry{}, err
	}
	blocksPerGroup, err := readUint32At(f, ext4BlocksPerGroupOffset)
	if err != nil {
		return ext4Geometry{}, err
	}
	blocksCount, err := readUint32At(f, ext4BlocksCountLowOffset)
	if err != nil {
		return ext4Geometry{}, err
	}
	freeBlocks, err := readUint32At(f, ext4FreeBlocksCountLowOffset)
	if err != nil {
		return ext4Geometry{}, err
	}
	inodesPerGroup, err := readUint32At(f, ext4InodesPerGroupOffset)
	if err != nil {
		return ext4Geometry{}, err
	}
	inodesCount, err := readUint32At(f, ext4InodesCountOffset)
	if err != nil {
		return ext4Geometry{}, err
	}
	freeInodes, err := readUint32At(f, ext4FreeInodesCountOffset)
	if err != nil {
		return ext4Geometry{}, err
	}
	inodeSize16, err := readUint16At(f, ext4InodeSizeOffset)
	if err != nil {
		return ext4Geometry{}, err
	}
	return ext4Geometry{
		blockSize:      1024 << logBlockSize,
		blocksPerGroup: blocksPerGroup,
		blocksCount:    blocksCount,
		freeBlocks:     freeBlocks,
		inodesPerGroup: inodesPerGroup,
		inodesCount:    inodesCount,
		freeInodes:     freeInodes,
		inodeSize:      uint32(inodeSize16),
	}, nil
}

func growExistingLastGroup(f *os.File, geo ext4Geometry, group, targetBlocks uint32) (uint32, error) {
	lastDescOffset := ext4GroupDescTableOffset + int64(group*ext4GroupDescSize)

	var desc [ext4GroupDescSize]byte
	if _, err := f.ReadAt(desc[:], lastDescOffset); err != nil {
		return 0, err
	}
	bitmapBlock := binary.LittleEndian.Uint32(desc[0:4])
	if bitmapBlock == 0 {
		return 0, fmt.Errorf("invalid ext4 group %d bitmap block", group)
	}

	bitmap := make([]byte, geo.blockSize)
	bitmapOffset := int64(bitmapBlock) * int64(geo.blockSize)
	if _, err := f.ReadAt(bitmap, bitmapOffset); err != nil {
		return 0, err
	}

	groupBase := group * geo.blocksPerGroup
	oldWithin := geo.blocksCount - groupBase
	targetWithin := targetBlocks - groupBase
	if oldWithin > targetWithin || targetWithin > geo.blocksPerGroup {
		return 0, fmt.Errorf("invalid ext4 grow window %d..%d for group size %d", oldWithin, targetWithin, geo.blocksPerGroup)
	}
	for block := oldWithin; block < targetWithin; block++ {
		clearBitmapBit(bitmap, block)
	}
	if _, err := f.WriteAt(bitmap, bitmapOffset); err != nil {
		return 0, err
	}

	delta := targetWithin - oldWithin
	groupFree := binary.LittleEndian.Uint16(desc[12:14])
	if uint32(groupFree)+delta > maxUint16 {
		return 0, fmt.Errorf("group free block count overflow: %d + %d", groupFree, delta)
	}
	binary.LittleEndian.PutUint16(desc[12:14], uint16(uint32(groupFree)+delta))
	if _, err := f.WriteAt(desc[:], lastDescOffset); err != nil {
		return 0, err
	}
	return delta, nil
}

func addExt4Groups(f *os.File, geo ext4Geometry, targetBlocks uint32) (uint32, uint32, error) {
	currentGroups := blocksToGroups(geo.blocksCount, geo.blocksPerGroup)
	targetGroups := blocksToGroups(targetBlocks, geo.blocksPerGroup)
	if targetGroups <= currentGroups {
		return 0, 0, nil
	}

	inodeTableBlocks := geo.inodesPerGroup * geo.inodeSize / geo.blockSize
	metaBlocks := 2 + inodeTableBlocks
	var addedFreeBlocks uint32
	var addedInodes uint32

	for group := currentGroups; group < targetGroups; group++ {
		groupStart := group * geo.blocksPerGroup
		presentBlocks := geo.blocksPerGroup
		if group == targetGroups-1 && targetBlocks%geo.blocksPerGroup != 0 {
			presentBlocks = targetBlocks - groupStart
		}
		if presentBlocks < metaBlocks {
			return 0, 0, fmt.Errorf("ext4 group %d too small for metadata: have %d blocks, need %d", group, presentBlocks, metaBlocks)
		}

		blockBitmapBlock := groupStart
		inodeBitmapBlock := groupStart + 1
		inodeTableBlock := groupStart + 2

		blockBitmap := make([]byte, geo.blockSize)
		for block := uint32(0); block < metaBlocks; block++ {
			setBitmapBit(blockBitmap, block)
		}
		for block := presentBlocks; block < geo.blocksPerGroup; block++ {
			setBitmapBit(blockBitmap, block)
		}
		if err := writeZeroRegion(f, int64(inodeBitmapBlock)*int64(geo.blockSize), int64(geo.blockSize)); err != nil {
			return 0, 0, err
		}
		if err := writeZeroRegion(f, int64(inodeTableBlock)*int64(geo.blockSize), int64(inodeTableBlocks)*int64(geo.blockSize)); err != nil {
			return 0, 0, err
		}
		if _, err := f.WriteAt(blockBitmap, int64(blockBitmapBlock)*int64(geo.blockSize)); err != nil {
			return 0, 0, err
		}

		var desc [ext4GroupDescSize]byte
		binary.LittleEndian.PutUint32(desc[0:4], blockBitmapBlock)
		binary.LittleEndian.PutUint32(desc[4:8], inodeBitmapBlock)
		binary.LittleEndian.PutUint32(desc[8:12], inodeTableBlock)
		freeBlocks := presentBlocks - metaBlocks
		if freeBlocks > maxUint16 {
			return 0, 0, fmt.Errorf("group %d free block count exceeds 16-bit descriptor field", group)
		}
		if geo.inodesPerGroup > maxUint16 {
			return 0, 0, fmt.Errorf("group %d free inode count exceeds 16-bit descriptor field", group)
		}
		binary.LittleEndian.PutUint16(desc[12:14], uint16(freeBlocks))
		binary.LittleEndian.PutUint16(desc[14:16], uint16(geo.inodesPerGroup))
		binary.LittleEndian.PutUint16(desc[28:30], uint16(geo.inodesPerGroup))
		descOffset := ext4GroupDescTableOffset + int64(group*ext4GroupDescSize)
		if _, err := f.WriteAt(desc[:], descOffset); err != nil {
			return 0, 0, err
		}
		addedFreeBlocks += freeBlocks
		addedInodes += geo.inodesPerGroup
	}
	return addedFreeBlocks, addedInodes, nil
}

func readUint32At(f *os.File, offset int64) (uint32, error) {
	var buf [4]byte
	if _, err := f.ReadAt(buf[:], offset); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(buf[:]), nil
}

func readUint16At(f *os.File, offset int64) (uint16, error) {
	var buf [2]byte
	if _, err := f.ReadAt(buf[:], offset); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint16(buf[:]), nil
}

func writeUint32At(f *os.File, offset int64, value uint32) error {
	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], value)
	_, err := f.WriteAt(buf[:], offset)
	return err
}

func clearBitmapBit(bitmap []byte, index uint32) {
	byteIndex := index / 8
	bit := uint8(index % 8)
	if int(byteIndex) >= len(bitmap) {
		return
	}
	bitmap[byteIndex] &^= 1 << bit
}

func setBitmapBit(bitmap []byte, index uint32) {
	byteIndex := index / 8
	bit := uint8(index % 8)
	if int(byteIndex) >= len(bitmap) {
		return
	}
	bitmap[byteIndex] |= 1 << bit
}

func blocksToGroups(blocks, blocksPerGroup uint32) uint32 {
	if blocks == 0 || blocksPerGroup == 0 {
		return 0
	}
	return (blocks + blocksPerGroup - 1) / blocksPerGroup
}

func maxAddressableBlocks(maxDiskSize int64, blockSize uint32) uint32 {
	if blockSize == 0 || maxDiskSize <= 0 {
		return 0
	}
	blocks := maxDiskSize / int64(blockSize)
	if uint64(blocks) > maxUint32 {
		return ^uint32(0)
	}
	return uint32(blocks)
}

func writeZeroRegion(f *os.File, offset, length int64) error {
	if length <= 0 {
		return nil
	}
	const chunkSize = 4096
	zero := make([]byte, chunkSize)
	for written := int64(0); written < length; {
		n := int64(chunkSize)
		if remaining := length - written; remaining < n {
			n = remaining
		}
		if _, err := f.WriteAt(zero[:n], offset+written); err != nil {
			return err
		}
		written += n
	}
	return nil
}

func minUint32(a, b uint32) uint32 {
	if a < b {
		return a
	}
	return b
}

func writeRootfsTar(root string, w io.Writer) error {
	tw := tar.NewWriter(w)
	defer tw.Close()

	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)

		linkTarget := ""
		if info.Mode()&os.ModeSymlink != 0 {
			linkTarget, err = os.Readlink(path)
			if err != nil {
				return err
			}
		}

		hdr, err := tar.FileInfoHeader(info, linkTarget)
		if err != nil {
			return err
		}
		hdr.Name = rel
		if info.IsDir() && !strings.HasSuffix(hdr.Name, "/") {
			hdr.Name += "/"
		}
		if stat, ok := info.Sys().(*syscall.Stat_t); ok {
			hdr.Uid = int(stat.Uid)
			hdr.Gid = int(stat.Gid)
			// Use the raw Linux mode bits so setuid (0o4000), setgid (0o2000)
			// and the sticky bit (0o1000) survive into the tar header. Go's
			// tar.FileInfoHeader translates through os.FileMode, which drops
			// those unix-only bits; without this copy, `sudo` and other
			// suid binaries lose their setuid-on-exec attribute during the
			// rootfs → tar → ext4 pipeline.
			hdr.Mode = int64(stat.Mode & 0o7777)
			hdr.AccessTime = statAccessTime(stat)
			hdr.ChangeTime = statChangeTime(stat)
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}

		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
}

// Ref returns the image reference string.
func (p *PulledImage) Ref() string { return p.ref.String() }

func mapKeys[V any](values map[string]V) []string {
	if len(values) == 0 {
		return nil
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func cloneStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

func healthcheckFromOCI(cfg *v1.HealthConfig) *Healthcheck {
	if cfg == nil {
		return nil
	}
	return &Healthcheck{
		Test:        append([]string(nil), cfg.Test...),
		Interval:    time.Duration(cfg.Interval),
		Timeout:     time.Duration(cfg.Timeout),
		StartPeriod: time.Duration(cfg.StartPeriod),
		Retries:     cfg.Retries,
	}
}
