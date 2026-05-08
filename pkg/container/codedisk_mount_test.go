package container

import (
	"testing"

	"github.com/gocracker/gocracker/pkg/vmm"
)

// TestCodeDisksAsDriveConfigs covers the public-CodeDisk → vmm.DriveConfig
// mapping used by the snapshot-restore (Phase 2) path. The drive IDs must
// be stable ("code0", "code1", ...) so they match the Phase 1 cold-boot
// ordering produced by runtimeDrives, and the host paths must passthrough
// verbatim (no resolution here — the CLI already did that).
func TestCodeDisksAsDriveConfigs(t *testing.T) {
	cases := []struct {
		name string
		in   []CodeDisk
		want []vmm.DriveConfig
	}{
		{
			name: "empty input returns nil",
			in:   nil,
			want: nil,
		},
		{
			name: "single rw disk",
			in: []CodeDisk{
				{HostPath: "/tmp/app.ext4", Mount: "/app", FSType: "ext4"},
			},
			want: []vmm.DriveConfig{
				{ID: "code0", Path: "/tmp/app.ext4", Root: false, ReadOnly: false},
			},
		},
		{
			name: "two disks, one ro",
			in: []CodeDisk{
				{HostPath: "/tmp/a.ext4", Mount: "/app"},
				{HostPath: "/tmp/b.ext4", Mount: "/data", ReadOnly: true},
			},
			want: []vmm.DriveConfig{
				{ID: "code0", Path: "/tmp/a.ext4", Root: false, ReadOnly: false},
				{ID: "code1", Path: "/tmp/b.ext4", Root: false, ReadOnly: true},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := codeDisksAsDriveConfigs(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("len = %d, want %d (got=%+v)", len(got), len(tc.want), got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("entry %d: got %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// fakeVM is the minimal vmm.Handle-shaped value the helpers consume.
// We only need VMConfig() — exposing the stub via the same interface
// keeps the helpers honest about their actual surface.
type fakeVM struct {
	cfg vmm.Config
}

func (f *fakeVM) VMConfig() vmm.Config { return f.cfg }

func TestVsockUDSForCodeDiskMount(t *testing.T) {
	t.Run("vsock missing returns empty", func(t *testing.T) {
		vm := &fakeVM{cfg: vmm.Config{}}
		if got := vsockUDSForCodeDiskMount(vm); got != "" {
			t.Errorf("got %q, want empty (no Vsock cfg)", got)
		}
	})
	t.Run("vsock with path returns path", func(t *testing.T) {
		vm := &fakeVM{cfg: vmm.Config{Vsock: &vmm.VsockConfig{Enabled: true, UDSPath: "/var/run/vm.sock"}}}
		if got := vsockUDSForCodeDiskMount(vm); got != "/var/run/vm.sock" {
			t.Errorf("got %q, want /var/run/vm.sock", got)
		}
	})
}

func TestNonRootDriveCount(t *testing.T) {
	cases := []struct {
		name string
		cfg  vmm.Config
		want int
	}{
		{
			name: "no drives",
			cfg:  vmm.Config{},
			want: 0,
		},
		{
			name: "only root",
			cfg:  vmm.Config{Drives: []vmm.DriveConfig{{ID: "root", Root: true}}},
			want: 0,
		},
		{
			name: "root plus two",
			cfg: vmm.Config{Drives: []vmm.DriveConfig{
				{ID: "root", Root: true},
				{ID: "code0"},
				{ID: "code1"},
			}},
			want: 2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vm := &fakeVM{cfg: tc.cfg}
			if got := nonRootDriveCount(vm); got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}
