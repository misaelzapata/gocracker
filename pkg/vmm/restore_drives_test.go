//go:build linux

package vmm

import (
	"strings"
	"testing"
)

// TestApplyAdditionalDrives is the unit gate for Phase 2 of
// code-disk-attach: when the caller passes RestoreOptions.AdditionalDrives,
// the helper appends them to the snapshot's Config.Drives so setupDevices
// later sees the merged list. The merge must reject inputs that would
// produce an inconsistent guest device layout: nil drives are no-ops,
// duplicate IDs collide, and Root-flagged extras are rejected outright.
func TestApplyAdditionalDrives(t *testing.T) {
	t.Run("nil snapshot is rejected silently when extras are empty", func(t *testing.T) {
		if err := applyAdditionalDrives(nil, nil); err != nil {
			t.Fatalf("got %v, want nil", err)
		}
	})

	t.Run("empty extras leaves snapshot untouched", func(t *testing.T) {
		snap := &Snapshot{Config: Config{Drives: []DriveConfig{{ID: "root", Root: true}}}}
		if err := applyAdditionalDrives(snap, nil); err != nil {
			t.Fatalf("err = %v", err)
		}
		if got := len(snap.Config.Drives); got != 1 {
			t.Errorf("drives len = %d, want 1", got)
		}
	})

	t.Run("appends new non-root drives", func(t *testing.T) {
		snap := &Snapshot{Config: Config{Drives: []DriveConfig{{ID: "root", Path: "/r.ext4", Root: true}}}}
		extras := []DriveConfig{
			{ID: "code0", Path: "/v1.ext4"},
			{ID: "code1", Path: "/v2.ext4", ReadOnly: true},
		}
		if err := applyAdditionalDrives(snap, extras); err != nil {
			t.Fatalf("err = %v", err)
		}
		if got := len(snap.Config.Drives); got != 3 {
			t.Fatalf("drives len = %d, want 3", got)
		}
		if snap.Config.Drives[1].ID != "code0" || snap.Config.Drives[2].ID != "code1" {
			t.Errorf("merged drives in wrong order: %+v", snap.Config.Drives)
		}
		if !snap.Config.Drives[2].ReadOnly {
			t.Errorf("ReadOnly not preserved")
		}
	})

	t.Run("rejects root extra", func(t *testing.T) {
		snap := &Snapshot{Config: Config{Drives: []DriveConfig{{ID: "root", Root: true}}}}
		err := applyAdditionalDrives(snap, []DriveConfig{{ID: "evil", Root: true, Path: "/x"}})
		if err == nil || !strings.Contains(err.Error(), "root drive") {
			t.Fatalf("err = %v, want root-drive error", err)
		}
	})

	t.Run("rejects empty ID", func(t *testing.T) {
		snap := &Snapshot{Config: Config{Drives: []DriveConfig{{ID: "root", Root: true}}}}
		err := applyAdditionalDrives(snap, []DriveConfig{{Path: "/x"}})
		if err == nil || !strings.Contains(err.Error(), "ID") {
			t.Fatalf("err = %v, want ID-required error", err)
		}
	})

	t.Run("rejects empty path", func(t *testing.T) {
		snap := &Snapshot{Config: Config{Drives: []DriveConfig{{ID: "root", Root: true}}}}
		err := applyAdditionalDrives(snap, []DriveConfig{{ID: "code0"}})
		if err == nil || !strings.Contains(err.Error(), "path") {
			t.Fatalf("err = %v, want path-required error", err)
		}
	})

	t.Run("rejects ID collision with existing snapshot drives", func(t *testing.T) {
		// Phase 1 path bakes a code-disk into the snapshot under ID
		// "code0"; Phase 2 caller trying to also attach "code0" must
		// fail rather than silently shadow the original.
		snap := &Snapshot{Config: Config{Drives: []DriveConfig{
			{ID: "root", Root: true},
			{ID: "code0", Path: "/baked.ext4"},
		}}}
		err := applyAdditionalDrives(snap, []DriveConfig{{ID: "code0", Path: "/new.ext4"}})
		if err == nil || !strings.Contains(err.Error(), "collides") {
			t.Fatalf("err = %v, want collision error", err)
		}
	})

	t.Run("rejects duplicate ID within extras themselves", func(t *testing.T) {
		snap := &Snapshot{Config: Config{Drives: []DriveConfig{{ID: "root", Root: true}}}}
		err := applyAdditionalDrives(snap, []DriveConfig{
			{ID: "code0", Path: "/a"},
			{ID: "code0", Path: "/b"},
		})
		if err == nil || !strings.Contains(err.Error(), "collides") {
			t.Fatalf("err = %v, want intra-extras collision error", err)
		}
	})
}
