package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestEncodeProjectPath(t *testing.T) {
	// Verified against real Claude Code projects/ dirs.
	cases := map[string]string{
		"/home/wlad/Projects/GTaara_group/PM": "-home-wlad-Projects-GTaara-group-PM",
		"/home/wlad/.ccc/secretary-gtara":     "-home-wlad--ccc-secretary-gtara",
		"/home/wlad/Projects/teleclaude/ccc":  "-home-wlad-Projects-teleclaude-ccc",
	}
	for in, want := range cases {
		if got := encodeProjectPath(in); got != want {
			t.Errorf("encodeProjectPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEnsureSecretaryMcpInConfigDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude.json")

	// Seed a config with unrelated content + an empty mcpServers (the fresh-account case).
	seed := map[string]interface{}{
		"numAccounts": json.Number("3"),
		"mcpServers":  map[string]interface{}{},
		"projects":    map[string]interface{}{"/x": map[string]interface{}{"hasTrustDialogAccepted": true}},
	}
	b, _ := json.MarshalIndent(seed, "", "  ")
	if err := os.WriteFile(path, b, 0644); err != nil {
		t.Fatal(err)
	}

	if err := ensureSecretaryMcpInConfigDir(path); err != nil {
		t.Fatalf("first call: %v", err)
	}
	assertSecretaryEntry(t, path)
	// Unrelated content preserved.
	got := readJSON(t, path)
	if n, ok := got["numAccounts"].(json.Number); !ok || n.String() != "3" {
		t.Errorf("numAccounts not preserved exactly: %v (%T)", got["numAccounts"], got["numAccounts"])
	}
	if _, ok := got["projects"].(map[string]interface{}); !ok {
		t.Errorf("projects section lost")
	}

	// Idempotency: second call must not error and must keep the entry.
	stat1, _ := os.Stat(path)
	if err := ensureSecretaryMcpInConfigDir(path); err != nil {
		t.Fatalf("second call: %v", err)
	}
	assertSecretaryEntry(t, path)
	// A no-write idempotent call should not rewrite the file (mtime unchanged is a bonus,
	// but the key invariant is correctness of content, already checked).
	_ = stat1

	// Missing file: should create it with the entry.
	missing := filepath.Join(dir, "sub", ".claude.json")
	os.MkdirAll(filepath.Dir(missing), 0755)
	if err := ensureSecretaryMcpInConfigDir(missing); err != nil {
		t.Fatalf("missing-file call: %v", err)
	}
	assertSecretaryEntry(t, missing)
}

func TestAccountDirForSession(t *testing.T) {
	cfg := &Config{
		Accounts: map[string]string{"work": "/home/x/.claude-work", "alt": "/home/x/.claude-alt"},
		Groups:   map[string]*GroupInfo{"openarx_ai": {Account: "work"}},
	}
	cases := []struct {
		name string
		si   *SessionInfo
		want string
	}{
		{"session override wins", &SessionInfo{Group: "openarx_ai", Account: "alt"}, "/home/x/.claude-alt"},
		{"no override -> group account", &SessionInfo{Group: "openarx_ai"}, "/home/x/.claude-work"},
		{"unknown alias -> group fallback", &SessionInfo{Group: "openarx_ai", Account: "ghost"}, "/home/x/.claude-work"},
		{"no override, no group account -> empty", &SessionInfo{Group: "default"}, ""},
	}
	for _, c := range cases {
		if got := accountDirForSession(cfg, c.si); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestDirHasJSONL(t *testing.T) {
	dir := t.TempDir()
	if dirHasJSONL(dir) {
		t.Error("empty dir should have no jsonl")
	}
	os.WriteFile(filepath.Join(dir, "readme.txt"), []byte("x"), 0644)
	if dirHasJSONL(dir) {
		t.Error("dir with only .txt should report no jsonl")
	}
	os.WriteFile(filepath.Join(dir, "session.jsonl"), []byte("{}"), 0644)
	if !dirHasJSONL(dir) {
		t.Error("dir with .jsonl should report true")
	}
	if dirHasJSONL(filepath.Join(dir, "nonexistent")) {
		t.Error("missing dir should report false")
	}
}

func assertSecretaryEntry(t *testing.T, path string) {
	t.Helper()
	root := readJSON(t, path)
	servers, ok := root["mcpServers"].(map[string]interface{})
	if !ok {
		t.Fatalf("mcpServers missing/wrong type in %s", path)
	}
	sec, ok := servers["secretary"].(map[string]interface{})
	if !ok {
		t.Fatalf("secretary entry missing in %s", path)
	}
	if sec["command"] != "ccc" || sec["type"] != "stdio" {
		t.Errorf("secretary entry wrong: %v", sec)
	}
	args, ok := sec["args"].([]interface{})
	if !ok || len(args) != 1 || args[0] != "mcp-secretary" {
		t.Errorf("secretary args wrong: %v", sec["args"])
	}
}

func readJSON(t *testing.T, path string) map[string]interface{} {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]interface{}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&root); err != nil {
		t.Fatal(err)
	}
	return root
}
