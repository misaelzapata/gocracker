package vmm

import (
	"strings"
	"testing"
)

func TestApplySharedFSRebinds_NoOp(t *testing.T) {
	snap := &Snapshot{Config: Config{SharedFS: []SharedFSConfig{
		{Source: "/orig", Tag: "tag0", Target: "/mnt/t0", SocketPath: "/worker/v0.sock"},
	}}}
	if err := applySharedFSRebinds(snap, nil); err != nil {
		t.Fatalf("nil rebinds should be a no-op: %v", err)
	}
	if snap.Config.SharedFS[0].Source != "/orig" || snap.Config.SharedFS[0].SocketPath != "/worker/v0.sock" {
		t.Fatalf("SharedFS mutated: %+v", snap.Config.SharedFS[0])
	}
}

func TestApplySharedFSRebinds_Rewrites(t *testing.T) {
	snap := &Snapshot{Config: Config{SharedFS: []SharedFSConfig{
		{Source: "/template/a", Tag: "tag0", Target: "/mnt/a", SocketPath: "/worker/v0.sock"},
		{Source: "/template/b", Tag: "tag1", Target: "/mnt/b", SocketPath: "/worker/v1.sock"},
	}}}
	err := applySharedFSRebinds(snap, []SharedFSRebind{
		{Target: "/mnt/a", Source: "/sandbox/toolbox"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if snap.Config.SharedFS[0].Source != "/sandbox/toolbox" {
		t.Errorf("Source not rewritten: got %q", snap.Config.SharedFS[0].Source)
	}
	// Tag must be preserved so the guest kernel's frozen virtiofs mount
	// continues to find its device.
	if snap.Config.SharedFS[0].Tag != "tag0" {
		t.Errorf("Tag rewritten: got %q, want tag0", snap.Config.SharedFS[0].Tag)
	}
	// SocketPath must be cleared so the new host's virtiofsd is used.
	if snap.Config.SharedFS[0].SocketPath != "" {
		t.Errorf("SocketPath not cleared: %q", snap.Config.SharedFS[0].SocketPath)
	}
	// Non-rebound entry stays untouched.
	if snap.Config.SharedFS[1].Source != "/template/b" || snap.Config.SharedFS[1].SocketPath != "/worker/v1.sock" {
		t.Errorf("sibling mutated: %+v", snap.Config.SharedFS[1])
	}
}

func TestApplySharedFSRebinds_UnknownTargetFailsFast(t *testing.T) {
	snap := &Snapshot{Config: Config{SharedFS: []SharedFSConfig{
		{Source: "/template/a", Tag: "tag0", Target: "/mnt/a"},
	}}}
	err := applySharedFSRebinds(snap, []SharedFSRebind{
		{Target: "/never-mounted", Source: "/whatever"},
	})
	if err == nil {
		t.Fatal("expected error for unknown target")
	}
	if !strings.Contains(err.Error(), "/never-mounted") {
		t.Errorf("error does not name the missing target: %v", err)
	}
	if !strings.Contains(err.Error(), "virtiofs") {
		t.Errorf("error does not hint at the virtiofs convention: %v", err)
	}
}

func TestApplySharedFSRebinds_IgnoresEntriesWithoutTarget(t *testing.T) {
	// Pre-Target snapshots (legacy) have SharedFS entries with empty
	// Target. Rebinds by Target should fail clearly rather than silently
	// matching an empty-string lookup.
	snap := &Snapshot{Config: Config{SharedFS: []SharedFSConfig{
		{Source: "/legacy", Tag: "tag0"}, // no Target
	}}}
	err := applySharedFSRebinds(snap, []SharedFSRebind{
		{Target: "/mnt/x", Source: "/new"},
	})
	if err == nil {
		t.Fatal("expected error: legacy snapshot has no matching Target")
	}
}

func TestApplySharedFSRebinds_NilSnapshotNoOp(t *testing.T) {
	if err := applySharedFSRebinds(nil, []SharedFSRebind{{Target: "/x", Source: "/y"}}); err != nil {
		t.Fatalf("nil snap should no-op, got %v", err)
	}
}
