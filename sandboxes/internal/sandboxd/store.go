package sandboxd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
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
// file is renamed .corrupt-<ts> and we start fresh — losing the
// stopped audit trail is preferable to refusing to boot.
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
		_ = os.Rename(statePath, statePath+".corrupt")
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

// Get returns the sandbox for id, or (nil, false) if not present.
func (s *Store) Get(id string) (*Sandbox, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sb, ok := s.sandboxes[id]
	return sb, ok
}

// List returns a snapshot of all sandboxes sorted by CreatedAt
// ascending. Pointers in the returned slice are the same objects in
// the store — callers must not mutate fields directly; use Update.
func (s *Store) List() []*Sandbox {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Sandbox, 0, len(s.sandboxes))
	for _, sb := range s.sandboxes {
		out = append(out, sb)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

// Update applies fn to the sandbox under the store's lock and
// persists. Returns false if the id isn't found. fn is called with
// the sandbox's own mutex held so concurrent UpdateSelf calls also
// serialize.
func (s *Store) Update(id string, fn func(*Sandbox)) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	sb, ok := s.sandboxes[id]
	if !ok {
		return false
	}
	sb.mu.Lock()
	fn(sb)
	sb.mu.Unlock()
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
