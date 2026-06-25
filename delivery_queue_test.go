package main

import (
	"path/filepath"
	"testing"

	"github.com/kidandcat/ccc/internal/scheduler"
)

// TestDeliverySpacing verifies the global queue spaces consecutive deliveries by
// deliverSpacingSecs and re-seeds the cursor from pending timers after a restart.
func TestDeliverySpacing(t *testing.T) {
	dir := t.TempDir()
	s, err := scheduler.New(filepath.Join(dir, "sched"), func(scheduler.Timer) {})
	if err != nil {
		t.Fatal(err)
	}
	mailScheduler = s
	lastDeliverSlot = 0
	defer func() { mailScheduler = nil; lastDeliverSlot = 0 }()

	// Three rapid deliveries: first ~now, then +30, +60.
	s1 := enqueueDelivery("a", "T1", "x", "secretary", "", "f", "s")
	s2 := enqueueDelivery("b", "T2", "x", "secretary", "", "f", "s")
	s3 := enqueueDelivery("c", "T3", "x", "secretary", "", "f", "s")
	if s2-s1 != deliverSpacingSecs || s3-s2 != deliverSpacingSecs {
		t.Fatalf("spacing wrong: s1=%d s2=%d s3=%d (want gaps of %d)", s1, s2, s3, deliverSpacingSecs)
	}

	// Simulate a restart: drop in-memory cursor, reload from pending timers.
	lastDeliverSlot = 0
	restoreDeliverSlot()
	if lastDeliverSlot != s3 {
		t.Fatalf("restoreDeliverSlot = %d, want %d (latest pending)", lastDeliverSlot, s3)
	}
	// Next delivery continues spacing after the restored cursor.
	s4 := enqueueDelivery("d", "T4", "x", "secretary", "", "f", "s")
	if s4-s3 != deliverSpacingSecs {
		t.Fatalf("post-restart spacing wrong: s3=%d s4=%d", s3, s4)
	}
}
