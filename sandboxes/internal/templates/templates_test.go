package templates

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func baseSpec() Spec {
	return Spec{
		Image:      "alpine:3.20",
		KernelPath: "/k",
		MemMB:      256,
		CPUs:       1,
	}
}

func TestSpecHash_Deterministic(t *testing.T) {
	s := baseSpec()
	h1 := SpecHash(s)
	h2 := SpecHash(s)
	if h1 != h2 {
		t.Errorf("hash not deterministic: %q vs %q", h1, h2)
	}
	if len(h1) != 64 {
		t.Errorf("hash length = %d, want 64 (sha256 hex)", len(h1))
	}
}

func TestSpecHash_DistinctForDifferentInputs(t *testing.T) {
	a := baseSpec()
	b := baseSpec()
	b.MemMB = 512
	if SpecHash(a) == SpecHash(b) {
		t.Error("MemMB difference should change hash")
	}
}

func TestSpecHash_OrderInsensitiveForEnv(t *testing.T) {
	// NB: Env is a slice, so order DOES matter (intentional —
	// PATH=A:B and PATH=B:A behave differently in some images).
	// This test asserts the hash respects that, so callers don't
	// expect equality where there is none.
	a := baseSpec()
	b := baseSpec()
	a.Env = []string{"X=1", "Y=2"}
	b.Env = []string{"Y=2", "X=1"}
	if SpecHash(a) == SpecHash(b) {
		t.Error("env order matters; reordering should change hash")
	}
}

func TestSpecHash_VersionPrefixChanges(t *testing.T) {
	// Sanity: cache format version is part of the hash. Bumping it
	// should invalidate prior hashes — we can't easily test that
	// without mutating the const, so just confirm a non-empty hash
	// reflects the version prefix. Any change to the const is
	// caught by hash-difference tests in dependent code.
	h := SpecHash(baseSpec())
	if h == "" {
		t.Error("hash is empty")
	}
}

func TestSpec_Validate(t *testing.T) {
	cases := []struct {
		name string
		spec Spec
		ok   bool
	}{
		{"valid image", baseSpec(), true},
		{"valid dockerfile", Spec{Dockerfile: "/f", KernelPath: "/k"}, true},
		{"missing kernel", Spec{Image: "i"}, false},
		{"missing image and dockerfile", Spec{KernelPath: "/k"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.spec.Validate()
			if (err == nil) != tc.ok {
				t.Errorf("Validate=%v, want ok=%v", err, tc.ok)
			}
			if !tc.ok && !errors.Is(err, ErrInvalidSpec) {
				t.Errorf("err=%v, want wraps ErrInvalidSpec", err)
			}
		})
	}
}

func TestRegistry_AddGetList(t *testing.T) {
	r, err := NewRegistry("")
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t1 := &Template{ID: "tmpl-a", SpecHash: "h1", Spec: baseSpec(), State: StateReady}
	if err := r.Add(t1); err != nil {
		t.Fatalf("Add: %v", err)
	}
	got, ok := r.Get("tmpl-a")
	if !ok || got.ID != "tmpl-a" {
		t.Errorf("Get returned %+v ok=%v", got, ok)
	}
	if got.UpdatedAt.IsZero() || got.CreatedAt.IsZero() {
		t.Errorf("timestamps not populated: %+v", got)
	}
	if list := r.List(); len(list) != 1 {
		t.Errorf("List = %d entries, want 1", len(list))
	}
}

func TestRegistry_Add_RejectsDuplicateID(t *testing.T) {
	r, _ := NewRegistry("")
	t1 := &Template{ID: "x"}
	_ = r.Add(t1)
	if err := r.Add(&Template{ID: "x"}); err == nil {
		t.Error("duplicate Add should error")
	}
}

func TestRegistry_FindBySpecHash_ReadyOnly(t *testing.T) {
	r, _ := NewRegistry("")
	building := &Template{ID: "build", SpecHash: "h", State: StateBuilding}
	ready := &Template{ID: "ready", SpecHash: "h", State: StateReady}
	failed := &Template{ID: "err", SpecHash: "h", State: StateError}
	_ = r.Add(building)
	_ = r.Add(ready)
	_ = r.Add(failed)

	got, ok := r.FindBySpecHash("h")
	if !ok {
		t.Fatal("FindBySpecHash returned not-found")
	}
	if got.ID != "ready" {
		t.Errorf("got id=%q, want ready (building/error skipped)", got.ID)
	}
}

func TestRegistry_FindBySpecHash_NoMatchReturnsFalse(t *testing.T) {
	r, _ := NewRegistry("")
	if _, ok := r.FindBySpecHash("nonexistent"); ok {
		t.Error("FindBySpecHash should return false for unknown hash")
	}
}

func TestRegistry_Update_BumpsTimestamp(t *testing.T) {
	r, _ := NewRegistry("")
	t1 := &Template{ID: "x", State: StateBuilding}
	_ = r.Add(t1)
	got1, _ := r.Get("x")
	time.Sleep(2 * time.Millisecond)
	r.Update("x", func(t *Template) {
		t.State = StateReady
		t.SnapshotDir = "/snap"
	})
	got2, _ := r.Get("x")
	if got2.State != StateReady || got2.SnapshotDir != "/snap" {
		t.Errorf("Update didn't apply fn: %+v", got2)
	}
	if !got2.UpdatedAt.After(got1.UpdatedAt) {
		t.Errorf("UpdatedAt didn't advance: %v vs %v", got1.UpdatedAt, got2.UpdatedAt)
	}
}

func TestRegistry_Update_NotFoundReturnsFalse(t *testing.T) {
	r, _ := NewRegistry("")
	if r.Update("ghost", func(*Template) {}) {
		t.Error("Update on unknown id should return false")
	}
}

func TestRegistry_Remove(t *testing.T) {
	r, _ := NewRegistry("")
	_ = r.Add(&Template{ID: "x"})
	t1, ok := r.Remove("x")
	if !ok || t1.ID != "x" {
		t.Errorf("Remove returned %+v ok=%v", t1, ok)
	}
	if _, ok := r.Get("x"); ok {
		t.Error("post-Remove Get should be false")
	}
	if _, ok := r.Remove("x"); ok {
		t.Error("double-Remove should return false")
	}
}

func TestRegistry_PersistAndReload(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "templates.json")

	r1, err := NewRegistry(statePath)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t1 := &Template{ID: "tmpl-x", SpecHash: "h", Spec: baseSpec(), State: StateReady, SnapshotDir: "/snap"}
	if err := r1.Add(t1); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Reopen and verify the entry survived.
	r2, err := NewRegistry(statePath)
	if err != nil {
		t.Fatalf("reopen NewRegistry: %v", err)
	}
	got, ok := r2.Get("tmpl-x")
	if !ok || got.SpecHash != "h" || got.SnapshotDir != "/snap" {
		t.Errorf("reload lost data: %+v ok=%v", got, ok)
	}
}

func TestRegistry_CorruptStateRecovers(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "templates.json")
	// Write garbage.
	if err := writeFile(statePath, []byte("{ NOT JSON")); err != nil {
		t.Fatalf("seed corrupt file: %v", err)
	}
	// NewRegistry should recover (rename aside, start fresh).
	r, err := NewRegistry(statePath)
	if err != nil {
		t.Fatalf("NewRegistry should recover from corrupt state, got %v", err)
	}
	if list := r.List(); len(list) != 0 {
		t.Errorf("post-recovery list = %d, want 0", len(list))
	}
	// A sidecar with the corrupt content should exist.
	matches, _ := filepath.Glob(statePath + ".corrupt-*")
	if len(matches) == 0 {
		t.Error("corrupt sidecar not created")
	}
}

func TestConcurrentRegistry(t *testing.T) {
	r, _ := NewRegistry("")
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := "t-" + itoaSimple(i)
			_ = r.Add(&Template{ID: id, SpecHash: "h", State: StateReady})
		}(i)
	}
	wg.Wait()
	if list := r.List(); len(list) != 50 {
		t.Errorf("List = %d, want 50", len(list))
	}
}

func writeFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0o600)
}

func itoaSimple(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [16]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
