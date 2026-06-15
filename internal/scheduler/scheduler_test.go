package scheduler

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// collector is a fire callback that records fired timers.
type collector struct {
	mu    sync.Mutex
	fired []Timer
}

func (c *collector) fire(t Timer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fired = append(c.fired, t)
}

func (c *collector) ids() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.fired))
	for i, t := range c.fired {
		out[i] = t.ID
	}
	return out
}

func TestScheduleAndFireDue(t *testing.T) {
	c := &collector{}
	s, err := New(t.TempDir(), c.fire)
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.Schedule(Timer{FireAt: 100, Kind: "ack", Agent: "devops", Ticket: "T1", Prompt: "stuck"})
	if err != nil {
		t.Fatal(err)
	}

	if got := s.fireDue(99); len(got) != 0 {
		t.Fatalf("fired before due: %+v", got)
	}
	if s.Pending() != 1 {
		t.Fatalf("pending = %d, want 1", s.Pending())
	}

	got := s.fireDue(100)
	if len(got) != 1 || got[0].ID != id {
		t.Fatalf("fireDue(100) = %+v, want [%s]", got, id)
	}
	if ids := c.ids(); len(ids) != 1 || ids[0] != id {
		t.Fatalf("callback ids = %v, want [%s]", ids, id)
	}
	// Carried payload survives the round-trip.
	if got[0].Agent != "devops" || got[0].Ticket != "T1" || got[0].Kind != "ack" {
		t.Fatalf("payload lost: %+v", got[0])
	}
	if s.Pending() != 0 {
		t.Fatalf("pending after fire = %d, want 0", s.Pending())
	}
	if _, err := os.Stat(s.path(id)); !os.IsNotExist(err) {
		t.Fatalf("timer file not removed after fire: %v", err)
	}
}

func TestCancel(t *testing.T) {
	c := &collector{}
	s, _ := New(t.TempDir(), c.fire)
	id, _ := s.Schedule(Timer{FireAt: 50})
	if err := s.Cancel(id); err != nil {
		t.Fatal(err)
	}
	if s.Pending() != 0 {
		t.Fatalf("pending after cancel = %d", s.Pending())
	}
	if got := s.fireDue(1000); len(got) != 0 {
		t.Fatalf("cancelled timer fired: %+v", got)
	}
	if _, err := os.Stat(s.path(id)); !os.IsNotExist(err) {
		t.Fatalf("timer file not removed after cancel")
	}
	// Cancelling an unknown id is a no-op.
	if err := s.Cancel("nope"); err != nil {
		t.Fatalf("cancel unknown: %v", err)
	}
}

func TestPersistenceReload(t *testing.T) {
	dir := t.TempDir()
	s1, _ := New(dir, nil)
	id1, _ := s1.Schedule(Timer{FireAt: 200, Ticket: "A"})
	id2, _ := s1.Schedule(Timer{FireAt: 300, Ticket: "B"})

	// A fresh scheduler on the same dir reloads both pending timers.
	c := &collector{}
	s2, err := New(dir, c.fire)
	if err != nil {
		t.Fatal(err)
	}
	if s2.Pending() != 2 {
		t.Fatalf("reloaded pending = %d, want 2", s2.Pending())
	}
	if _, ok := s2.Get(id1); !ok {
		t.Fatalf("id1 not reloaded")
	}

	got := s2.fireDue(250) // only id1 (FireAt 200) is due
	if len(got) != 1 || got[0].ID != id1 {
		t.Fatalf("fireDue(250) = %+v, want [%s]", got, id1)
	}
	if s2.Pending() != 1 {
		t.Fatalf("pending = %d, want 1 (id2 still armed)", s2.Pending())
	}
	_ = id2
}

func TestOverdueFiresImmediately(t *testing.T) {
	c := &collector{}
	s, _ := New(t.TempDir(), c.fire)
	// A deadline that elapsed while CCC was "down".
	s.Schedule(Timer{FireAt: 10})
	if got := s.fireDue(1_000_000); len(got) != 1 {
		t.Fatalf("overdue timer did not fire: %+v", got)
	}
}

func TestFireOrderEarliestFirst(t *testing.T) {
	c := &collector{}
	s, _ := New(t.TempDir(), c.fire)
	late, _ := s.Schedule(Timer{ID: "late", FireAt: 90})
	early, _ := s.Schedule(Timer{ID: "early", FireAt: 80})
	got := s.fireDue(100)
	if len(got) != 2 || got[0].ID != early || got[1].ID != late {
		t.Fatalf("fire order = %v, want [early late]", c.ids())
	}
}

func TestCallbackMayRescheduleOutsideLock(t *testing.T) {
	dir := t.TempDir()
	var s *Scheduler
	var chained string
	// The fire callback schedules a follow-up timer (chained escalation). This
	// would deadlock if fire ran under the lock.
	s, _ = New(dir, func(tm Timer) {
		if tm.Kind == "first" {
			chained, _ = s.Schedule(Timer{FireAt: tm.FireAt + 100, Kind: "second"})
		}
	})
	s.Schedule(Timer{FireAt: 100, Kind: "first"})
	s.fireDue(100)

	if chained == "" {
		t.Fatal("callback did not schedule the follow-up")
	}
	if s.Pending() != 1 {
		t.Fatalf("pending = %d, want 1 (the chained timer)", s.Pending())
	}
	if got, ok := s.Get(chained); !ok || got.Kind != "second" {
		t.Fatalf("chained timer missing or wrong: %+v ok=%v", got, ok)
	}
	// The chained timer survived (its file exists) and the first was removed.
	if _, err := os.Stat(s.path(chained)); err != nil {
		t.Fatalf("chained timer not persisted: %v", err)
	}
}

func TestCallbackCancelKeepsConsistent(t *testing.T) {
	dir := t.TempDir()
	var s *Scheduler
	var victim string
	s, _ = New(dir, func(tm Timer) {
		if tm.Kind == "killer" && victim != "" {
			s.Cancel(victim)
		}
	})
	victim, _ = s.Schedule(Timer{FireAt: 200, Kind: "victim"})
	s.Schedule(Timer{FireAt: 100, Kind: "killer"})

	s.fireDue(100) // fires killer, which cancels victim (not yet due)
	if _, ok := s.Get(victim); ok {
		t.Fatal("victim should have been cancelled by the callback")
	}
	if s.Pending() != 0 {
		t.Fatalf("pending = %d, want 0", s.Pending())
	}
}

func TestNewIDUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := newID()
		if seen[id] {
			t.Fatalf("duplicate id: %s", id)
		}
		seen[id] = true
	}
}

func TestReloadIgnoresTmpAndJunk(t *testing.T) {
	dir := t.TempDir()
	// Stray temp/non-json files must not break reload.
	os.WriteFile(filepath.Join(dir, "tmr-x.json.tmp"), []byte("{partial"), 0644)
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hello"), 0644)
	s, _ := New(dir, nil)
	s.Schedule(Timer{ID: "real", FireAt: 5})
	s2, err := New(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s2.Pending() != 1 {
		t.Fatalf("pending = %d, want 1", s2.Pending())
	}
}
