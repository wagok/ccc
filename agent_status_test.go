package main

import (
	"testing"
	"time"
)

func TestAgentStatusRoundtripAndSincePreservation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if _, ok := readAgentStatus("PM"); ok {
		t.Fatal("expected no status before any write")
	}

	writeAgentStatus("PM", "working", 1000)
	st, ok := readAgentStatus("PM")
	if !ok || st.Status != "working" || st.Since != 1000 {
		t.Fatalf("after working write: %+v ok=%v", st, ok)
	}

	// Same status again with a later timestamp must PRESERVE Since.
	writeAgentStatus("PM", "working", 1050)
	st, _ = readAgentStatus("PM")
	if st.Since != 1000 {
		t.Errorf("Since should be preserved on unchanged status, got %d", st.Since)
	}

	// Status change resets Since.
	writeAgentStatus("PM", "idle", 1200)
	st, _ = readAgentStatus("PM")
	if st.Status != "idle" || st.Since != 1200 {
		t.Errorf("after idle write: %+v", st)
	}
}

func TestAgentStatusNameEscaping(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// Remote agent names contain ':' and paths may contain '/'.
	name := "dell17:housekeeper"
	writeAgentStatus(name, "idle", 500)
	st, ok := readAgentStatus(name)
	if !ok || st.Agent != name || st.Status != "idle" {
		t.Fatalf("escaped-name roundtrip failed: %+v ok=%v", st, ok)
	}
}

func TestIdleSubStore(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if idleHasSubscribers("PM") {
		t.Fatal("no subscribers expected initially")
	}

	s1 := idleSub{ID: "a", Subscriber: "QA", Target: "PM", OneShot: true, Created: time.Now().Unix()}
	s2 := idleSub{ID: "b", Subscriber: "UI", Target: "PM", OneShot: false, Created: time.Now().Unix()}
	s3 := idleSub{ID: "c", Subscriber: "QA", Target: "engine", OneShot: true, Created: time.Now().Unix()}
	for _, s := range []idleSub{s1, s2, s3} {
		if err := writeIdleSub(s); err != nil {
			t.Fatalf("writeIdleSub %s: %v", s.ID, err)
		}
	}

	if got := idleSubsForTarget("PM"); len(got) != 2 {
		t.Errorf("PM should have 2 subscribers, got %d", len(got))
	}
	if got := idleSubsForTarget("engine"); len(got) != 1 {
		t.Errorf("engine should have 1 subscriber, got %d", len(got))
	}
	if !idleHasSubscribers("PM") || !idleHasSubscribers("engine") {
		t.Error("idleHasSubscribers should be true for PM and engine")
	}
	if idleHasSubscribers("UI") {
		t.Error("UI has no subscribers")
	}

	// Persistent() mirrors !OneShot.
	if s1.Persistent() || !s2.Persistent() {
		t.Error("Persistent() wrong")
	}

	deleteIdleSub("a")
	if got := idleSubsForTarget("PM"); len(got) != 1 || got[0].ID != "b" {
		t.Errorf("after delete, PM should have only sub b, got %+v", got)
	}
}

// A persistent subscription re-fires forever, so it needs both a way out and a
// deadline. Before this, notify_when_free could only be created — three real
// subscriptions outlived their topics by 45–59 days with no lever on any side.
func TestIdleSubExpiry(t *testing.T) {
	now := time.Now().Unix()

	oneShot := idleSub{ID: "o", Subscriber: "A", Target: "B", OneShot: true, Created: now - 100*86400}
	if oneShot.Expired(now) {
		t.Error("one-shot subscriptions must never expire — they are removed on first fire")
	}

	fresh := idleSub{ID: "f", Subscriber: "A", Target: "B", Created: now - 3*86400}
	if fresh.Expired(now) {
		t.Error("a 3-day-old persistent subscription is still live")
	}

	stale := idleSub{ID: "s", Subscriber: "A", Target: "B", Created: now - 15*86400}
	if !stale.Expired(now) {
		t.Errorf("a 15-day-old persistent subscription must expire (limit %d days)", idleSubMaxAgeSecs/86400)
	}

	// A subscription with no creation stamp must not be treated as infinitely old
	// and silently retired.
	if (idleSub{ID: "z", Subscriber: "A", Target: "B"}).Expired(now) {
		t.Error("a subscription without a Created stamp should not expire")
	}
}

func TestIdleSubsForSubscriber(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	for _, s := range []idleSub{
		{ID: "1", Subscriber: "contracts", Target: "openarx", Created: 100},
		{ID: "2", Subscriber: "contracts", Target: "portal", Created: 100},
		{ID: "3", Subscriber: "methodist", Target: "openarx", Created: 100},
	} {
		if err := writeIdleSub(s); err != nil {
			t.Fatalf("write %s: %v", s.ID, err)
		}
	}

	mine := idleSubsForSubscriber("contracts")
	if len(mine) != 2 {
		t.Fatalf("contracts should own 2 subscriptions, got %d", len(mine))
	}
	for _, s := range mine {
		if s.Subscriber != "contracts" {
			t.Errorf("leaked another agent's subscription: %+v", s)
		}
	}
	if len(idleSubsForSubscriber("nobody")) != 0 {
		t.Error("an agent with no subscriptions should get none")
	}
	// The by-target view must stay independent of the by-subscriber one.
	if got := idleSubsForTarget("openarx"); len(got) != 2 {
		t.Errorf("openarx should have 2 watchers, got %d", len(got))
	}
}
