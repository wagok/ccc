package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/kidandcat/ccc/internal/scheduler"
)

// Agent work-state tracking + idle subscriptions.
//
// State is derived from Claude Code's turn-boundary hooks, NOT from scraping the
// tmux pane (which is unreliable now that the input prompt is shown even while the
// agent works). Two edges drive it, both converging on handleTypingSocketCmd:
//   - UserPromptSubmit hook -> "start" -> status=working
//   - Stop hook             -> "stop"  -> status=idle
//
// A subscription fires not on the raw "stop" but on QUIESCENCE: the agent must
// stay idle for idleQuietSecs with no new "start". Each stop (re)arms an
// "idle_notify" scheduler timer; each start cancels it. Only when the timer
// survives the full quiet period is the agent considered truly free. This makes a
// subscription mean "agent finished and is ready for new work", not "one turn
// ended" (which recurs mid-task on tool loops / clarifying questions).

const idleQuietSecs = 180 // 3 minutes of no new work = agent is free

var agentStateMu sync.Mutex

func agentStatusDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".ccc", "agent-status")
}

func idleSubDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".ccc", "idle-subs")
}

// agentStatus is the persisted work-state of one agent.
type agentStatus struct {
	Agent   string `json:"agent"`
	Status  string `json:"status"` // "working" | "idle"
	Since   int64  `json:"since"`  // unix secs the current status began
	Updated int64  `json:"updated"`
}

// idleSub is a standing request to notify Subscriber when Target becomes free.
type idleSub struct {
	ID         string `json:"id"`
	Subscriber string `json:"subscriber"` // agent to notify
	Target     string `json:"target"`     // agent whose idle we wait for
	Note       string `json:"note,omitempty"`
	OneShot    bool   `json:"one_shot"` // true: removed after firing once
	Created    int64  `json:"created"`
}

func writeJSONFileAtomic(path string, v interface{}) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func agentStatusPath(agent string) string {
	return filepath.Join(agentStatusDir(), url.QueryEscape(agent)+".json")
}

func writeAgentStatus(agent, status string, now int64) {
	agentStateMu.Lock()
	defer agentStateMu.Unlock()
	st := agentStatus{Agent: agent, Status: status, Since: now, Updated: now}
	// Preserve Since if the status is unchanged (keeps an accurate "idle since").
	if prev, ok := readAgentStatusLocked(agent); ok && prev.Status == status {
		st.Since = prev.Since
	}
	_ = writeJSONFileAtomic(agentStatusPath(agent), st)
}

func readAgentStatus(agent string) (agentStatus, bool) {
	agentStateMu.Lock()
	defer agentStateMu.Unlock()
	return readAgentStatusLocked(agent)
}

func readAgentStatusLocked(agent string) (agentStatus, bool) {
	data, err := os.ReadFile(agentStatusPath(agent))
	if err != nil {
		return agentStatus{}, false
	}
	var st agentStatus
	if json.Unmarshal(data, &st) != nil {
		return agentStatus{}, false
	}
	return st, true
}

// recordAgentWorkEdge updates persisted state on a turn-boundary hook edge and
// arms/cancels the idle-notify timer. Called from handleTypingSocketCmd for both
// local and (relayed) remote agents. edge is "start" or "stop"; anything else is
// ignored (e.g. a malformed typing payload).
func recordAgentWorkEdge(cfg *Config, session, edge string) {
	now := time.Now().Unix()
	switch edge {
	case "start":
		writeAgentStatus(session, "working", now)
		if mailScheduler != nil {
			mailScheduler.Cancel(idleTimerID(session)) // agent resumed -> not idle
		}
	case "stop":
		// A turn that died on an exhausted quota also ends with a stop edge.
		// Reporting that as "idle" would be a lie: the agent is not available,
		// it is parked until the allowance returns — and notify_when_free would
		// fire on an agent that cannot accept work.
		if _, blocked := quotaBlockActive(accountKeyForSession(cfg, session), now); blocked {
			writeAgentStatus(session, "rate_limited", now)
			return
		}
		writeAgentStatus(session, "idle", now)
		// Arm the quiescence timer only if someone is actually waiting.
		if mailScheduler != nil && idleHasSubscribers(session) {
			mailScheduler.Schedule(scheduler.Timer{
				ID:     idleTimerID(session),
				FireAt: now + idleQuietSecs,
				Kind:   "idle_notify",
				Agent:  session,
			})
		}
	}
}

func idleTimerID(agent string) string { return "idle:" + agent }

// ---- subscription storage ----

func idleSubPath(id string) string {
	return filepath.Join(idleSubDir(), url.QueryEscape(id)+".json")
}

func writeIdleSub(s idleSub) error { return writeJSONFileAtomic(idleSubPath(s.ID), s) }

func deleteIdleSub(id string) { _ = os.Remove(idleSubPath(id)) }

func listIdleSubs() []idleSub {
	entries, err := os.ReadDir(idleSubDir())
	if err != nil {
		return nil
	}
	var out []idleSub
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(idleSubDir(), e.Name()))
		if err != nil {
			continue
		}
		var s idleSub
		if json.Unmarshal(data, &s) == nil && s.ID != "" {
			out = append(out, s)
		}
	}
	return out
}

func idleSubsForTarget(target string) []idleSub {
	var out []idleSub
	for _, s := range listIdleSubs() {
		if s.Target == target {
			out = append(out, s)
		}
	}
	return out
}

func idleHasSubscribers(target string) bool {
	return len(idleSubsForTarget(target)) > 0
}

// onIdleNotifyTimer fires when an agent has stayed idle for the full quiet period.
// It notifies every subscriber of that agent and removes the one-shot ones.
func onIdleNotifyTimer(t scheduler.Timer) {
	cfg, err := loadConfig()
	if err != nil {
		return
	}
	target := t.Agent
	// Safety net: a "start" edge normally cancels this timer, but a start signal
	// could be lost across a server restart. Re-check the persisted state and bail
	// if the agent resumed work (a later stop will re-arm).
	if st, ok := readAgentStatus(target); ok && st.Status != "idle" {
		return
	}
	subs := idleSubsForTarget(target)
	for _, s := range subs {
		msg := fmt.Sprintf("🔔 Agent %s is now free — idle for %d min, ready for a new task or a result check.", target, idleQuietSecs/60)
		if s.Note != "" {
			msg += "\nYour note: " + s.Note
		}
		if err := wakeAgent(cfg, s.Subscriber, msg); err != nil {
			fmt.Fprintf(os.Stderr, "idle_notify: wake %s: %v\n", s.Subscriber, err)
		}
		if s.OneShot {
			deleteIdleSub(s.ID)
		}
	}
}

// ---- socket handlers ----

// handleAgentStatusCmd returns an agent's persisted work-state. Group-isolated.
func handleAgentStatusCmd(encoder *json.Encoder, cfg *Config, req APIRequest) {
	var p struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(req.Payload, &p); err != nil || p.Name == "" {
		encoder.Encode(APIResponse{OK: false, Error: "get_agent_status: missing 'name'"})
		return
	}
	callerGroup := sessionGroup(cfg.Sessions[resolveCaller(cfg, req.Host, req.Cwd)])
	if sessionGroup(cfg.Sessions[p.Name]) != callerGroup {
		encoder.Encode(APIResponse{OK: false, Error: "get_agent_status: " + p.Name + " is not in your group"})
		return
	}
	out := map[string]interface{}{"agent": p.Name}
	if st, ok := readAgentStatus(p.Name); ok {
		now := time.Now().Unix()
		secs := now - st.Since
		out["status"] = st.Status
		out["seconds_in_state"] = secs
		out["free"] = st.Status == "idle" && secs >= idleQuietSecs
		// Say WHEN a parked agent comes back, so a caller can plan instead of
		// polling a peer that is guaranteed not to answer.
		if b, blocked := quotaBlockActive(accountKeyForSession(cfg, p.Name), now); blocked {
			out["status"] = "rate_limited"
			out["free"] = false
			out["blocked_until"] = b.ResetAt
			out["note"] = "account allowance exhausted (" + b.Reason + "); resumes automatically"
		}
	} else {
		out["status"] = "unknown"
		out["note"] = "no work-edge recorded yet — the agent has not started or stopped a turn since status tracking began"
	}
	data, _ := json.Marshal(out)
	encoder.Encode(APIResponse{OK: true, Result: data})
}

// handleSubscribeIdleCmd registers a "notify me when Target is free" request.
// Group-isolated. If Target is already free, notifies the caller immediately.
func handleSubscribeIdleCmd(encoder *json.Encoder, cfg *Config, req APIRequest) {
	subscriber := resolveCaller(cfg, req.Host, req.Cwd)
	if subscriber == "" {
		encoder.Encode(APIResponse{OK: false, Error: "notify_when_free: caller cwd matches no agent"})
		return
	}
	var p struct {
		Name       string `json:"name"`
		Note       string `json:"note"`
		Persistent bool   `json:"persistent"`
	}
	if err := json.Unmarshal(req.Payload, &p); err != nil || p.Name == "" {
		encoder.Encode(APIResponse{OK: false, Error: "notify_when_free: missing 'name'"})
		return
	}
	if p.Name == subscriber {
		encoder.Encode(APIResponse{OK: false, Error: "notify_when_free: cannot subscribe to yourself"})
		return
	}
	callerGroup := sessionGroup(cfg.Sessions[subscriber])
	if sessionGroup(cfg.Sessions[p.Name]) != callerGroup {
		encoder.Encode(APIResponse{OK: false, Error: "notify_when_free: " + p.Name + " is not in your group"})
		return
	}

	now := time.Now().Unix()
	sub := idleSub{
		ID:         fmt.Sprintf("%s-%s-%d", subscriber, p.Name, time.Now().UnixNano()),
		Subscriber: subscriber,
		Target:     p.Name,
		Note:       p.Note,
		OneShot:    !p.Persistent,
		Created:    now,
	}

	// If the target is already free, notify now and don't bother storing a one-shot.
	if st, ok := readAgentStatus(p.Name); ok && st.Status == "idle" && now-st.Since >= idleQuietSecs {
		msg := fmt.Sprintf("🔔 Agent %s is already free (idle %d min).", p.Name, (now-st.Since)/60)
		if p.Note != "" {
			msg += "\nYour note: " + p.Note
		}
		if err := wakeAgent(cfg, subscriber, msg); err != nil {
			encoder.Encode(APIResponse{OK: false, Error: "notify_when_free: notify failed: " + err.Error()})
			return
		}
		if sub.Persistent() {
			_ = writeIdleSub(sub)
		}
		encoder.Encode(APIResponse{OK: true, Result: mustJSON(map[string]interface{}{
			"subscribed": true, "target": p.Name, "delivered": "immediately — target already free", "id": sub.ID,
		})})
		return
	}

	if err := writeIdleSub(sub); err != nil {
		encoder.Encode(APIResponse{OK: false, Error: "notify_when_free: " + err.Error()})
		return
	}
	// If the target is idle but not yet for the full quiet period, arm the timer
	// for the remaining time. Otherwise the next stop edge arms it.
	if st, ok := readAgentStatus(p.Name); ok && st.Status == "idle" && mailScheduler != nil {
		mailScheduler.Schedule(scheduler.Timer{
			ID:     idleTimerID(p.Name),
			FireAt: st.Since + idleQuietSecs,
			Kind:   "idle_notify",
			Agent:  p.Name,
		})
	}
	encoder.Encode(APIResponse{OK: true, Result: mustJSON(map[string]interface{}{
		"subscribed": true, "target": p.Name, "id": sub.ID,
		"fires": fmt.Sprintf("when %s stays idle for %d min", p.Name, idleQuietSecs/60),
	})})
}

func (s idleSub) Persistent() bool { return !s.OneShot }

func mustJSON(v interface{}) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
