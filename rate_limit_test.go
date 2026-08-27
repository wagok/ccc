package main

import (
	"testing"
	"time"

	"github.com/kidandcat/ccc/internal/config"
)

// Every message below was taken verbatim from ~/.ccc/stopfailure.log, where the
// old blind retry fired ~12k times: 10296 weekly-quota + 2069 session-quota
// nudges that could never work, against ONE genuine transient throttle.
func TestClassifyStopFailure(t *testing.T) {
	now := time.Date(2026, 8, 27, 13, 0, 0, 0, time.UTC)

	tests := []struct {
		name  string
		err   string
		msg   string
		want  failClass
		reset bool // a reset time is expected
	}{
		{
			name: "session quota with clock time",
			err:  "rate_limit",
			msg:  "You've hit your session limit · resets 2:10pm (UTC)",
			want: classQuota, reset: true,
		},
		{
			name: "weekly quota with date",
			err:  "rate_limit",
			msg:  "You've hit your weekly limit · resets Aug 28, 8am (UTC)",
			want: classQuota, reset: true,
		},
		{
			name: "weekly quota, time only",
			err:  "rate_limit",
			msg:  "You've hit your weekly limit · resets 8am (UTC)",
			want: classQuota, reset: true,
		},
		{
			// The one case the retry was built for — and Anthropic marks it
			// explicitly so it cannot be confused with a spent allowance.
			name: "genuine transient throttle",
			err:  "rate_limit",
			msg:  "API Error: Server is temporarily limiting requests (not your usage limit) · Rate limited",
			want: classTransient,
		},
		{
			name: "529 overload",
			err:  "server_error",
			msg:  "API Error: 529 Overloaded. This is a server-side issue, usually temporary — try again in a moment.",
			want: classTransient,
		},
		{
			name: "expired login",
			err:  "authentication_failed",
			msg:  "Login expired · Please run /login",
			want: classHard,
		},
		{
			name: "context full",
			err:  "invalid_request",
			msg:  "Prompt is too long",
			want: classHard,
		},
		{
			name: "org disabled subscription",
			err:  "oauth_org_not_allowed",
			msg:  "Your organization has disabled Claude subscription access for Claude Code · Use an Anthropic API key instead",
			want: classHard,
		},
		{
			name: "usage policy refusal",
			err:  "invalid_request",
			msg:  "API Error: Claude Code is unable to respond to this request, which appears to violate our Usage Policy",
			want: classHard,
		},
		{
			name: "unparseable tool call is not ours to retry",
			err:  "unknown",
			msg:  "The model's tool call could not be parsed (retry also failed).",
			want: classIgnore,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyStopFailure(tc.err, tc.msg, now)
			if got.Class != tc.want {
				t.Fatalf("class = %q, want %q (reason %q)", got.Class, tc.want, got.Reason)
			}
			if tc.reset && got.ResetAt == 0 {
				t.Fatalf("expected a parsed reset time, got none")
			}
			if got.Class != classIgnore && got.Reason == "" {
				t.Fatal("actionable verdict must carry a reason for the human")
			}
		})
	}
}

func TestParseResetTime(t *testing.T) {
	now := time.Date(2026, 8, 27, 13, 0, 0, 0, time.UTC)

	tests := []struct {
		name string
		msg  string
		want time.Time
	}{
		{
			name: "afternoon clock time later today",
			msg:  "You've hit your session limit · resets 2:10pm (UTC)",
			want: time.Date(2026, 8, 27, 14, 10, 0, 0, time.UTC),
		},
		{
			// 8am already passed at 13:00, so it means tomorrow.
			name: "morning time already passed rolls to tomorrow",
			msg:  "You've hit your weekly limit · resets 8am (UTC)",
			want: time.Date(2026, 8, 28, 8, 0, 0, 0, time.UTC),
		},
		{
			name: "explicit date wins over roll-over",
			msg:  "You've hit your weekly limit · resets Aug 29, 8am (UTC)",
			want: time.Date(2026, 8, 29, 8, 0, 0, 0, time.UTC),
		},
		{
			name: "noon is 12:00 not 00:00",
			msg:  "resets 12pm (UTC)",
			want: time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC).Add(24 * time.Hour),
		},
		{
			name: "midnight is 00:00",
			msg:  "resets 12am (UTC)",
			want: time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseResetTime(tc.msg, now)
			if got != tc.want.Unix() {
				t.Fatalf("got %s, want %s",
					time.Unix(got, 0).UTC().Format(time.RFC3339), tc.want.Format(time.RFC3339))
			}
		})
	}

	if got := parseResetTime("You've hit your session limit", now); got != 0 {
		t.Fatalf("message without a reset time should yield 0, got %d", got)
	}
}

// The allowance belongs to the subscription, so agents must resolve to the same
// key exactly when they share one — that is what collapses a five-agent storm
// into a single block.
func TestAccountKeyForSession(t *testing.T) {
	cfg := &Config{
		Accounts: map[string]string{"mornhouse": "/home/wlad/.claude-mornhouse"},
		Groups:   map[string]*config.GroupInfo{"gtara": {Account: "mornhouse"}},
		Sessions: map[string]*config.SessionInfo{
			"QA":        {Group: "gtara"},
			"PM":        {Group: "gtara"},
			"solo":      {Group: "other"},
			"pinned":    {Group: "other", Account: "mornhouse"},
			"noSession": nil,
		},
	}

	if a, b := accountKeyForSession(cfg, "QA"), accountKeyForSession(cfg, "PM"); a != b || a != "mornhouse" {
		t.Fatalf("group-mates should share the key: QA=%q PM=%q", a, b)
	}
	if got := accountKeyForSession(cfg, "solo"); got != "default" {
		t.Fatalf("unpinned session = %q, want default", got)
	}
	if got := accountKeyForSession(cfg, "pinned"); got != "mornhouse" {
		t.Fatalf("session override = %q, want mornhouse", got)
	}
	if got := accountKeyForSession(cfg, "ghost"); got != "default" {
		t.Fatalf("unknown session = %q, want default", got)
	}
}

func TestQuotaBlockActive(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	now := time.Now().Unix()

	writeQuotaBlock(quotaBlock{Account: "acct", ResetAt: now + 600, Since: now, Reason: "session limit reached"})
	if _, ok := quotaBlockActive("acct", now); !ok {
		t.Fatal("block with a future reset should be active")
	}
	if _, ok := quotaBlockActive("acct", now+601); ok {
		t.Fatal("block must expire once the reset moment passes")
	}
	if _, ok := quotaBlockActive("other", now); ok {
		t.Fatal("a block must not leak across accounts")
	}

	clearQuotaBlock("acct")
	if _, ok := quotaBlockActive("acct", now); ok {
		t.Fatal("cleared block should be gone")
	}
}
