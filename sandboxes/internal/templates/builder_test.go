package templates

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gocracker/gocracker/pkg/container"
)

// fakeBooter satisfies ColdBooter with controllable return values.
type fakeBooter struct {
	calls           atomic.Int32
	delay           time.Duration
	failErr         error
	snapshotDirPath string
}

func (f *fakeBooter) BootAndCapture(ctx context.Context, opts container.RunOptions) (string, error) {
	f.calls.Add(1)
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if f.failErr != nil {
		return "", f.failErr
	}
	return f.snapshotDirPath, nil
}

func newBuilderForTest(t *testing.T, booter ColdBooter) (*Builder, *Registry) {
	t.Helper()
	r, err := NewRegistry("")
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	b := NewBuilder(r)
	b.Booter = booter
	return b, r
}

func TestBuild_FreshCreatesTemplate(t *testing.T) {
	snapDir := t.TempDir()
	booter := &fakeBooter{snapshotDirPath: snapDir}
	b, r := newBuilderForTest(t, booter)

	res, err := b.Build(context.Background(), "", baseSpec())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if res.CacheHit {
		t.Error("CacheHit should be false on fresh build")
	}
	if res.Template.State != StateReady {
		t.Errorf("state=%s, want ready", res.Template.State)
	}
	if res.Template.SnapshotDir != snapDir {
		t.Errorf("SnapshotDir=%q, want %q", res.Template.SnapshotDir, snapDir)
	}
	if booter.calls.Load() != 1 {
		t.Errorf("BootAndCapture calls=%d, want 1", booter.calls.Load())
	}
	if list := r.List(); len(list) != 1 {
		t.Errorf("registry size=%d, want 1", len(list))
	}
}

// TestBuild_CacheHitIsNoOp — plan §6 success criterion: "Segundo
// create idéntico: cache hit, < 10 ms".
func TestBuild_CacheHitIsNoOp(t *testing.T) {
	snapDir := t.TempDir()
	booter := &fakeBooter{snapshotDirPath: snapDir}
	b, _ := newBuilderForTest(t, booter)

	if _, err := b.Build(context.Background(), "t1", baseSpec()); err != nil {
		t.Fatalf("first Build: %v", err)
	}
	if booter.calls.Load() != 1 {
		t.Fatalf("first Build booter calls=%d", booter.calls.Load())
	}

	start := time.Now()
	res, err := b.Build(context.Background(), "t2", baseSpec())
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("second Build: %v", err)
	}
	if !res.CacheHit {
		t.Error("CacheHit should be true on identical spec")
	}
	if booter.calls.Load() != 1 {
		t.Errorf("booter calls=%d, want 1 (cache hit shouldn't boot)", booter.calls.Load())
	}
	if elapsed > 5*time.Millisecond {
		t.Errorf("cache hit took %v, want <5ms (plan target <10ms)", elapsed)
	}
	if res.Template.SnapshotDir != snapDir {
		t.Errorf("cache hit SnapshotDir=%q, want %q", res.Template.SnapshotDir, snapDir)
	}
}

func TestBuild_StaleSnapshotTriggersRebuild(t *testing.T) {
	firstSnap := t.TempDir()
	booter := &fakeBooter{snapshotDirPath: firstSnap}
	b, _ := newBuilderForTest(t, booter)
	if _, err := b.Build(context.Background(), "t1", baseSpec()); err != nil {
		t.Fatalf("first Build: %v", err)
	}
	// Simulate operator rm -rf'ing the snapshot dir.
	if err := os.RemoveAll(firstSnap); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	// Point booter at a fresh path for the rebuild path.
	booter.snapshotDirPath = t.TempDir()

	res, err := b.Build(context.Background(), "t2", baseSpec())
	if err != nil {
		t.Fatalf("second Build: %v", err)
	}
	if res.CacheHit {
		t.Error("expected rebuild (stale snapshot), got cache hit")
	}
	if booter.calls.Load() != 2 {
		t.Errorf("booter calls=%d, want 2 (rebuild ran)", booter.calls.Load())
	}
}

func TestBuild_RejectsInvalidSpec(t *testing.T) {
	booter := &fakeBooter{}
	b, _ := newBuilderForTest(t, booter)
	_, err := b.Build(context.Background(), "", Spec{})
	if err == nil {
		t.Fatal("Build with invalid spec should error")
	}
	if !errors.Is(err, ErrInvalidSpec) {
		t.Errorf("err=%v, want wraps ErrInvalidSpec", err)
	}
	if booter.calls.Load() != 0 {
		t.Errorf("booter called for invalid spec: %d", booter.calls.Load())
	}
}

func TestBuild_BooterFailureTransitionsToError(t *testing.T) {
	booter := &fakeBooter{failErr: errors.New("cold-boot failed")}
	b, r := newBuilderForTest(t, booter)
	_, err := b.Build(context.Background(), "t1", baseSpec())
	if err == nil {
		t.Fatal("Build should error on booter failure")
	}
	tmpl, _ := r.Get("t1")
	if tmpl.State != StateError {
		t.Errorf("state=%s, want error", tmpl.State)
	}
	if tmpl.LastError == "" {
		t.Error("LastError not populated")
	}
}

func TestBuild_ConcurrentSameHashSerializes(t *testing.T) {
	snapDir := t.TempDir()
	booter := &fakeBooter{snapshotDirPath: snapDir, delay: 50 * time.Millisecond}
	b, _ := newBuilderForTest(t, booter)

	var wg sync.WaitGroup
	cacheHits := atomic.Int32{}
	fresh := atomic.Int32{}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := b.Build(context.Background(), "", baseSpec())
			if err != nil {
				t.Errorf("Build: %v", err)
				return
			}
			if res.CacheHit {
				cacheHits.Add(1)
			} else {
				fresh.Add(1)
			}
		}()
	}
	wg.Wait()

	if booter.calls.Load() != 1 {
		t.Errorf("booter calls=%d, want 1 (same-hash coalesce)", booter.calls.Load())
	}
	if fresh.Load() != 1 {
		t.Errorf("fresh builds=%d, want 1", fresh.Load())
	}
	if cacheHits.Load() != 3 {
		t.Errorf("cache hits=%d, want 3", cacheHits.Load())
	}
}

func TestDelete_LastReferenceRemovesSnapshot(t *testing.T) {
	snapDir := t.TempDir() + "/snap"
	if err := os.MkdirAll(snapDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	booter := &fakeBooter{snapshotDirPath: snapDir}
	b, _ := newBuilderForTest(t, booter)
	_, _ = b.Build(context.Background(), "t1", baseSpec())

	if err := b.Delete("t1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if pathExists(snapDir) {
		t.Error("Delete didn't remove snapshot dir (last reference)")
	}
}

func TestDelete_SharedSnapshotStays(t *testing.T) {
	snapDir := t.TempDir() + "/snap"
	_ = os.MkdirAll(snapDir, 0o755)
	booter := &fakeBooter{snapshotDirPath: snapDir}
	b, _ := newBuilderForTest(t, booter)
	_, _ = b.Build(context.Background(), "t1", baseSpec())
	_, _ = b.Build(context.Background(), "t2", baseSpec())

	if err := b.Delete("t1"); err != nil {
		t.Fatalf("Delete t1: %v", err)
	}
	if !pathExists(snapDir) {
		t.Error("snapshot dir removed while t2 still references it")
	}
	if err := b.Delete("t2"); err != nil {
		t.Fatalf("Delete t2: %v", err)
	}
	if pathExists(snapDir) {
		t.Error("snapshot dir not removed after last reference gone")
	}
}

func TestDelete_NotFound(t *testing.T) {
	b, _ := newBuilderForTest(t, &fakeBooter{})
	if err := b.Delete("ghost"); !errors.Is(err, ErrTemplateNotFound) {
		t.Errorf("err=%v, want ErrTemplateNotFound", err)
	}
}

func TestChooseID_GeneratesWhenEmpty(t *testing.T) {
	id := chooseID("")
	if !strings.HasPrefix(id, "tmpl-") || len(id) != 17 {
		t.Errorf("generated id=%q, want tmpl-<12hex>", id)
	}
	if chooseID("my-id") != "my-id" {
		t.Error("chooseID clobbered explicit id")
	}
}
