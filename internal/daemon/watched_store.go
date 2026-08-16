package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"src.solsynth.dev/solsynth/maidcafe/internal/config"
)

// watchedProcessStore holds the daemon-side watched-process list: seeded from
// config, mutated through the watched-processes API, persisted atomically so
// user additions survive restarts. The persisted file is authoritative once
// it exists (config entries are only a first-run seed); the file lives under
// the daemon's data directory so a host upgrade never resets it.
type watchedProcessStore struct {
	mu    sync.Mutex
	path  string
	names []string
}

func newWatchedProcessStore(path string, seeded []string) *watchedProcessStore {
	store := &watchedProcessStore{path: path}
	if raw, err := os.ReadFile(path); err == nil {
		var names []string
		if json.Unmarshal(raw, &names) == nil && names != nil {
			store.names = normalizeWatchedNames(names)
			return store
		}
	}
	store.names = normalizeWatchedNames(seeded)
	if store.path != "" && len(store.names) > 0 {
		_ = store.saveLocked()
	}
	return store
}

func normalizeWatchedNames(names []string) []string {
	seen := make(map[string]struct{}, len(names))
	out := make([]string, 0, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" || !config.ValidWatchedProcessName(name) {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// List returns a copy of the watched names, sorted.
func (s *watchedProcessStore) List() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.names...)
}

// Add registers a watched process name (already validated by the caller) and
// returns the updated sorted list. Adding an existing name is a no-op.
func (s *watchedProcessStore) Add(name string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.names {
		if existing == name {
			return append([]string(nil), s.names...), nil
		}
	}
	s.names = append(s.names, name)
	sort.Strings(s.names)
	if err := s.saveLocked(); err != nil {
		return nil, err
	}
	return append([]string(nil), s.names...), nil
}

// Remove deletes a watched process name and returns the updated sorted list.
// Removing an unknown name is a no-op.
func (s *watchedProcessStore) Remove(name string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.names[:0]
	found := false
	for _, existing := range s.names {
		if existing == name {
			found = true
			continue
		}
		out = append(out, existing)
	}
	if !found {
		return append([]string(nil), s.names...), nil
	}
	s.names = out
	if err := s.saveLocked(); err != nil {
		return nil, err
	}
	return append([]string(nil), s.names...), nil
}

// saveLocked writes the list atomically (temp file + rename). Callers hold mu.
func (s *watchedProcessStore) saveLocked() error {
	if s.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(s.names)
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
