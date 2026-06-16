package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSessionGroup(t *testing.T) {
	if SessionGroup(nil) != DefaultGroup {
		t.Fatal("nil session must be default group")
	}
	if SessionGroup(&SessionInfo{}) != DefaultGroup {
		t.Fatal("empty Group must be default")
	}
	if SessionGroup(&SessionInfo{Group: "research"}) != "research" {
		t.Fatal("explicit Group not returned")
	}
}

func TestGroupChatIDBackCompat(t *testing.T) {
	// No Groups configured: default resolves to the original GroupID.
	cfg := &Config{GroupID: 111}
	if GroupChatID(cfg, "") != 111 || GroupChatID(cfg, DefaultGroup) != 111 {
		t.Fatalf("default must fall back to GroupID 111")
	}
	if GroupChatID(cfg, "research") != 0 {
		t.Fatal("unknown group must be 0")
	}
}

func TestGroupChatIDNamedAndOverride(t *testing.T) {
	cfg := &Config{
		GroupID: 111,
		Groups: map[string]*GroupInfo{
			"research": {ChatID: 222},
			"default":  {ChatID: 999}, // explicit default overrides GroupID
		},
	}
	if GroupChatID(cfg, "research") != 222 {
		t.Fatalf("research = %d, want 222", GroupChatID(cfg, "research"))
	}
	if GroupChatID(cfg, "default") != 999 {
		t.Fatalf("explicit default = %d, want 999", GroupChatID(cfg, "default"))
	}
	if !GroupExists(cfg, "research") || GroupExists(cfg, "nope") {
		t.Fatal("GroupExists wrong")
	}
}

func TestSessionGroupChatID(t *testing.T) {
	cfg := &Config{
		GroupID: 111,
		Groups:  map[string]*GroupInfo{"research": {ChatID: 222}},
		Sessions: map[string]*SessionInfo{
			"backend":  {Path: "/p/backend"},                    // default group
			"paper":    {Path: "/p/paper", Group: "research"},   // research group
		},
	}
	if got := SessionGroupChatID(cfg, "backend"); got != 111 {
		t.Fatalf("backend group chat = %d, want 111 (default)", got)
	}
	if got := SessionGroupChatID(cfg, "paper"); got != 222 {
		t.Fatalf("paper group chat = %d, want 222 (research)", got)
	}
}

func TestGetSessionByGroupTopic(t *testing.T) {
	cfg := &Config{
		GroupID: 111,
		Groups:  map[string]*GroupInfo{"research": {ChatID: 222}},
		Sessions: map[string]*SessionInfo{
			"alpha": {TopicID: 5},                    // default group (111), topic 5
			"beta":  {TopicID: 5, Group: "research"}, // research group (222), SAME topic id 5
		},
	}
	// Same topic id in two groups must be disambiguated by chat id.
	if got := GetSessionByGroupTopic(cfg, 111, 5); got != "alpha" {
		t.Fatalf("(111,5) = %q, want alpha", got)
	}
	if got := GetSessionByGroupTopic(cfg, 222, 5); got != "beta" {
		t.Fatalf("(222,5) = %q, want beta", got)
	}
	if got := GetSessionByGroupTopic(cfg, 999, 5); got != "" {
		t.Fatalf("(999,5) = %q, want empty", got)
	}
}

func TestLoadSaveRoundtripWithGroups(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	in := &Config{
		BotToken: "t", GroupID: 111,
		Groups: map[string]*GroupInfo{"research": {ChatID: 222, Name: "Research"}},
		Sessions: map[string]*SessionInfo{
			"paper": {TopicID: 5, Path: "/p/paper", Group: "research"},
		},
	}
	if err := Save(in); err != nil {
		t.Fatal(err)
	}
	out, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if out.Groups["research"].ChatID != 222 || out.Groups["research"].Name != "Research" {
		t.Fatalf("group not preserved: %+v", out.Groups["research"])
	}
	if out.Sessions["paper"].Group != "research" {
		t.Fatalf("session group not preserved: %+v", out.Sessions["paper"])
	}
}

func TestLoadBackCompatNoGroups(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// A pre-groups config: no "groups", sessions without "group".
	old := `{"bot_token":"t","group_id":111,"sessions":{"backend":{"topic_id":5,"path":"/p/backend"}}}`
	if err := os.WriteFile(filepath.Join(home, ".ccc.json"), []byte(old), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Groups != nil {
		t.Fatal("old config should load with nil Groups")
	}
	// Everything still resolves to the default group = GroupID.
	if SessionGroupChatID(cfg, "backend") != 111 {
		t.Fatal("back-compat: existing session must resolve to default group 111")
	}
}
