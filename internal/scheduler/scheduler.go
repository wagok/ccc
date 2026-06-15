// Package scheduler is the persistent timer engine for the smart-secretary mail
// system: schedule a future wake-up (a delivery-stage deadline or the
// secretary's own escalation timer), cancel it, and survive a CCC restart.
//
// The engine is deliberately generic. It stores timers and, when one is due,
// hands it to a fire callback; it knows nothing about tmux/Telegram — main.go
// wires the callback to the real wake mechanism. This keeps it unit-testable
// without a live system: drive fireDue() with a controlled clock instead of
// sleeping.
//
// Durability is at-least-once: a timer's file is removed only AFTER its fire
// callback returns, so a crash in between re-fires the timer on reload. For
// escalation deadlines a duplicate wake is harmless; a lost one is not.
package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Timer is a scheduled wake-up. The semantic fields (Agent/Ticket/Stage/Prompt)
// are opaque to the engine — it stores them and hands them back to the fire
// callback unchanged.
type Timer struct {
	ID      string            `json:"id"`
	FireAt  int64             `json:"fire_at"`          // unix seconds
	Kind    string            `json:"kind,omitempty"`   // delivery_timeout | escalation | wake | ...
	Agent   string            `json:"agent,omitempty"`  // agent this concerns / wake target
	Ticket  string            `json:"ticket,omitempty"` // associated letter ticket
	Stage   string            `json:"stage,omitempty"`  // delivery stage guarded: tmux | ack | reply
	Prompt  string            `json:"prompt,omitempty"` // text to deliver on fire
	Meta    map[string]string `json:"meta,omitempty"`   // extension point
	Created int64             `json:"created"`
}

// Scheduler stores timers in memory backed by one JSON file per timer.
type Scheduler struct {
	dir  string
	fire func(Timer)

	mu     sync.Mutex
	timers map[string]Timer
}

var idSeq int64

func newID() string {
	return fmt.Sprintf("tmr-%d-%04d", time.Now().UnixNano(), atomic.AddInt64(&idSeq, 1)%10000)
}

// New creates a scheduler backed by dir and reloads any persisted timers.
// fire may be nil (useful in tests that drive fireDue directly).
func New(dir string, fire func(Timer)) (*Scheduler, error) {
	if fire == nil {
		fire = func(Timer) {}
	}
	s := &Scheduler{dir: dir, fire: fire, timers: map[string]Timer{}}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Scheduler) load() error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			continue
		}
		var t Timer
		if json.Unmarshal(data, &t) == nil && t.ID != "" {
			s.timers[t.ID] = t
		}
	}
	return nil
}

func (s *Scheduler) path(id string) string {
	return filepath.Join(s.dir, id+".json")
}

// Schedule persists a timer and arms it. If t.ID is empty a new id is generated
// and returned. An existing id is overwritten (reschedule).
func (s *Scheduler) Schedule(t Timer) (string, error) {
	if t.ID == "" {
		t.ID = newID()
	}
	if t.Created == 0 {
		t.Created = time.Now().Unix()
	}
	if err := s.persist(t); err != nil {
		return "", err
	}
	s.mu.Lock()
	s.timers[t.ID] = t
	s.mu.Unlock()
	return t.ID, nil
}

func (s *Scheduler) persist(t Timer) error {
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	// Write-then-rename so a crash never leaves a half-written timer file.
	tmp := s.path(t.ID) + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path(t.ID))
}

// Cancel removes a timer. Unknown ids are a no-op.
func (s *Scheduler) Cancel(id string) error {
	s.mu.Lock()
	_, ok := s.timers[id]
	delete(s.timers, id)
	s.mu.Unlock()
	if !ok {
		return nil
	}
	if err := os.Remove(s.path(id)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// fireDue fires every timer whose FireAt <= now, removes it, and returns the
// fired timers (earliest-first). The fire callback runs OUTSIDE the lock so it
// may itself call Schedule/Cancel without deadlocking.
func (s *Scheduler) fireDue(now int64) []Timer {
	s.mu.Lock()
	var due []Timer
	for _, t := range s.timers {
		if t.FireAt <= now {
			due = append(due, t)
		}
	}
	s.mu.Unlock()

	sort.Slice(due, func(i, j int) bool {
		if due[i].FireAt != due[j].FireAt {
			return due[i].FireAt < due[j].FireAt
		}
		return due[i].ID < due[j].ID
	})

	for _, t := range due {
		s.fire(t)
		// Remove only if the callback did not reschedule this id to a new time
		// or cancel it. Comparing FireAt distinguishes a fresh reschedule.
		s.mu.Lock()
		if cur, ok := s.timers[t.ID]; ok && cur.FireAt == t.FireAt {
			delete(s.timers, t.ID)
			os.Remove(s.path(t.ID))
		}
		s.mu.Unlock()
	}
	return due
}

// Run fires due timers once per second until ctx is cancelled. Anything already
// overdue (e.g. a deadline that elapsed while CCC was down) fires immediately.
func (s *Scheduler) Run(ctx context.Context) {
	s.fireDue(time.Now().Unix())
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.fireDue(time.Now().Unix())
		}
	}
}

// List returns a snapshot of pending timers, ordered by id.
func (s *Scheduler) List() []Timer {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Timer, 0, len(s.timers))
	for _, t := range s.timers {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Get returns a timer by id.
func (s *Scheduler) Get(id string) (Timer, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.timers[id]
	return t, ok
}

// Pending returns the number of armed timers.
func (s *Scheduler) Pending() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.timers)
}
