package api

import (
	"strings"
	"testing"
)

func TestValidateNetworkMode_AcceptsEmpty(t *testing.T) {
	if err := validateNetworkMode("", "", ""); err != nil {
		t.Errorf("empty mode: %v", err)
	}
	if err := validateNetworkMode("", "10.0.0.2/24", "10.0.0.1"); err != nil {
		t.Errorf("empty mode + explicit IP: %v", err)
	}
}

func TestValidateNetworkMode_AcceptsNone(t *testing.T) {
	if err := validateNetworkMode("none", "", ""); err != nil {
		t.Errorf("none: %v", err)
	}
	if err := validateNetworkMode("NONE", "", ""); err != nil {
		t.Errorf("NONE (case): %v", err)
	}
}

func TestValidateNetworkMode_AcceptsAuto(t *testing.T) {
	if err := validateNetworkMode("auto", "", ""); err != nil {
		t.Errorf("auto alone: %v", err)
	}
	if err := validateNetworkMode("Auto", "", ""); err != nil {
		t.Errorf("Auto (case): %v", err)
	}
}

func TestValidateNetworkMode_RejectsAutoWithStaticIP(t *testing.T) {
	err := validateNetworkMode("auto", "10.0.42.2/24", "")
	if err == nil {
		t.Fatal("expected error when auto is combined with static_ip")
	}
	if !strings.Contains(err.Error(), "exclusive") {
		t.Errorf("error message missing 'exclusive': %v", err)
	}
}

func TestValidateNetworkMode_RejectsAutoWithGateway(t *testing.T) {
	err := validateNetworkMode("auto", "", "10.0.42.1")
	if err == nil {
		t.Fatal("expected error when auto is combined with gateway")
	}
}

func TestValidateNetworkMode_RejectsUnknownValue(t *testing.T) {
	err := validateNetworkMode("bridge", "", "")
	if err == nil {
		t.Fatal("expected error for unknown mode")
	}
	if !strings.Contains(err.Error(), "bridge") {
		t.Errorf("error does not echo the invalid value: %v", err)
	}
}

func TestNormalizeNetworkMode(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"none", ""},
		{"NONE", ""},
		{"  none  ", ""},
		{"auto", "auto"},
		{"Auto", "auto"},
		{"  auto  ", "auto"},
		{"bridge", ""}, // unknown normalizes to "" (validator already rejected it)
	}
	for _, c := range cases {
		if got := normalizeNetworkMode(c.in); got != c.want {
			t.Errorf("normalizeNetworkMode(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCloneTapName_FitsIFNAMSIZ(t *testing.T) {
	cases := []string{"gc-1", "gc-99999", "gc-1776270905018381062", "weird-nongcprefix-id"}
	for _, id := range cases {
		name := cloneTapName(id)
		if len(name) > 15 {
			t.Errorf("cloneTapName(%q) = %q (len=%d), exceeds IFNAMSIZ-1", id, name, len(name))
		}
		if !strings.HasPrefix(name, "tclone-") {
			t.Errorf("cloneTapName(%q) = %q, want tclone- prefix", id, name)
		}
	}
}

func TestCloneTapName_DistinctPerID(t *testing.T) {
	a := cloneTapName("gc-111111")
	b := cloneTapName("gc-222222")
	if a == b {
		t.Errorf("clone tap names collide: %q == %q", a, b)
	}
}
