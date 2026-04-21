package sandboxd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Store is the in-memory map of live sandboxes plus a best-effort
// JSON snapshot on disk. Persistence is for crash diagnostics — on
// restart the runtime can NOT pick up where it left off because the
// VM handles are gone. Stopped sandboxes are kept in the snapshot
// for a brief audit trail and dropped on the next clean restart.
//
// All exported methods take the lock — callers should not retain
// pointers across Store calls without re-fetching.
type Store struct {
	statePath string

	mu        sync.Mutex
	sandboxes map[string]*Sandbox
}

// NewStore opens (or creates) a JSON-backed store at the given path.
// On nonexistent file the store starts empty; on parse error the
// file is renamed to `<path>.corrupt-<unix-ts>` and we start fresh
// — losing the stopped audit trail is preferable to refusing to
// boot, and the timestamped sidecar keeps multiple corruption
// events on disk for post-mortem instead of overwriting the
// previous one.
func NewStore(statePath string) (*Store, error) {
	s := &Store{statePath: statePath, sandboxes: map[string]*Sandbox{}}
	if statePath == "" {
		return s, nil
	}
	if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
		return nil, fmt.Errorf("sandboxd store: mkdir parent %s: %w", filepath.Dir(statePath), err)
	}
	data, err := os.ReadFile(statePath)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, fmt.Errorf("sandboxd store: read %s: %w", statePath, err)
	}
	if len(data) == 0 {
		return s, nil
	}
	var loaded map[string]*Sandbox
	if err := json.Unmarshal(data, &loaded); err != nil {
		sidecar := fmt.Sprintf("%s.corrupt-%d", statePath, time.Now().Unix())
		if rerr := os.Rename(statePath, sidecar); rerr != nil {
			// If we can't move the corrupt file aside, fall back
			// to deleting it so we can recover on this boot — the
			// alternative is refusing to start, which is worse.
			_ = os.Remove(statePath)
		}
		return s, nil
	}
	s.sandboxes = loaded
	return s, nil
}

// Add inserts a new sandbox. Returns an error if the ID is already
// taken — sandboxd should generate IDs that don't collide, but the
// guard is cheap.
func (s *Store) Add(sb *Sandbox) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.sandboxes[sb.ID]; exists {
		return fmt.Errorf("sandboxd store: id %q already in use", sb.ID)
	}
	s.sandboxes[sb.ID] = sb
	s.persistLocked()
	return nil
}

// Get returns a snapshot of the sandbox for id, or zero-value+false
// if not present. The snapshot is safe to use outside the store lock
// (e.g. for JSON encoding) because it's decoupled from any
// concurrent Store.Update writes on the live record. Callers that
// need to mutate the sandbox must go through Store.Update instead.
func (s *Store) Get(id string) (Sandbox, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sb, ok := s.sandboxes[id]
	if !ok {
		return Sandbox{}, false
	}
	return sb.snapshot(), true
}

// List returns snapshots of every sandbox sorted by CreatedAt
// ascending. Unlike the prior "return []*Sandbox live pointers"
// shape, this is race-safe: callers can iterate and JSON-encode the
// result without holding any lock because each element is a copy.
func (s *Store) List() []Sandbox {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Sandbox, 0, len(s.sandboxes))
	for _, sb := range s.sandboxes {
		out = append(out, sb.snapshot())
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

// Update applies fn to the sandbox under the store's lock and
// persists. Returns false if the id isn't found. Store.mu is held for
// the whole callback so fn has exclusive access; it is deferred so a
// panic inside fn doesn't leave the lock held.
func (s *Store) Update(id string, fn func(*Sandbox)) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	sb, ok := s.sandboxes[id]
	if !ok {
		return false
	}
	fn(sb)
	s.persistLocked()
	return true
}

// Remove deletes the sandbox from the in-memory map. Returns the
// removed sandbox so callers can do cleanup with the still-attached
// runResult pointer (e.g. VM.Stop).
func (s *Store) Remove(id string) (*Sandbox, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sb, ok := s.sandboxes[id]
	if !ok {
		return nil, false
	}
	delete(s.sandboxes, id)
	s.persistLocked()
	return sb, true
}

// Snapshot returns the on-disk JSON path for inspection — useful
// in tests and for operators who want to peek at live state.
func (s *Store) Snapshot() string { return s.statePath }

func (s *Store) persistLocked() {
	if s.statePath == "" {
		return
	}
	data, err := json.MarshalIndent(s.sandboxes, "", "  ")
	if err != nil {
		return
	}
	tmp := s.statePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, s.statePath)
}
