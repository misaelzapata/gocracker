package sandboxd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStore_AddGetRemove(t *testing.T) {
	s, err := NewStore("")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	sb := &Sandbox{ID: "sb-aaa", State: StateReady, CreatedAt: time.Now().UTC()}
	if err := s.Add(sb); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if got, ok := s.Get("sb-aaa"); !ok || got.ID != "sb-aaa" {
		t.Fatalf("Get: %+v ok=%v", got, ok)
	}
	if removed, ok := s.Remove("sb-aaa"); !ok || removed.ID != "sb-aaa" {
		t.Fatalf("Remove: %+v ok=%v", removed, ok)
	}
	if _, ok := s.Get("sb-aaa"); ok {
		t.Fatal("expected sandbox gone after remove")
	}
}

func TestStore_DuplicateAddRejected(t *testing.T) {
	s, _ := NewStore("")
	sb := &Sandbox{ID: "sb-dup"}
	if err := s.Add(sb); err != nil {
		t.Fatalf("first add: %v", err)
	}
	if err := s.Add(sb); err == nil {
		t.Fatal("expected duplicate add to error")
	}
}

func TestStore_ListSorted(t *testing.T) {
	s, _ := NewStore("")
	now := time.Now().UTC()
	for i, id := range []string{"sb-c", "sb-a", "sb-b"} {
		_ = s.Add(&Sandbox{ID: id, CreatedAt: now.Add(time.Duration(i) * time.Second)})
	}
	list := s.List()
	if len(list) != 3 {
		t.Fatalf("len: got %d, want 3", len(list))
	}
	// Sorted by CreatedAt → original insertion order: sb-c, sb-a, sb-b.
	if list[0].ID != "sb-c" || list[1].ID != "sb-a" || list[2].ID != "sb-b" {
		t.Fatalf("order: %v", []string{list[0].ID, list[1].ID, list[2].ID})
	}
}

func TestStore_UpdateMissingReturnsFalse(t *testing.T) {
	s, _ := NewStore("")
	if ok := s.Update("missing", func(*Sandbox) {}); ok {
		t.Fatal("expected Update of missing id to return false")
	}
}

func TestStore_PersistRoundTrip(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "store.json")

	s1, _ := NewStore(statePath)
	_ = s1.Add(&Sandbox{ID: "sb-persist", State: StateReady, Image: "alpine:3.20", CreatedAt: time.Now().UTC()})

	// Confirm the file exists with non-empty contents.
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("state file is empty")
	}

	// Reopen and verify.
	s2, err := NewStore(statePath)
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}
	got, ok := s2.Get("sb-persist")
	if !ok {
		t.Fatal("sandbox missing after reload")
	}
	if got.Image != "alpine:3.20" || got.State != StateReady {
		t.Fatalf("reload mismatch: %+v", got)
	}
}

func TestStore_CorruptFileRecovers(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "store.json")
	if err := os.WriteFile(statePath, []byte("{not valid json"), 0o600); err != nil {
		t.Fatalf("seed corrupt: %v", err)
	}
	s, err := NewStore(statePath)
	if err != nil {
		t.Fatalf("NewStore on corrupt: %v", err)
	}
	if len(s.List()) != 0 {
		t.Fatal("expected empty store after recovery")
	}
	// The corrupt file should have been moved aside under a
	// timestamped name so multiple failures don't overwrite
	// the last diagnostic copy.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	found := false
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "store.json.corrupt-") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected .corrupt-<ts> sidecar; entries=%v", entries)
	}
}
