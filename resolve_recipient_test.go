package main

import "testing"

func TestResolveRecipient(t *testing.T) {
	cfg := &Config{Sessions: map[string]*SessionInfo{
		"msi:openarx-promo":    {Group: "openarx_ai"},
		"msi:openarx-research": {Group: "openarx_ai"},
		"backend":              {Group: ""}, // default group
		"old":                  {Group: "openarx_ai", Deleted: true},
	}}
	cases := []struct{ name, group, want string }{
		{"openarx-promo", "openarx_ai", "msi:openarx-promo"},     // short -> full
		{"msi:openarx-promo", "openarx_ai", "msi:openarx-promo"}, // already full
		{"backend", "default", "backend"},                       // exact in default
		{"ghost", "openarx_ai", "ghost"},                        // unknown passes through
		{"openarx-promo", "default", "openarx-promo"},           // wrong group -> no match
		{"", "openarx_ai", ""},                                  // empty
	}
	for _, c := range cases {
		if got := resolveRecipient(cfg, c.name, c.group); got != c.want {
			t.Errorf("resolveRecipient(%q,%q)=%q want %q", c.name, c.group, got, c.want)
		}
	}
}
