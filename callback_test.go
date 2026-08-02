package main

import (
	"reflect"
	"testing"

	"github.com/kidandcat/ccc/internal/config"
)

func TestMultiSelectSubmitKeys(t *testing.T) {
	// Go(0)+Rust(2) of 3 options — the manually verified sequence.
	got := multiSelectSubmitKeys([]bool{true, false, true}, 3)
	want := []string{"Enter", "Down", "Down", "Enter", "Right", "Enter"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Go+Rust = %v, want %v", got, want)
	}
	// First option only.
	if got := multiSelectSubmitKeys([]bool{true, false, false}, 3); !reflect.DeepEqual(got, []string{"Enter", "Down", "Down", "Right", "Enter"}) {
		t.Fatalf("first only = %v", got)
	}
	// Last option only.
	if got := multiSelectSubmitKeys([]bool{false, false, true}, 3); !reflect.DeepEqual(got, []string{"Down", "Down", "Enter", "Right", "Enter"}) {
		t.Fatalf("last only = %v", got)
	}
	// Nothing selected — just open Submit tab and confirm.
	if got := multiSelectSubmitKeys([]bool{false, false, false}, 3); !reflect.DeepEqual(got, []string{"Down", "Down", "Right", "Enter"}) {
		t.Fatalf("none = %v", got)
	}
	// All selected.
	if got := multiSelectSubmitKeys([]bool{true, true}, 2); !reflect.DeepEqual(got, []string{"Enter", "Down", "Enter", "Right", "Enter"}) {
		t.Fatalf("all = %v", got)
	}
}

// A remote agent's session name contains ':' ("msi:dream"), which used to make
// the left-anchored split resolve the session to just the host — the keystrokes
// then went nowhere and the button press silently did nothing.
func TestParseChoiceCallback(t *testing.T) {
	cfg := &Config{Sessions: map[string]*config.SessionInfo{
		"dream":     {},
		"msi:dream": {Host: "msi"},
	}}

	tests := []struct {
		name    string
		data    string
		session string
		qIdx    int
		total   int
		optIdx  int
	}{
		{"local", "dream:0:1:2", "dream", 0, 1, 2},
		{"host-prefixed", "msi:dream:0:1:2", "msi:dream", 0, 1, 2},
		{"host-prefixed, later question", "msi:dream:2:3:1", "msi:dream", 2, 3, 1},
		{"legacy local", "dream:1:2", "dream", 1, 0, 2},
		{"legacy host-prefixed", "msi:dream:1:2", "msi:dream", 1, 0, 2},
		// Session no longer in the registry: still parsed as the current format
		// so the keys reach a live tmux session of that name.
		{"unknown session", "msi:ghost:0:1:2", "msi:ghost", 0, 1, 2},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			session, qIdx, total, optIdx, ok := parseChoiceCallback(cfg, tc.data)
			if !ok {
				t.Fatalf("%q: not parsed", tc.data)
			}
			if session != tc.session || qIdx != tc.qIdx || total != tc.total || optIdx != tc.optIdx {
				t.Fatalf("%q = (%q, q=%d, total=%d, opt=%d), want (%q, q=%d, total=%d, opt=%d)",
					tc.data, session, qIdx, total, optIdx, tc.session, tc.qIdx, tc.total, tc.optIdx)
			}
		})
	}

	// Non-numeric trailing fields are not a choice callback at all.
	if _, _, _, _, ok := parseChoiceCallback(cfg, "dream:abc:def"); ok {
		t.Fatal("non-numeric callback data should not parse")
	}
}

// An answer from someone other than the admin has to carry their name, or a
// multi-human group cannot tell who answered. The admin stays untagged.
func TestCallbackTag(t *testing.T) {
	cfg := &Config{ChatID: 111}

	admin := &CallbackQuery{}
	admin.From.ID = 111
	admin.From.FirstName = "Vlad"
	admin.From.Username = "Wagok"
	if got := callbackTag(cfg, admin); got != "" {
		t.Fatalf("admin press = %q, want no tag", got)
	}

	other := &CallbackQuery{}
	other.From.ID = 222
	other.From.FirstName = "Serhii"
	other.From.Username = "serhii"
	if got, want := callbackTag(cfg, other), "[from Serhii (@serhii)] "; got != want {
		t.Fatalf("other press = %q, want %q", got, want)
	}

	// No username set — fall back to the display name alone.
	noUser := &CallbackQuery{}
	noUser.From.ID = 333
	noUser.From.FirstName = "Ira"
	if got, want := callbackTag(cfg, noUser), "[from Ira] "; got != want {
		t.Fatalf("no-username press = %q, want %q", got, want)
	}
}
