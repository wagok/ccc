package main

// rate_limit.go reacts to a turn that died on an API error (the StopFailure
// hook). Not every such death is the same, and the old code treated them alike:
// anything whose payload merely contained "rate" or "limit" got a "please
// continue" nudge every 30-90s, forever. For an exhausted token quota that can
// never work — the log showed ~12k such nudges against ONE genuine transient
// throttle — and each nudge posted to Telegram and re-marked the agent as
// working, which broke idle detection.
//
// So classify first, then react:
//
//	transient  server-side throttle / 529 overload   -> retry, backing off
//	quota      session or weekly allowance spent     -> ONE wake at the reset time
//	hard       auth expired, policy, prompt too long -> no retry, tell the human
//
// Anthropic distinguishes the first two itself: the transient message says
// "(not your usage limit)", the quota one says "You've hit your session limit ·
// resets 2:10pm (UTC)" — the reset time is right there, so wait for it instead
// of guessing.
//
// A quota belongs to the ACCOUNT, not the agent: every agent on the same
// subscription runs out together. The block is therefore stored per account and
// one timer releases all of its agents, instead of each agent retrying alone.

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kidandcat/ccc/internal/scheduler"
)

// rlContinueText is injected to resume an agent after a recoverable failure —
// the exact phrasing that reliably restarts a stalled turn (verified live).
const rlContinueText = "An API rate limit error occurred; please continue"

// Backoff for transient failures, in seconds; the last entry repeats. The old
// flat 30-90s meant a throttle that lasted minutes was hammered dozens of times.
var rlBackoffSecs = []int64{45, 120, 300, 900, 1800}

// rlMaxAttempts caps a transient retry chain (~1h with the schedule above).
// Past that it is not transient, so stop and tell the human.
const rlMaxAttempts = 8

// rlResetBufferSecs is added to a parsed reset time. Reset boundaries are
// reported to the minute, so waking exactly on it can land just before.
const rlResetBufferSecs = 60

// rlQuotaFallbackSecs is how long to park an agent when the message says the
// quota is gone but carries no parseable reset time.
const rlQuotaFallbackSecs = 30 * 60

// rlStaleResetSecs is used when the message names a reset moment that has
// already passed: the allowance is probably back, so probe soon.
const rlStaleResetSecs = 2 * 60

// ---- classification ----

type failClass string

const (
	classTransient failClass = "transient"
	classQuota     failClass = "quota"
	classHard      failClass = "hard"
	classIgnore    failClass = "ignore"
)

// stopFailure is the verdict on one dead turn.
type stopFailure struct {
	Class   failClass
	ResetAt int64  // quota only; 0 when no reset time could be read
	Reason  string // short, human-readable — goes to Telegram on quota/hard
}

var (
	// "You've hit your weekly limit · resets Aug 25, 8am (UTC)"
	// "You've hit your session limit · resets 2:10pm (UTC)"
	reQuotaHit = regexp.MustCompile(`(?i)hit your (session|weekly|usage) limit`)
	reReset    = regexp.MustCompile(`(?i)resets\s+(?:([A-Za-z]{3})[a-z]*\.?\s+(\d{1,2}),?\s+)?(\d{1,2})(?::(\d{2}))?\s*(am|pm)?\s*(?:\(([^)]+)\))?`)
)

// classifyStopFailure decides what a dead turn means. errField is the hook's
// structured "error" value; msg is last_assistant_message, which carries the
// text Anthropic showed the user.
func classifyStopFailure(errField, msg string, now time.Time) stopFailure {
	e := strings.ToLower(strings.TrimSpace(errField))
	m := strings.TrimSpace(msg)
	low := strings.ToLower(m)

	// Hard stops first: retrying these is pointless and hides the real problem.
	switch {
	case strings.Contains(e, "authentication") || strings.Contains(low, "login expired"):
		return stopFailure{Class: classHard, Reason: "Login expired — run /login for this agent's account"}
	case strings.Contains(e, "oauth_org_not_allowed") || strings.Contains(low, "organization has disabled"):
		return stopFailure{Class: classHard, Reason: "The organization disabled Claude Code subscription access"}
	case strings.Contains(low, "prompt is too long"):
		return stopFailure{Class: classHard, Reason: "Context is full — the agent needs /compact or a restart"}
	case strings.Contains(low, "usage policy"):
		return stopFailure{Class: classHard, Reason: "Request refused as a Usage Policy violation"}
	}

	// Transient: Anthropic spells this out so it is not mistaken for a quota.
	if strings.Contains(low, "not your usage limit") ||
		strings.Contains(low, "temporarily limiting") ||
		strings.Contains(low, "overloaded") || strings.Contains(low, "529") ||
		strings.Contains(e, "server_error") || strings.Contains(e, "overload") {
		return stopFailure{Class: classTransient, Reason: "Temporary server-side throttle"}
	}

	// Quota spent — the interesting case, and the one that must not be retried.
	if reQuotaHit.MatchString(low) {
		kind := "usage"
		if mm := reQuotaHit.FindStringSubmatch(low); len(mm) > 1 {
			kind = strings.ToLower(mm[1])
		}
		reset := parseResetTime(m, now)
		reason := fmt.Sprintf("%s limit reached", kind)
		if reset > 0 {
			reason += fmt.Sprintf(", resets %s UTC", time.Unix(reset, 0).UTC().Format("15:04"))
		}
		return stopFailure{Class: classQuota, ResetAt: reset, Reason: reason}
	}

	// A rate_limit error whose text we do not recognise: treat as transient, but
	// the backoff and attempt cap keep an unknown case from looping forever.
	if strings.Contains(e, "rate_limit") || strings.Contains(e, "rate limit") {
		return stopFailure{Class: classTransient, Reason: "Rate limited"}
	}
	return stopFailure{Class: classIgnore}
}

// parseResetTime reads the reset moment out of a quota message. Seen forms:
//
//	resets 8am (UTC)
//	resets 2:10pm (UTC)
//	resets Aug 25, 8am (UTC)
//
// Returns 0 when nothing parseable is present. A bare clock time that already
// passed today means tomorrow. Only UTC is honoured explicitly; any other zone
// name is treated as UTC, which is what Anthropic has been sending.
func parseResetTime(msg string, now time.Time) int64 {
	mm := reReset.FindStringSubmatch(msg)
	if mm == nil {
		return 0
	}
	monName, dayStr, hourStr, minStr, ampm := mm[1], mm[2], mm[3], mm[4], strings.ToLower(mm[5])

	hour, err := strconv.Atoi(hourStr)
	if err != nil || hour < 0 || hour > 23 {
		return 0
	}
	min := 0
	if minStr != "" {
		if min, err = strconv.Atoi(minStr); err != nil || min > 59 {
			return 0
		}
	}
	switch ampm {
	case "am":
		if hour == 12 {
			hour = 0
		}
	case "pm":
		if hour != 12 {
			hour += 12
		}
	}
	if hour > 23 {
		return 0
	}

	utc := now.UTC()
	year, month, day := utc.Date()

	if monName != "" && dayStr != "" {
		m, ok := monthByName(monName)
		if !ok {
			return 0
		}
		d, err := strconv.Atoi(dayStr)
		if err != nil || d < 1 || d > 31 {
			return 0
		}
		month, day = m, d
		// A date far behind us means the message named next year's month.
		cand := time.Date(year, month, day, hour, min, 0, 0, time.UTC)
		if utc.Sub(cand) > 180*24*time.Hour {
			year++
		}
		return time.Date(year, month, day, hour, min, 0, 0, time.UTC).Unix()
	}

	cand := time.Date(year, month, day, hour, min, 0, 0, time.UTC)
	if !cand.After(utc) {
		cand = cand.Add(24 * time.Hour) // clock time already passed -> tomorrow
	}
	return cand.Unix()
}

func monthByName(s string) (time.Month, bool) {
	switch strings.ToLower(s)[:3] {
	case "jan":
		return time.January, true
	case "feb":
		return time.February, true
	case "mar":
		return time.March, true
	case "apr":
		return time.April, true
	case "may":
		return time.May, true
	case "jun":
		return time.June, true
	case "jul":
		return time.July, true
	case "aug":
		return time.August, true
	case "sep":
		return time.September, true
	case "oct":
		return time.October, true
	case "nov":
		return time.November, true
	case "dec":
		return time.December, true
	}
	return 0, false
}

// ---- per-account quota block ----

// quotaBlock records that an account's allowance is spent until ResetAt. It is
// keyed by account because the allowance is shared: one block parks every agent
// on that subscription and one timer releases them all.
type quotaBlock struct {
	Account string   `json:"account"`
	ResetAt int64    `json:"reset_at"`
	Since   int64    `json:"since"`
	Reason  string   `json:"reason"`
	Agents  []string `json:"agents"`
}

func rateLimitDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".ccc", "rate-limit")
}

func quotaBlockPath(account string) string {
	dir := rateLimitDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, url.PathEscape(account)+".json")
}

// accountKeyForSession names the subscription an agent runs on. Agents sharing
// a key share an allowance. "default" covers everything not pinned to an alias.
func accountKeyForSession(cfg *Config, session string) string {
	if cfg == nil {
		return "default"
	}
	si := cfg.Sessions[session]
	if si == nil {
		return "default"
	}
	if si.Account != "" {
		return si.Account
	}
	if g := cfg.Groups[sessionGroup(si)]; g != nil && g.Account != "" {
		return g.Account
	}
	return "default"
}

func readQuotaBlock(account string) (quotaBlock, bool) {
	p := quotaBlockPath(account)
	if p == "" {
		return quotaBlock{}, false
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return quotaBlock{}, false
	}
	var b quotaBlock
	if json.Unmarshal(data, &b) != nil {
		return quotaBlock{}, false
	}
	return b, true
}

func writeQuotaBlock(b quotaBlock) {
	if dir := rateLimitDir(); dir != "" {
		os.MkdirAll(dir, 0755)
	}
	if p := quotaBlockPath(b.Account); p != "" {
		_ = writeJSONFileAtomic(p, b)
	}
}

func clearQuotaBlock(account string) {
	if p := quotaBlockPath(account); p != "" {
		os.Remove(p)
	}
}

// quotaBlockActive reports whether an account is currently out of allowance.
func quotaBlockActive(account string, now int64) (quotaBlock, bool) {
	b, ok := readQuotaBlock(account)
	if !ok || b.ResetAt <= now {
		return quotaBlock{}, false
	}
	return b, true
}

func quotaTimerID(account string) string { return "rlq:" + account }

// ---- reaction ----

// handleRateLimitRecover is the socket entry point for the StopFailure hook.
// req.Payload carries the raw hook JSON; an older client that sends only Cwd
// still gets the safe transient path.
func handleRateLimitRecover(encoder *json.Encoder, cfg *Config, req APIRequest) {
	agent := req.Session
	if agent == "" {
		agent = resolveSessionByHostCwd(cfg, req.Host, req.Cwd)
	}
	if agent == "" {
		encoder.Encode(APIResponse{OK: false, Error: "rl.recover: no session for caller"})
		return
	}
	if mailScheduler == nil {
		encoder.Encode(APIResponse{OK: false, Error: "rl.recover: scheduler not running"})
		return
	}

	var hd struct {
		Error                string `json:"error"`
		LastAssistantMessage string `json:"last_assistant_message"`
	}
	if len(req.Payload) > 0 {
		json.Unmarshal(req.Payload, &hd)
	}
	verdict := classifyStopFailure(hd.Error, hd.LastAssistantMessage, time.Now())

	switch verdict.Class {
	case classQuota:
		encoder.Encode(APIResponse{OK: true, Response: applyQuotaBlock(cfg, agent, verdict)})
	case classHard:
		reportHardFailure(cfg, agent, verdict)
		writeAgentStatus(agent, "stopped", time.Now().Unix())
		encoder.Encode(APIResponse{OK: true, Response: "hard failure: " + verdict.Reason})
	case classIgnore:
		encoder.Encode(APIResponse{OK: true, Response: "not actionable, ignored"})
	default:
		encoder.Encode(APIResponse{OK: true, Response: scheduleTransientRetry(cfg, agent)})
	}
}

// scheduleTransientRetry backs off further on each successive failure and gives
// up after rlMaxAttempts, so an error that only looks transient cannot loop.
func scheduleTransientRetry(cfg *Config, agent string) string {
	id := "rl:" + agent
	attempt := 1
	if prev, ok := mailScheduler.Get(id); ok && prev.Meta != nil {
		if n, err := strconv.Atoi(prev.Meta["attempt"]); err == nil {
			attempt = n + 1
		}
	}
	if attempt > rlMaxAttempts {
		mailScheduler.Cancel(id)
		notifyAgentTopic(cfg, agent, fmt.Sprintf(
			"⛔ %s: API errors persisted through %d retries — giving up. The agent is stopped.",
			agent, rlMaxAttempts))
		writeAgentStatus(agent, "stopped", time.Now().Unix())
		return "gave up after max attempts"
	}

	idx := attempt - 1
	if idx >= len(rlBackoffSecs) {
		idx = len(rlBackoffSecs) - 1
	}
	// Jitter keeps agents that failed together from retrying in lockstep.
	delay := rlBackoffSecs[idx] + int64(rand.Intn(30))
	mailScheduler.Schedule(scheduler.Timer{
		ID:     id,
		FireAt: time.Now().Unix() + delay,
		Kind:   "rl_continue",
		Agent:  agent,
		Meta:   map[string]string{"attempt": strconv.Itoa(attempt)},
	})
	return fmt.Sprintf("transient: retry %d/%d in %ds", attempt, rlMaxAttempts, delay)
}

// applyQuotaBlock parks the agent until the account's allowance returns. The
// first agent to hit the wall creates the block and announces it; the rest join
// it silently, which is what turns a five-agent storm into one notice.
func applyQuotaBlock(cfg *Config, agent string, v stopFailure) string {
	now := time.Now().Unix()
	account := accountKeyForSession(cfg, agent)

	reset := v.ResetAt
	switch {
	case reset == 0:
		reset = now + rlQuotaFallbackSecs // no reset time in the message
	case reset <= now:
		// The message names a moment that has already passed, so the allowance
		// should be back — probe soon rather than parking for half an hour.
		reset = now + rlStaleResetSecs
	}

	b, existing := readQuotaBlock(account)
	fresh := !existing || b.ResetAt <= now
	if fresh {
		b = quotaBlock{Account: account, ResetAt: reset, Since: now, Reason: v.Reason}
	} else if reset > b.ResetAt {
		b.ResetAt = reset // a later message knows better
	}
	if !containsString(b.Agents, agent) {
		b.Agents = append(b.Agents, agent)
	}
	writeQuotaBlock(b)

	// A pending transient retry must not fire into a wall.
	mailScheduler.Cancel("rl:" + agent)
	writeAgentStatus(agent, "rate_limited", now)

	mailScheduler.Schedule(scheduler.Timer{
		ID:     quotaTimerID(account),
		FireAt: b.ResetAt + rlResetBufferSecs,
		Kind:   "rl_quota",
		Agent:  account,
	})

	if fresh {
		when := time.Unix(b.ResetAt, 0).UTC().Format("15:04")
		notifyAgentTopic(cfg, agent, fmt.Sprintf(
			"⏳ Account %q: %s. Agents on it are paused and resume at %s UTC.",
			account, v.Reason, when))
	}
	return fmt.Sprintf("quota block on %q until %d (agents: %d)", account, b.ResetAt, len(b.Agents))
}

// onQuotaReset releases every agent parked by an account's block.
func onQuotaReset(t scheduler.Timer) {
	account := t.Agent // this Kind stores the account key here
	cfg, err := loadConfig()
	if err != nil {
		return
	}
	b, ok := readQuotaBlock(account)
	if !ok {
		return
	}
	now := time.Now().Unix()
	if b.ResetAt > now {
		// Extended while we waited — re-arm instead of waking into the wall.
		if mailScheduler != nil {
			mailScheduler.Schedule(scheduler.Timer{
				ID: quotaTimerID(account), FireAt: b.ResetAt + rlResetBufferSecs,
				Kind: "rl_quota", Agent: account,
			})
		}
		return
	}
	clearQuotaBlock(account)

	if len(b.Agents) > 0 {
		notifyAgentTopic(cfg, b.Agents[0], fmt.Sprintf(
			"✅ Account %q: limit reset — resuming %d agent(s).", account, len(b.Agents)))
	}
	for i, agent := range b.Agents {
		// Stagger: waking five agents at once just re-exhausts the allowance.
		if mailScheduler != nil && i > 0 {
			mailScheduler.Schedule(scheduler.Timer{
				ID: "rl:" + agent, FireAt: now + int64(20*i),
				Kind: "rl_continue", Agent: agent,
			})
			continue
		}
		wakeAgent(cfg, agent, rlContinueText)
	}
}

// reportHardFailure tells the human, because nothing automatic can fix these.
func reportHardFailure(cfg *Config, agent string, v stopFailure) {
	notifyAgentTopic(cfg, agent, fmt.Sprintf("⛔ %s stopped: %s", agent, v.Reason))
}

// notifyAgentTopic posts one line into an agent's Telegram topic without
// injecting anything into the agent itself.
func notifyAgentTopic(cfg *Config, agent, text string) {
	if cfg == nil {
		return
	}
	info := cfg.Sessions[agent]
	if info == nil {
		return
	}
	sendMessage(cfg, sessionGroupChatID(cfg, agent), info.TopicID, text)
	appendHistory(info.TopicID, HistoryMessage{
		ID: nextMessageID(), Timestamp: time.Now().Unix(), From: "claude", Text: text,
	})
}

func containsString(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// onRateLimitContinue fires a scheduled retry, unless the account ran out of
// allowance in the meantime — then the block owns the agent and this is a no-op.
func onRateLimitContinue(t scheduler.Timer) {
	cfg, err := loadConfig()
	if err != nil {
		return
	}
	if _, blocked := quotaBlockActive(accountKeyForSession(cfg, t.Agent), time.Now().Unix()); blocked {
		return
	}
	wakeAgent(cfg, t.Agent, rlContinueText)
}
