package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kidandcat/ccc/internal/mail"
)

func TestPrepareSecretaryDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	dir, err := prepareSecretaryDir()
	if err != nil {
		t.Fatal(err)
	}
	if dir != filepath.Join(home, ".ccc", "secretary") {
		t.Fatalf("dir = %s", dir)
	}
	for _, sub := range []string{"inbox", "archive"} {
		if fi, err := os.Stat(filepath.Join(dir, sub)); err != nil || !fi.IsDir() {
			t.Fatalf("subdir %s missing", sub)
		}
	}

	// The instruction is installed and is the real manual.
	claude, err := os.ReadFile(filepath.Join(dir, "CLAUDE.md"))
	if err != nil {
		t.Fatalf("CLAUDE.md not installed: %v", err)
	}
	if !strings.Contains(string(claude), "Smart Secretary") {
		t.Fatalf("CLAUDE.md content unexpected")
	}

	// The dotfile .mcp.json is embedded (via all:) and installed.
	mcp, err := os.ReadFile(filepath.Join(dir, ".mcp.json"))
	if err != nil {
		t.Fatalf(".mcp.json not installed: %v", err)
	}
	if !strings.Contains(string(mcp), "mcp-secretary") {
		t.Fatalf(".mcp.json content unexpected: %s", mcp)
	}
}

func TestInstallSecretaryTemplateNoClobber(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir, err := prepareSecretaryDir()
	if err != nil {
		t.Fatal(err)
	}

	// Simulate a Vlad edit, then re-run install (as happens on every restart).
	edited := filepath.Join(dir, "CLAUDE.md")
	if err := os.WriteFile(edited, []byte("EDITED BY VLAD"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := installSecretaryTemplate(dir); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(edited)
	if string(got) != "EDITED BY VLAD" {
		t.Fatalf("install clobbered an existing file: %q", got)
	}
}

func TestSecretaryEnabledMarker(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if _, err := prepareSecretaryDir(); err != nil { // marker lives in the dir
		t.Fatal(err)
	}

	if secretaryEnabled() {
		t.Fatal("freshly prepared secretary must not be enabled")
	}
	if err := setSecretaryEnabled(true); err != nil {
		t.Fatal(err)
	}
	if !secretaryEnabled() {
		t.Fatal("expected enabled after setSecretaryEnabled(true)")
	}
	if _, err := os.Stat(secretaryEnabledMarker()); err != nil {
		t.Fatalf("marker file missing: %v", err)
	}
	if err := setSecretaryEnabled(false); err != nil {
		t.Fatal(err)
	}
	if secretaryEnabled() {
		t.Fatal("expected disabled after setSecretaryEnabled(false)")
	}
	// Disabling again is a no-op, not an error.
	if err := setSecretaryEnabled(false); err != nil {
		t.Fatalf("double-disable: %v", err)
	}
}

func TestSecretaryMailboxPathMatches(t *testing.T) {
	// Bootstrap and the mail subsystem must agree on the secretary's directory.
	t.Setenv("HOME", t.TempDir())
	dir, _ := prepareSecretaryDir()
	if dir != mail.MailboxDir(mail.SecretaryAgent) {
		t.Fatalf("bootstrap dir %s != mailbox dir %s", dir, mail.MailboxDir(mail.SecretaryAgent))
	}
}
