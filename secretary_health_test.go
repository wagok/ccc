package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeInboxLetter(t *testing.T, dir, ticket string, ageSecs int64) {
	t.Helper()
	inbox := filepath.Join(dir, "inbox")
	os.MkdirAll(inbox, 0755)
	p := filepath.Join(inbox, ticket+".json")
	if err := os.WriteFile(p, []byte(`{"ticket":"`+ticket+`"}`), 0644); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-time.Duration(ageSecs) * time.Second)
	os.Chtimes(p, when, when)
}

func TestSecretaryStuckTickets(t *testing.T) {
	dir := t.TempDir()
	writeInboxLetter(t, dir, "T-old-unqueued", 1200)  // 20 min, never queued -> STUCK
	writeInboxLetter(t, dir, "T-old-queued", 1200)    // 20 min, but queued  -> ok
	writeInboxLetter(t, dir, "T-recent-unqueued", 60) // 1 min, too new       -> ok

	os.WriteFile(filepath.Join(dir, "journal.jsonl"), []byte(
		`{"ticket":"T-old-queued","event":"received"}`+"\n"+
			`{"ticket":"T-old-queued","event":"queued"}`+"\n"+
			`{"ticket":"T-old-unqueued","event":"received"}`+"\n"+
			`{"ticket":"T-recent-unqueued","event":"received"}`+"\n"), 0644)

	stuck := secretaryStuckTickets(dir, 600) // threshold 10 min
	if len(stuck) != 1 || stuck[0] != "T-old-unqueued" {
		t.Fatalf("expected [T-old-unqueued], got %v", stuck)
	}
}

func TestSecretaryStuckTickets_DeliveredAndEmpty(t *testing.T) {
	// Empty inbox -> nothing stuck.
	empty := t.TempDir()
	os.MkdirAll(filepath.Join(empty, "inbox"), 0755)
	if s := secretaryStuckTickets(empty, 600); len(s) != 0 {
		t.Errorf("empty inbox should yield no stuck, got %v", s)
	}

	// 'delivered' counts as processed (not stuck); a missing journal entry for an
	// old letter counts as stuck.
	dir := t.TempDir()
	writeInboxLetter(t, dir, "D-delivered", 1200)
	writeInboxLetter(t, dir, "X-nojournal", 1200)
	os.WriteFile(filepath.Join(dir, "journal.jsonl"),
		[]byte(`{"ticket":"D-delivered","event":"delivered"}`+"\n"), 0644)

	stuck := secretaryStuckTickets(dir, 600)
	if len(stuck) != 1 || stuck[0] != "X-nojournal" {
		t.Fatalf("expected only [X-nojournal] stuck, got %v", stuck)
	}
}
