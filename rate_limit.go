package main

// rate_limit.go auto-recovers an agent whose turn died on a transient Anthropic
// API error ("Server is temporarily limiting requests · Rate limited"). Such an
// error ends the turn and leaves the agent idle — a headless agent then hangs.
//
// Detection is via the StopFailure hook (the turn ended due to an API error).
// On a matching failure CCC schedules, after a RANDOM 30–90s delay (so several
// agents that failed together don't retry in lockstep), a wake injecting
// "An API rate limit error occurred; please continue" — the exact phrasing that
// reliably resumes the agent (verified live). It is a natural backoff: if the
// limit still holds, the retry fails again and reschedules.

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"time"

	"github.com/kidandcat/ccc/internal/scheduler"
)

// rlContinueText is injected to resume an agent after a rate-limit failure.
const rlContinueText = "An API rate limit error occurred; please continue"

// handleRateLimitRecover schedules the delayed recovery for the agent identified
// by the caller's host+cwd. One pending recovery per agent (ID "rl:<agent>"), so
// repeated failures collapse into a single in-flight retry.
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
	delay := int64(30 + rand.Intn(61)) // 30..90s inclusive
	mailScheduler.Schedule(scheduler.Timer{
		ID:     "rl:" + agent,
		FireAt: time.Now().Unix() + delay,
		Kind:   "rl_continue",
		Agent:  agent,
	})
	encoder.Encode(APIResponse{OK: true, Response: fmt.Sprintf("recovery scheduled in %ds", delay)})
}

// onRateLimitContinue fires the delayed recovery: inject the continue prompt.
func onRateLimitContinue(t scheduler.Timer) {
	cfg, err := loadConfig()
	if err != nil {
		return
	}
	wakeAgent(cfg, t.Agent, rlContinueText)
}
