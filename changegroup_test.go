package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMoveHistory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	oldDir := filepath.Dir(getHistoryDir(100)) // ~/.ccc/history/100
	if err := os.MkdirAll(filepath.Join(oldDir, "messages"), 0755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(oldDir, "messages", "x.jsonl"), []byte("{}"), 0644)

	if err := moveHistory(100, 200); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Fatal("old history dir should be gone after move")
	}
	newFile := filepath.Join(filepath.Dir(getHistoryDir(200)), "messages", "x.jsonl")
	if _, err := os.Stat(newFile); err != nil {
		t.Fatalf("moved history missing: %v", err)
	}
	// No source -> no-op, no error.
	if err := moveHistory(300, 400); err != nil {
		t.Fatalf("move with no source: %v", err)
	}
}
