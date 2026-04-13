package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/gocracker/gocracker/internal/hostguard"
)

func TestRunSuccessWithoutChecks(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"-kvm=false", "-tun=false", "-pty=false"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() code = %d, want 0", code)
	}
	if strings.TrimSpace(stdout.String()) != "hostcheck: ok" {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunHostDevicesError(t *testing.T) {
	orig := checkHostDevices
	checkHostDevices = func(req hostguard.DeviceRequirements) error {
		if !req.NeedKVM || !req.NeedTun {
			t.Fatalf("unexpected req: %+v", req)
		}
		return errors.New("boom")
	}
	defer func() { checkHostDevices = orig }()

	var stdout, stderr bytes.Buffer
	code := run(nil, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run() code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "host devices: boom") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunPTYError(t *testing.T) {
	origDevices := checkHostDevices
	origPTY := checkPTYSupport
	checkHostDevices = func(hostguard.DeviceRequirements) error { return nil }
	checkPTYSupport = func() error { return errors.New("pty missing") }
	defer func() {
		checkHostDevices = origDevices
		checkPTYSupport = origPTY
	}()

	var stdout, stderr bytes.Buffer
	code := run([]string{"-kvm=false", "-tun=false"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run() code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "pty: pty missing") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}
