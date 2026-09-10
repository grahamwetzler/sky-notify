package main

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// State is the cooldown ledger, keyed by "<hex>|<trigger>".
//
// The in-memory map is authoritative and the disk write is best-effort-with-retry. If
// ntfy succeeds but persistence fails we still advance the cooldown: re-queueing the
// alert would republish the same notification every poll for as long as the disk stays
// unwritable. One duplicate after a restart beats a pager storm.
type State struct {
	path string
	Now  func() time.Time

	// flushMu serializes disk writes so two concurrent flushes cannot land out of
	// order and leave an older snapshot as the final one on disk.
	flushMu sync.Mutex

	mu       sync.Mutex
	last     map[string]time.Time
	gen      uint64
	dirty    bool
	writeErr error
}

func NewState(dir string) *State {
	return &State{
		path: filepath.Join(dir, "state.json"),
		Now:  time.Now,
		last: map[string]time.Time{},
	}
}

// Load reads the cooldown ledger. A corrupt or unreadable file is a warning and an
// empty map: losing cooldown history is a nuisance, refusing to start is an outage.
func (s *State) Load(cooldown time.Duration) {
	b, err := os.ReadFile(s.path)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("state unreadable, starting empty", "path", s.path, "err", err)
		}
		return
	}
	var m map[string]time.Time
	if err := json.Unmarshal(b, &m); err != nil {
		slog.Warn("state corrupt, starting empty", "path", s.path, "err", err)
		return
	}
	now := s.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, t := range m {
		if now.Sub(t) < cooldown {
			s.last[k] = t
		}
	}
	slog.Info("loaded cooldown state", "path", s.path, "entries", len(s.last))
}

func (s *State) Eligible(key string, cooldown time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.last[key]
	return !ok || s.Now().Sub(t) >= cooldown
}

// Record advances the cooldown for every key and persists synchronously. The write
// error is reported (and surfaced in /healthz) but never returned as a reason to retry
// the notification.
func (s *State) Record(keys []string, cooldown time.Duration) {
	now := s.Now()
	s.mu.Lock()
	for _, k := range keys {
		s.last[k] = now
	}
	s.prune(cooldown, now)
	s.gen++
	s.dirty = true
	s.mu.Unlock()
	s.Flush(cooldown)
}

// prune must be called with the lock held.
func (s *State) prune(cooldown time.Duration, now time.Time) {
	for k, t := range s.last {
		if now.Sub(t) >= cooldown {
			delete(s.last, k)
		}
	}
}

// Flush persists the ledger if it is dirty. Safe to call on a timer to retry a
// previously failed write.
func (s *State) Flush(cooldown time.Duration) {
	s.flushMu.Lock()
	defer s.flushMu.Unlock()

	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return
	}
	gen := s.gen
	blob, err := json.Marshal(s.last)
	s.mu.Unlock()
	if err != nil {
		s.setWriteErr(err)
		return
	}
	if err := writeFileDurable(s.path, blob); err != nil {
		slog.Error("state write failed; cooldown held in memory only", "path", s.path, "err", err)
		s.setWriteErr(err)
		return
	}
	s.mu.Lock()
	// Only clear dirty if nothing changed while we were writing, so a mutation that
	// raced this write is not mistaken for persisted.
	if s.gen == gen {
		s.dirty = false
	}
	s.writeErr = nil
	s.mu.Unlock()
}

func (s *State) setWriteErr(err error) {
	s.mu.Lock()
	s.writeErr = err
	s.mu.Unlock()
}

func (s *State) WriteErr() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeErr
}
