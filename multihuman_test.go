package main

import "testing"

func TestHumanTag(t *testing.T) {
	cfg := &Config{ChatID: 100}
	cases := []struct {
		fromID            int64
		first, user, want string
	}{
		{100, "Vlad", "vlad", ""},                  // primary admin -> untagged
		{200, "Alice", "alice", "[from Alice (@alice)] "},
		{200, "Bob", "", "[from Bob] "},            // no username
		{200, "", "carol", "[from carol] "},        // no first name -> username, no dup
		{200, "", "", "[from user200] "},           // nothing -> id fallback
	}
	for _, c := range cases {
		if got := humanTag(cfg, c.fromID, c.first, c.user); got != c.want {
			t.Errorf("humanTag(%d,%q,%q) = %q, want %q", c.fromID, c.first, c.user, got, c.want)
		}
	}
}
