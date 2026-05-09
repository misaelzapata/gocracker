//go:build linux

package main

// cmdInstall sets up gocracker-sandboxd as a persistent system service.
//
// What it does (all idempotent):
//  1. Creates the "gocracker" Unix group (if absent).
//  2. Copies the running binary + gocracker to /usr/local/bin/.
//  3. Creates /etc/gocracker/sandboxd.env with GOCRACKER_KERNEL=<path>.
//  4. Writes /etc/systemd/system/gocracker-sandboxd.service.
//  5. Runs: systemctl daemon-reload && systemctl enable --now gocracker-sandboxd.
//  6. Prints the one-liner users need to join the group (no sudo for MCP).
//
// Run once as root after installing the binaries:
//
//	sudo gocracker-sandboxd install --kernel-path /abs/path/to/kernel

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

const serviceUnit = `[Unit]
Description=gocracker sandbox control plane
Documentation=https://github.com/gocracker/gocracker
After=network.target

[Service]
Type=simple
ExecStart=/usr/local/bin/gocracker-sandboxd serve \
    --addr 127.0.0.1:9091 \
    --state-dir /var/lib/gocracker-sandboxd \
    --kernel-path ${GOCRACKER_KERNEL} \
    --network-mode ${GOCRACKER_NETWORK_MODE} \
    --uds-group gocracker
EnvironmentFile=-/etc/gocracker/sandboxd.env
Restart=on-failure
RestartSec=5s
StandardOutput=journal
StandardError=journal
SyslogIdentifier=gocracker-sandboxd

[Install]
WantedBy=multi-user.target
`

const envTemplate = `# gocracker-sandboxd environment
# Edit this file and run: systemctl restart gocracker-sandboxd

GOCRACKER_KERNEL=%s
# Network mode: "auto" (TAP+iptables, needs root) or "slirp" (rootless, needs kvm group only)
GOCRACKER_NETWORK_MODE=%s
`

func cmdInstall(args []string) {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	kernelPath := fs.String("kernel-path", os.Getenv("GOCRACKER_KERNEL"),
		"Absolute path to the guest kernel vmlinux (required)")
	group := fs.String("group", "gocracker",
		"Unix group that may dial sandbox UDS sockets (gocracker-mcp runs as this group)")
	binDir := fs.String("bin-dir", "/usr/local/bin",
		"Directory to install gocracker-sandboxd and gocracker binaries")
	networkMode := fs.String("network-mode", "auto",
		`"auto" (TAP+iptables, service runs as root) or "slirp" (rootless, only needs kvm group)`)
	_ = fs.Parse(args)

	if *kernelPath == "" {
		fmt.Fprintln(os.Stderr, "install: --kernel-path required (or set GOCRACKER_KERNEL)")
		os.Exit(1)
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "install: must run as root (sudo gocracker-sandboxd install …)")
		os.Exit(1)
	}

	steps := []struct {
		name string
		fn   func() error
	}{
		{"create group " + *group, func() error { return ensureGroup(*group) }},
		{"install binaries → " + *binDir, func() error { return installBinaries(*binDir) }},
		{"write /etc/gocracker/sandboxd.env", func() error { return writeEnvFile(*kernelPath, *networkMode) }},
		{"write /etc/systemd/system/gocracker-sandboxd.service", writeServiceUnit},
		{"systemctl daemon-reload", func() error { return runCmd("systemctl", "daemon-reload") }},
		{"systemctl enable --now gocracker-sandboxd", func() error {
			return runCmd("systemctl", "enable", "--now", "gocracker-sandboxd")
		}},
	}

	for _, s := range steps {
		fmt.Printf("  %s … ", s.name)
		if err := s.fn(); err != nil {
			fmt.Printf("FAILED\n    %v\n", err)
			os.Exit(1)
		}
		fmt.Println("ok")
	}

	fmt.Printf(`
gocracker-sandboxd is running and will restart automatically after reboots.

To use gocracker-mcp without sudo, add your user to the %q group:

    sudo usermod -aG %s $USER
    newgrp %s          # apply now, or log out and back in

Then run once to wire every installed AI tool:

    gocracker-mcp setup

`, *group, *group, *group)
}

func ensureGroup(name string) error {
	err := runCmd("getent", "group", name)
	if err == nil {
		return nil // already exists
	}
	return runCmd("groupadd", "--system", name)
}

func installBinaries(dir string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if err := copyBin(self, filepath.Join(dir, "gocracker-sandboxd")); err != nil {
		return err
	}
	// Also copy gocracker if it's sitting next to us (the worker binary).
	sibling := filepath.Join(filepath.Dir(self), "gocracker")
	if _, err := os.Stat(sibling); err == nil {
		return copyBin(sibling, filepath.Join(dir, "gocracker"))
	}
	if path, err := exec.LookPath("gocracker"); err == nil {
		return copyBin(path, filepath.Join(dir, "gocracker"))
	}
	return nil // gocracker not found — sandboxd still works if it's in PATH elsewhere
}

func copyBin(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	// Write to a temp file then rename so the swap is atomic (no partial
	// binary visible to concurrent systemd starts).
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

func writeEnvFile(kernelPath, networkMode string) error {
	if err := os.MkdirAll("/etc/gocracker", 0o755); err != nil {
		return err
	}
	content := fmt.Sprintf(envTemplate, kernelPath, networkMode)
	// Don't overwrite if the admin has customised it.
	if _, err := os.Stat("/etc/gocracker/sandboxd.env"); errors.Is(err, os.ErrNotExist) {
		return os.WriteFile("/etc/gocracker/sandboxd.env", []byte(content), 0o640)
	}
	return nil
}

func writeServiceUnit() error {
	return os.WriteFile(
		"/etc/systemd/system/gocracker-sandboxd.service",
		[]byte(serviceUnit),
		0o644,
	)
}

func runCmd(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stderr // route to stderr so stdout stays clean
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
