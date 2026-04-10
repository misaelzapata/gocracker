package container

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gocracker/gocracker/internal/guest"
	"github.com/gocracker/gocracker/internal/oci"
	"github.com/gocracker/gocracker/internal/runtimecfg"
)

func TestBuildCmdline_Defaults(t *testing.T) {
	cmdline := buildCmdline(oci.ImageConfig{}, RunOptions{})
	required := []string{
		"console=ttyS0",
		"reboot=k",
		"panic=1",
		"nomodule",
		"i8042.noaux",
		"i8042.nomux",
		"i8042.dumbkbd",
		"swiotlb=noforce",
		"rw",
		"root=/dev/vda",
		"rootfstype=ext4",
	}
	for _, want := range required {
		if !strings.Contains(cmdline, want) {
			t.Fatalf("cmdline missing %q:\n%s", want, cmdline)
		}
	}
	if strings.Contains(cmdline, runtimecfg.SerialDisable8250) {
		t.Fatalf("serial console cmdline should not disable 8250:\n%s", cmdline)
	}
}

func TestBuildCmdline_AllowsKernelModulesWhenInitrdCarriesThem(t *testing.T) {
	cmdline := buildCmdline(oci.ImageConfig{}, RunOptions{
		KernelModules: []guest.KernelModule{{Name: "virtiofs", HostPath: "/tmp/virtiofs.ko"}},
	})
	if strings.Contains(cmdline, "nomodule") {
		t.Fatalf("cmdline should omit nomodule when initrd carries kernel modules:\n%s", cmdline)
	}
}

func TestBuildCmdline_WithEntrypoint(t *testing.T) {
	spec := guestSpecForTest(oci.ImageConfig{}, RunOptions{
		Entrypoint: []string{"/usr/bin/myapp"},
	})
	want := runtimecfg.Process{Exec: "/usr/bin/myapp"}
	if !reflect.DeepEqual(spec.Process, want) {
		t.Fatalf("process = %#v, want %#v", spec.Process, want)
	}
}

func TestBuildCmdline_ImageConfigEntrypoint(t *testing.T) {
	spec := guestSpecForTest(oci.ImageConfig{
		Entrypoint: []string{"/docker-entrypoint.sh"},
	}, RunOptions{})
	want := runtimecfg.Process{Exec: "/docker-entrypoint.sh"}
	if !reflect.DeepEqual(spec.Process, want) {
		t.Fatalf("process = %#v, want %#v", spec.Process, want)
	}
}

func TestBuildCmdline_OptsOverrideImageEntrypoint(t *testing.T) {
	spec := guestSpecForTest(oci.ImageConfig{
		Entrypoint: []string{"/docker-entrypoint.sh"},
	}, RunOptions{
		Entrypoint: []string{"/custom-entrypoint"},
	})
	want := runtimecfg.Process{Exec: "/custom-entrypoint"}
	if !reflect.DeepEqual(spec.Process, want) {
		t.Fatalf("process = %#v, want %#v", spec.Process, want)
	}
}

func TestBuildCmdline_WithCmd(t *testing.T) {
	spec := guestSpecForTest(oci.ImageConfig{}, RunOptions{
		Cmd: []string{"echo", "hello"},
	})
	want := runtimecfg.Process{Exec: "echo", Args: []string{"hello"}}
	if !reflect.DeepEqual(spec.Process, want) {
		t.Fatalf("process = %#v, want %#v", spec.Process, want)
	}
}

func TestBuildCmdline_ImageConfigCmd(t *testing.T) {
	spec := guestSpecForTest(oci.ImageConfig{
		Cmd: []string{"nginx", "-g", "daemon off;"},
	}, RunOptions{})
	want := runtimecfg.Process{Exec: "nginx", Args: []string{"-g", "daemon off;"}}
	if !reflect.DeepEqual(spec.Process, want) {
		t.Fatalf("process = %#v, want %#v", spec.Process, want)
	}
}

func TestBuildCmdline_EntrypointWithExtraArgs(t *testing.T) {
	spec := guestSpecForTest(oci.ImageConfig{}, RunOptions{
		Entrypoint: []string{"/bin/sh", "-c"},
	})
	want := runtimecfg.Process{Exec: "/bin/sh", Args: []string{"-c"}}
	if !reflect.DeepEqual(spec.Process, want) {
		t.Fatalf("process = %#v, want %#v", spec.Process, want)
	}
}

func TestBuildCmdline_EntrypointAndCmdUseOCISemantics(t *testing.T) {
	spec := guestSpecForTest(oci.ImageConfig{
		Entrypoint: []string{"/docker-entrypoint.sh"},
		Cmd:        []string{"nginx", "-g", "daemon off;"},
	}, RunOptions{})
	want := runtimecfg.Process{
		Exec: "/docker-entrypoint.sh",
		Args: []string{"nginx", "-g", "daemon off;"},
	}
	if !reflect.DeepEqual(spec.Process, want) {
		t.Fatalf("process = %#v, want %#v", spec.Process, want)
	}
}

func TestBuildCmdline_WithEnv(t *testing.T) {
	spec := guestSpecForTest(oci.ImageConfig{}, RunOptions{
		Env: []string{"MY_VAR=hello"},
	})
	want := []string{"MY_VAR=hello"}
	if !reflect.DeepEqual(spec.Env, want) {
		t.Fatalf("env = %#v, want %#v", spec.Env, want)
	}
}

func TestBuildCmdline_WithHosts(t *testing.T) {
	spec := guestSpecForTest(oci.ImageConfig{}, RunOptions{
		Hosts: []string{"db=172.20.0.2", "cache=172.20.0.3"},
	})
	want := []string{"db=172.20.0.2", "cache=172.20.0.3"}
	if !reflect.DeepEqual(spec.Hosts, want) {
		t.Fatalf("hosts = %#v, want %#v", spec.Hosts, want)
	}
}

func TestBuildCmdline_ImageEnvMerged(t *testing.T) {
	spec := guestSpecForTest(oci.ImageConfig{
		Env: []string{"PATH=/usr/bin", "LANG=C"},
	}, RunOptions{
		Env: []string{"MY_VAR=test"},
	})
	want := []string{"PATH=/usr/bin", "LANG=C", "MY_VAR=test"}
	if !reflect.DeepEqual(spec.Env, want) {
		t.Fatalf("env = %#v, want %#v", spec.Env, want)
	}
}

func TestBuildCmdline_EnvWithSpaces(t *testing.T) {
	spec := guestSpecForTest(oci.ImageConfig{}, RunOptions{
		Env: []string{"MSG=hello world"},
	})
	want := []string{"MSG=hello world"}
	if !reflect.DeepEqual(spec.Env, want) {
		t.Fatalf("env = %#v, want %#v", spec.Env, want)
	}
}

func TestBuildCmdline_WithStaticIP(t *testing.T) {
	cmdline := buildCmdline(oci.ImageConfig{}, RunOptions{
		StaticIP: "172.20.0.5/24",
		Gateway:  "172.20.0.1",
	})
	if !strings.Contains(cmdline, "gc.ip=172.20.0.5/24") {
		t.Fatalf("missing static IP:\n%s", cmdline)
	}
	if !strings.Contains(cmdline, "gc.gw=172.20.0.1") {
		t.Fatalf("missing gateway:\n%s", cmdline)
	}
}

func TestBuildCmdline_StaticIPWithoutGateway(t *testing.T) {
	cmdline := buildCmdline(oci.ImageConfig{}, RunOptions{
		StaticIP: "10.0.0.2/24",
	})
	if !strings.Contains(cmdline, "gc.ip=10.0.0.2/24") {
		t.Fatalf("missing static IP:\n%s", cmdline)
	}
	if strings.Contains(cmdline, "gc.gw=") {
		t.Fatalf("unexpected gateway:\n%s", cmdline)
	}
}

func TestBuildCmdline_TapWithoutStaticIP(t *testing.T) {
	cmdline := buildCmdline(oci.ImageConfig{}, RunOptions{
		TapName: "tap0",
	})
	if !strings.Contains(cmdline, "gc.wait_network=1") {
		t.Fatalf("missing wait_network for tap:\n%s", cmdline)
	}
}

func TestBuildCmdline_WithMountsEnablesSyncRootfs(t *testing.T) {
	cmdline := buildCmdline(oci.ImageConfig{}, RunOptions{
		Mounts: []Mount{{Source: "/tmp/src", Target: "/data"}},
	})
	if !strings.Contains(cmdline, "gc.fs_sync=1") {
		t.Fatalf("missing gc.fs_sync for mounted rootfs:\n%s", cmdline)
	}
}

func TestBuildCmdline_WithWorkDir(t *testing.T) {
	spec := guestSpecForTest(oci.ImageConfig{}, RunOptions{
		WorkDir: "/app",
	})
	if spec.WorkDir != "/app" {
		t.Fatalf("workdir = %q, want /app", spec.WorkDir)
	}
}

func TestBuildCmdline_ImageWorkDir(t *testing.T) {
	spec := guestSpecForTest(oci.ImageConfig{
		WorkingDir: "/var/www",
	}, RunOptions{})
	if spec.WorkDir != "/var/www" {
		t.Fatalf("workdir = %q, want /var/www", spec.WorkDir)
	}
}

func TestBuildCmdline_OptsOverrideWorkDir(t *testing.T) {
	spec := guestSpecForTest(oci.ImageConfig{
		WorkingDir: "/var/www",
	}, RunOptions{
		WorkDir: "/custom",
	})
	if spec.WorkDir != "/custom" {
		t.Fatalf("workdir = %q, want /custom", spec.WorkDir)
	}
}

func TestBuildGuestSpec_PID1Mode(t *testing.T) {
	spec := guestSpecForTest(oci.ImageConfig{}, RunOptions{
		PID1Mode: runtimecfg.PID1ModeSupervised,
	})
	if spec.PID1Mode != runtimecfg.PID1ModeSupervised {
		t.Fatalf("pid1 mode = %q, want %q", spec.PID1Mode, runtimecfg.PID1ModeSupervised)
	}
}

func TestBuildGuestSpec_InteractiveConsoleDefaultsToSupervisedPID1(t *testing.T) {
	spec := guestSpecForTest(oci.ImageConfig{}, RunOptions{
		ConsoleIn: strings.NewReader(""),
	})
	if spec.PID1Mode != runtimecfg.PID1ModeSupervised {
		t.Fatalf("pid1 mode = %q, want %q", spec.PID1Mode, runtimecfg.PID1ModeSupervised)
	}
}

func TestWriteRuntimeSpecToDiskImage_ReplacesCachedRuntimeSpec(t *testing.T) {
	if _, err := exec.LookPath("debugfs"); err != nil {
		t.Skip("debugfs not available")
	}
	rootfs := t.TempDir()
	runtimeDir := filepath.Join(rootfs, "etc", "gocracker")
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		t.Fatalf("mkdir runtime dir: %v", err)
	}
	oldSpec := runtimecfg.GuestSpec{
		Process: runtimecfg.Process{Exec: "/bin/sh"},
	}
	if err := writeRuntimeSpecToRootfs(rootfs, oldSpec); err != nil {
		t.Fatalf("writeRuntimeSpecToRootfs(): %v", err)
	}
	diskPath := filepath.Join(t.TempDir(), "disk.ext4")
	if err := oci.BuildExt4(rootfs, diskPath, 64); err != nil {
		t.Fatalf("BuildExt4(): %v", err)
	}

	newSpec := runtimecfg.GuestSpec{
		Process:  runtimecfg.Process{Exec: "/bin/sh"},
		PID1Mode: runtimecfg.PID1ModeSupervised,
	}
	if err := writeRuntimeSpecToDiskImage(diskPath, newSpec); err != nil {
		t.Fatalf("writeRuntimeSpecToDiskImage(): %v", err)
	}

	out, err := exec.Command("debugfs", "-R", "cat "+runtimecfg.GuestSpecPath, diskPath).CombinedOutput()
	if err != nil {
		t.Fatalf("debugfs cat runtime.json: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), `"pid1_mode":"supervised"`) {
		t.Fatalf("runtime.json did not update:\n%s", out)
	}
}

func TestBuildCmdline_NoOptionalFields(t *testing.T) {
	cmdline := buildCmdline(oci.ImageConfig{}, RunOptions{})
	fields := runtimecfg.ParseKernelCmdline(cmdline)
	if fields[runtimecfg.FormatKey] != "" {
		t.Fatalf("unexpected structured runtime config:\n%s", cmdline)
	}
	if strings.Contains(cmdline, runtimecfg.ExecKey+"=") {
		t.Fatalf("unexpected exec in:\n%s", cmdline)
	}
	if strings.Contains(cmdline, runtimecfg.ArgPrefix) {
		t.Fatalf("unexpected args in:\n%s", cmdline)
	}
	if strings.Contains(cmdline, runtimecfg.WorkDirKey+"=") {
		t.Fatalf("unexpected workdir in:\n%s", cmdline)
	}
}

func TestBuildCmdline_FullConfig(t *testing.T) {
	cmdline := buildCmdline(oci.ImageConfig{
		Env:        []string{"PATH=/usr/bin"},
		WorkingDir: "/app",
	}, RunOptions{
		Entrypoint: []string{"/bin/server"},
		Cmd:        []string{"--port", "8080"},
		Env:        []string{"DEBUG=1"},
		StaticIP:   "192.168.1.2/24",
		Gateway:    "192.168.1.1",
	})
	spec := guestSpecForTest(oci.ImageConfig{
		Env:        []string{"PATH=/usr/bin"},
		WorkingDir: "/app",
	}, RunOptions{
		Entrypoint: []string{"/bin/server"},
		Cmd:        []string{"--port", "8080"},
		Env:        []string{"DEBUG=1"},
		StaticIP:   "192.168.1.2/24",
		Gateway:    "192.168.1.1",
	})
	wantProcess := runtimecfg.Process{Exec: "/bin/server", Args: []string{"--port", "8080"}}
	if !reflect.DeepEqual(spec.Process, wantProcess) {
		t.Fatalf("process = %#v, want %#v", spec.Process, wantProcess)
	}
	if spec.WorkDir != "/app" {
		t.Fatalf("workdir = %q, want /app", spec.WorkDir)
	}
	wantEnv := []string{"PATH=/usr/bin", "DEBUG=1"}
	if !reflect.DeepEqual(spec.Env, wantEnv) {
		t.Fatalf("env = %#v, want %#v", spec.Env, wantEnv)
	}
	for _, want := range []string{"gc.ip=192.168.1.2/24", "gc.gw=192.168.1.1"} {
		if !strings.Contains(cmdline, want) {
			t.Fatalf("missing %q in:\n%s", want, cmdline)
		}
	}
	for _, unexpected := range []string{runtimecfg.FormatKey + "=", runtimecfg.ExecKey + "=", runtimecfg.ArgPrefix, runtimecfg.EnvPrefix, runtimecfg.WorkDirKey + "="} {
		if strings.Contains(cmdline, unexpected) {
			t.Fatalf("unexpected structured runtime config %q in:\n%s", unexpected, cmdline)
		}
	}
}

func TestInspectCachedRunArtifacts_MissingDisk(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "image-config.json")
	_, usable, reason, err := inspectCachedRunArtifacts(filepath.Join(t.TempDir(), "disk.ext4"), configPath)
	if err != nil {
		t.Fatalf("inspectCachedRunArtifacts() error = %v", err)
	}
	if usable {
		t.Fatal("usable = true, want false")
	}
	if reason != "disk image missing" {
		t.Fatalf("reason = %q, want %q", reason, "disk image missing")
	}
}

func TestInspectCachedRunArtifacts_MissingConfig(t *testing.T) {
	dir := t.TempDir()
	diskPath := filepath.Join(dir, "disk.ext4")
	if err := os.WriteFile(diskPath, []byte("disk"), 0o644); err != nil {
		t.Fatalf("WriteFile(disk): %v", err)
	}
	_, usable, reason, err := inspectCachedRunArtifacts(diskPath, filepath.Join(dir, "image-config.json"))
	if err != nil {
		t.Fatalf("inspectCachedRunArtifacts() error = %v", err)
	}
	if usable {
		t.Fatal("usable = true, want false")
	}
	if reason != "image config missing" {
		t.Fatalf("reason = %q, want %q", reason, "image config missing")
	}
}

func TestInspectCachedRunArtifacts_InvalidConfig(t *testing.T) {
	dir := t.TempDir()
	diskPath := filepath.Join(dir, "disk.ext4")
	configPath := filepath.Join(dir, "image-config.json")
	if err := os.WriteFile(diskPath, []byte("disk"), 0o644); err != nil {
		t.Fatalf("WriteFile(disk): %v", err)
	}
	if err := os.WriteFile(configPath, []byte("{not-json"), 0o644); err != nil {
		t.Fatalf("WriteFile(config): %v", err)
	}
	_, usable, reason, err := inspectCachedRunArtifacts(diskPath, configPath)
	if err != nil {
		t.Fatalf("inspectCachedRunArtifacts() error = %v", err)
	}
	if usable {
		t.Fatal("usable = true, want false")
	}
	if !strings.Contains(reason, "image config unreadable") {
		t.Fatalf("reason = %q, want image config unreadable", reason)
	}
}

func TestInspectCachedRunArtifacts_ValidConfig(t *testing.T) {
	dir := t.TempDir()
	diskPath := filepath.Join(dir, "disk.ext4")
	configPath := filepath.Join(dir, "image-config.json")
	if err := os.WriteFile(diskPath, []byte("disk"), 0o644); err != nil {
		t.Fatalf("WriteFile(disk): %v", err)
	}
	want := oci.ImageConfig{
		Entrypoint: []string{"docker-entrypoint.sh"},
		Cmd:        []string{"postgres"},
	}
	if err := writeImageConfig(configPath, want); err != nil {
		t.Fatalf("writeImageConfig(): %v", err)
	}
	got, usable, reason, err := inspectCachedRunArtifacts(diskPath, configPath)
	if err != nil {
		t.Fatalf("inspectCachedRunArtifacts() error = %v", err)
	}
	if !usable {
		t.Fatalf("usable = false, want true (reason %q)", reason)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("config = %#v, want %#v", got, want)
	}
}

func TestPrepareBootDisk_CreatesMutableRuntimeCopy(t *testing.T) {
	workDir := t.TempDir()
	templateDisk := filepath.Join(workDir, "disk.ext4")
	if err := os.WriteFile(templateDisk, []byte("template"), 0o644); err != nil {
		t.Fatalf("WriteFile(templateDisk): %v", err)
	}

	bootDisk, cleanup, err := prepareBootDisk(workDir, templateDisk, "gc-123")
	if err != nil {
		t.Fatalf("prepareBootDisk() error = %v", err)
	}
	defer cleanup()

	if bootDisk == templateDisk {
		t.Fatalf("bootDisk = %q, want runtime copy different from template", bootDisk)
	}
	data, err := os.ReadFile(bootDisk)
	if err != nil {
		t.Fatalf("ReadFile(bootDisk): %v", err)
	}
	if string(data) != "template" {
		t.Fatalf("boot disk contents = %q, want %q", string(data), "template")
	}
	if err := os.WriteFile(bootDisk, []byte("mutated"), 0o644); err != nil {
		t.Fatalf("WriteFile(bootDisk): %v", err)
	}
	original, err := os.ReadFile(templateDisk)
	if err != nil {
		t.Fatalf("ReadFile(templateDisk): %v", err)
	}
	if string(original) != "template" {
		t.Fatalf("template disk contents = %q, want %q", string(original), "template")
	}
}

func TestSanitizeRuntimePathComponent(t *testing.T) {
	got := sanitizeRuntimePathComponent("vm/with spaces:*?")
	if got != "vm_with_spaces___" {
		t.Fatalf("sanitizeRuntimePathComponent() = %q, want %q", got, "vm_with_spaces___")
	}
	if got := sanitizeRuntimePathComponent(""); got != "vm" {
		t.Fatalf("sanitizeRuntimePathComponent(empty) = %q, want %q", got, "vm")
	}
}

func guestSpecForTest(imgConfig oci.ImageConfig, opts RunOptions) runtimecfg.GuestSpec {
	return buildGuestSpec(imgConfig, opts, resolveSharedFSMounts(opts.Mounts))
}
