package main

import (
	"strings"
	"testing"

	"github.com/kidandcat/ccc/internal/config"
)

// A remote agent could only ever be registered into the default group and moved
// afterwards. Registration now carries the group and account, so a bad value
// must fail loudly here rather than silently creating the agent in the wrong
// place — which is the failure the old flow made easy.
func TestRegisterRemoteSessionRejectsUnknownTargets(t *testing.T) {
	cfg := &Config{
		GroupID:  -100123,
		Accounts: map[string]string{"mornhouse": "/home/wlad/.claude-mornhouse"},
		Groups:   map[string]*config.GroupInfo{"irisha_book": {ChatID: -100456}},
		Sessions: map[string]*config.SessionInfo{},
	}

	if _, err := registerRemoteSession(cfg, "msi:destiny", "/p", "msi", "irisha-books", ""); err == nil {
		t.Error("a misspelled group must be rejected, not silently ignored")
	} else if !strings.Contains(err.Error(), "unknown group") {
		t.Errorf("unhelpful error: %v", err)
	}

	if _, err := registerRemoteSession(cfg, "msi:destiny", "/p", "msi", "irisha_book", "nosuch"); err == nil {
		t.Error("an unknown account must be rejected")
	} else if !strings.Contains(err.Error(), "unknown account") {
		t.Errorf("unhelpful error: %v", err)
	}

	// Nothing may be written to the config on a rejected registration.
	if len(cfg.Sessions) != 0 {
		t.Errorf("a rejected registration left state behind: %v", cfg.Sessions)
	}
}
