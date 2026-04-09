// Package guest builds the guest initrd and provides the guest init binary.
//
// The init binary is pre-compiled at build time via "go generate" and embedded
// into the gocracker binary. No Go compiler is needed at runtime.
package guest

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	cpiolib "github.com/cavaliergopher/cpio"
	"github.com/gocracker/gocracker/internal/runtimecfg"
	"github.com/klauspost/compress/zstd"
)

// embeddedInitSentinel forces rebuilds of the guest package when the embedded
// init payload is regenerated out-of-band via go:generate.
const embeddedInitSentinel = "2026-04-09T18:35:00-03:00"

const moduleManifestPath = "/etc/gocracker/modules.list"

type KernelModule struct {
	Name     string
	HostPath string
}

type InitrdOptions struct {
	ExtraFiles    map[string]string
	KernelModules []KernelModule
	RuntimeSpec   *runtimecfg.GuestSpec
}

// BuildInitrd assembles a minimal rootfs with the embedded init binary
// and packs it as a gzip-compressed cpio initrd.
//
// extraFiles maps guest absolute paths to host source paths.
// e.g. {"/usr/local/bin/myapp": "/home/user/myapp"}
func BuildInitrd(outputPath string, extraFiles map[string]string) error {
	return BuildInitrdWithOptions(outputPath, InitrdOptions{ExtraFiles: extraFiles})
}

func EmbeddedInitDigest() string {
	sum := sha256.Sum256(embeddedInit)
	return hex.EncodeToString(sum[:])
}

// BuildInitrdWithOptions assembles a minimal rootfs with the embedded init
// binary, optional extra files, and optional kernel modules, then packs it as
// a gzip-compressed cpio initrd.
func BuildInitrdWithOptions(outputPath string, opts InitrdOptions) error {
	tmp, err := os.MkdirTemp("", "gocracker-initrd-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	// Directory skeleton
	for _, d := range []string{
		"sbin", "bin", "proc", "sys", "dev", "dev/pts",
		"tmp", "run", "etc", "lib", "lib64", "usr/bin", "home",
	} {
		os.MkdirAll(filepath.Join(tmp, d), 0755)
	}

	// Minimal /etc files
	os.WriteFile(filepath.Join(tmp, "etc/hosts"),
		[]byte("127.0.0.1 localhost\n::1 localhost\n"), 0644)
	os.WriteFile(filepath.Join(tmp, "etc/hostname"),
		[]byte("gocracker\n"), 0644)
	os.WriteFile(filepath.Join(tmp, "etc/resolv.conf"),
		[]byte("nameserver 8.8.8.8\nnameserver 1.1.1.1\n"), 0644)
	os.WriteFile(filepath.Join(tmp, "etc/passwd"),
		[]byte("root:x:0:0:root:/root:/bin/sh\n"), 0644)

	// Write pre-compiled init binary (embedded at build time)
	initBin := filepath.Join(tmp, "sbin/init")
	if err := os.WriteFile(initBin, embeddedInit, 0755); err != nil {
		return fmt.Errorf("write init binary: %w", err)
	}
	// Also write init at /init (kernel looks for /init in initramfs first)
	if err := os.WriteFile(filepath.Join(tmp, "init"), embeddedInit, 0755); err != nil {
		return fmt.Errorf("write /init: %w", err)
	}
	// Copy extra files
	for guestPath, hostPath := range opts.ExtraFiles {
		dst := filepath.Join(tmp, filepath.Clean(guestPath))
		os.MkdirAll(filepath.Dir(dst), 0755)
		if err := copyFile(hostPath, dst, 0755); err != nil {
			return fmt.Errorf("copy %s → %s: %w", hostPath, guestPath, err)
		}
	}
	if err := stageKernelModules(tmp, opts.KernelModules); err != nil {
		return fmt.Errorf("stage kernel modules: %w", err)
	}
	if err := stageRuntimeSpec(tmp, opts.RuntimeSpec); err != nil {
		return fmt.Errorf("stage runtime spec: %w", err)
	}

	// Pack into cpio.gz
	return packCpioGz(tmp, outputPath)
}

func stageRuntimeSpec(root string, spec *runtimecfg.GuestSpec) error {
	if spec == nil || !spec.HasStructuredFields() {
		return nil
	}
	data, err := spec.MarshalJSONBytes()
	if err != nil {
		return err
	}
	hostPath := filepath.Join(root, strings.TrimPrefix(runtimecfg.GuestSpecPath, "/"))
	if err := os.MkdirAll(filepath.Dir(hostPath), 0755); err != nil {
		return err
	}
	return os.WriteFile(hostPath, data, 0644)
}

func stageKernelModules(root string, modules []KernelModule) error {
	if len(modules) == 0 {
		return nil
	}

	stagedDir := filepath.Join(root, "lib", "modules", "gocracker")
	if err := os.MkdirAll(stagedDir, 0755); err != nil {
		return err
	}

	manifestLines := make([]string, 0, len(modules))
	for _, mod := range modules {
		name := sanitizeModuleName(mod)
		if name == "" {
			return fmt.Errorf("kernel module name is required for %q", mod.HostPath)
		}
		if mod.HostPath == "" {
			return fmt.Errorf("kernel module host path is required for %q", name)
		}
		guestRel := filepath.Join("lib", "modules", "gocracker", name+".ko")
		guestAbs := "/" + filepath.ToSlash(guestRel)
		dst := filepath.Join(root, guestRel)
		if err := copyKernelModule(mod.HostPath, dst); err != nil {
			return fmt.Errorf("%s: %w", mod.HostPath, err)
		}
		manifestLines = append(manifestLines, guestAbs)
	}

	sort.Strings(manifestLines)
	manifestHost := filepath.Join(root, strings.TrimPrefix(moduleManifestPath, "/"))
	if err := os.MkdirAll(filepath.Dir(manifestHost), 0755); err != nil {
		return err
	}
	return os.WriteFile(manifestHost, []byte(strings.Join(manifestLines, "\n")+"\n"), 0644)
}

func sanitizeModuleName(mod KernelModule) string {
	name := strings.TrimSpace(mod.Name)
	if name == "" {
		name = filepath.Base(strings.TrimSpace(mod.HostPath))
	}
	name = strings.TrimSuffix(name, ".zst")
	name = strings.TrimSuffix(name, ".xz")
	name = strings.TrimSuffix(name, ".gz")
	name = strings.TrimSuffix(name, ".ko")
	name = filepath.Base(name)
	return strings.TrimSpace(name)
}

func copyKernelModule(src, dst string) error {
	if strings.HasSuffix(src, ".zst") {
		return decompressZstdFile(src, dst, 0644)
	}
	return copyFile(src, dst, 0644)
}

func decompressZstdFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	decoder, err := zstd.NewReader(in)
	if err != nil {
		return err
	}
	defer decoder.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, decoder)
	return err
}

// packCpioGz packs a directory into a gzip-compressed cpio newc archive.
// Pure Go implementation — no external find/cpio/gzip binaries needed.
func packCpioGz(dir, output string) error {
	f, err := os.Create(output)
	if err != nil {
		return err
	}
	defer f.Close()

	gw, err := gzip.NewWriterLevel(f, gzip.BestCompression)
	if err != nil {
		return err
	}
	defer gw.Close()

	cw := cpiolib.NewWriter(gw)
	defer cw.Close()

	return filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			rel = "."
		} else {
			rel = "./" + rel
		}

		fi, err := d.Info()
		if err != nil {
			return err
		}

		hdr := &cpiolib.Header{
			Name: rel,
			Mode: cpiolib.FileMode(fi.Mode()),
			Size: 0,
		}

		switch {
		case fi.IsDir():
			// directories have size 0
		case fi.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			hdr.Size = int64(len(link))
			if err := cw.WriteHeader(hdr); err != nil {
				return err
			}
			_, err = cw.Write([]byte(link))
			return err
		case fi.Mode().IsRegular():
			hdr.Size = fi.Size()
		default:
			return nil // skip special files
		}

		if err := cw.WriteHeader(hdr); err != nil {
			return err
		}

		if fi.Mode().IsRegular() && fi.Size() > 0 {
			rf, err := os.Open(path)
			if err != nil {
				return err
			}
			defer rf.Close()
			_, err = io.Copy(cw, rf)
			return err
		}
		return nil
	})
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}

// EnvToKernelCmdline converts a slice of "KEY=VALUE" env vars into
// the legacy structured kernel cmdline fragment readable by the guest init.
func EnvToKernelCmdline(env []string) string {
	parts := runtimecfg.GuestSpec{Env: env}.AppendKernelArgs(nil)
	return strings.Join(parts, " ")
}
