package main

// reminders.go implements per-agent reminders on top of the shared scheduler
// (Kind="reminder"). An agent sets a reminder via the secretary MCP; when it is
// due, CCC injects the agent's own text into the agent's prompt — ALGORITHMICALLY,
// with no secretary-agent involvement. Reminders are one-time (after N minutes /
// at a datetime) or recurring (every N minutes / daily at HH:MM in a timezone).
//
// Ownership: an agent sees/sets/deletes only its OWN reminders; a secretary may
// manage reminders for any agent in its group.

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/kidandcat/ccc/internal/mail"
	"github.com/kidandcat/ccc/internal/scheduler"
)

// reminderSchedule is the schedule spec an agent passes to reminder_add. Exactly
// one of the four shapes is used. The agent supplies the timezone (per Vlad):
// daily_at requires tz (IANA name); at carries its own offset (ISO8601).
type reminderSchedule struct {
	InMinutes    int    `json:"in_minutes,omitempty"`    // once, after N minutes
	At           string `json:"at,omitempty"`            // once, at ISO8601 w/ offset
	EveryMinutes int    `json:"every_minutes,omitempty"` // recurring, every N minutes
	DailyAt      string `json:"daily_at,omitempty"`      // recurring, daily at "HH:MM"
	TZ           string `json:"tz,omitempty"`            // IANA tz for daily_at
}

type reminderAddArgs struct {
	Text     string           `json:"text"`
	Schedule reminderSchedule `json:"schedule"`
	Agent    string           `json:"agent,omitempty"` // secretary-only: target another agent
}

// parseHHMM parses "HH:MM" (24h).
func parseHHMM(s string) (int, int, error) {
	var hh, mm int
	if _, err := fmt.Sscanf(s, "%d:%d", &hh, &mm); err != nil {
		return 0, 0, fmt.Errorf("bad time %q (want HH:MM)", s)
	}
	if hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return 0, 0, fmt.Errorf("time %q out of range", s)
	}
	return hh, mm, nil
}

// nextDaily returns the next unix time at hh:mm in loc strictly after now.
func nextDaily(now time.Time, hh, mm int, loc *time.Location) int64 {
	n := now.In(loc)
	cand := time.Date(n.Year(), n.Month(), n.Day(), hh, mm, 0, 0, loc)
	if !cand.After(n) {
		cand = cand.AddDate(0, 0, 1)
	}
	return cand.Unix()
}

// parseReminderSchedule validates a schedule and returns the first fire time
// (unix secs) plus the recurrence metadata stored on the timer.
func parseReminderSchedule(s reminderSchedule, now time.Time) (int64, map[string]string, error) {
	meta := map[string]string{}
	switch {
	case s.InMinutes > 0:
		meta["recur"] = "once"
		return now.Add(time.Duration(s.InMinutes) * time.Minute).Unix(), meta, nil
	case s.At != "":
		ts, err := time.Parse(time.RFC3339, s.At)
		if err != nil {
			return 0, nil, fmt.Errorf("bad 'at' (use ISO8601 with offset, e.g. 2026-06-25T18:00:00+03:00): %v", err)
		}
		if ts.Unix() <= now.Unix() {
			return 0, nil, fmt.Errorf("'at' time is in the past")
		}
		meta["recur"] = "once"
		return ts.Unix(), meta, nil
	case s.EveryMinutes > 0:
		meta["recur"] = "every"
		meta["interval_min"] = strconv.Itoa(s.EveryMinutes)
		return now.Add(time.Duration(s.EveryMinutes) * time.Minute).Unix(), meta, nil
	case s.DailyAt != "":
		if s.TZ == "" {
			return 0, nil, fmt.Errorf("'daily_at' requires 'tz' (e.g. Europe/Kyiv)")
		}
		loc, err := time.LoadLocation(s.TZ)
		if err != nil {
			return 0, nil, fmt.Errorf("bad tz %q: %v", s.TZ, err)
		}
		hh, mm, err := parseHHMM(s.DailyAt)
		if err != nil {
			return 0, nil, err
		}
		meta["recur"] = "daily"
		meta["hhmm"] = s.DailyAt
		meta["tz"] = s.TZ
		return nextDaily(now, hh, mm, loc), meta, nil
	default:
		return 0, nil, fmt.Errorf("schedule must set one of: in_minutes, at, every_minutes, daily_at")
	}
}

// scheduleDesc renders a human-readable description of a reminder's recurrence.
func scheduleDesc(meta map[string]string) string {
	switch meta["recur"] {
	case "once":
		return "once"
	case "every":
		return "every " + meta["interval_min"] + " min"
	case "daily":
		return "daily at " + meta["hhmm"] + " " + meta["tz"]
	}
	return meta["recur"]
}

// onReminderTimer fires a due reminder: it injects the agent's text into the
// agent, then reschedules itself if recurring (same ID, next fire time — the
// engine preserves a rescheduled timer instead of deleting it).
func onReminderTimer(t scheduler.Timer) {
	cfg, err := loadConfig()
	if err != nil {
		return
	}
	wakeAgent(cfg, t.Agent, "⏰ Reminder: "+t.Prompt)

	now := time.Now().Unix()
	switch t.Meta["recur"] {
	case "every":
		iv, _ := strconv.Atoi(t.Meta["interval_min"])
		if iv <= 0 || mailScheduler == nil {
			return
		}
		next := t.FireAt + int64(iv)*60
		for next <= now { // skip any slots missed while CCC was down
			next += int64(iv) * 60
		}
		t.FireAt = next
		mailScheduler.Schedule(t)
	case "daily":
		if mailScheduler == nil {
			return
		}
		loc, err := time.LoadLocation(t.Meta["tz"])
		if err != nil {
			return
		}
		hh, mm, err := parseHHMM(t.Meta["hhmm"])
		if err != nil {
			return
		}
		t.FireAt = nextDaily(time.Now(), hh, mm, loc)
		mailScheduler.Schedule(t)
	}
	// "once" -> no reschedule; the engine removes it.
}

// reminderView is the listing shape returned to agents.
type reminderView struct {
	ID       string `json:"id"`
	Agent    string `json:"agent"`
	Text     string `json:"text"`
	Schedule string `json:"schedule"`
	NextFire string `json:"next_fire"`
}

// handleReminderAddCmd creates a reminder. Owner = caller; a secretary may target
// another agent in its group via the "agent" arg.
func handleReminderAddCmd(encoder *json.Encoder, cfg *Config, req APIRequest) {
	caller := resolveCaller(cfg, req.Host, req.Cwd)
	if caller == "" {
		encoder.Encode(APIResponse{OK: false, Error: "reminder: caller cwd matches no agent"})
		return
	}
	var a reminderAddArgs
	if err := json.Unmarshal(req.Payload, &a); err != nil {
		encoder.Encode(APIResponse{OK: false, Error: "reminder: bad payload: " + err.Error()})
		return
	}
	if strings.TrimSpace(a.Text) == "" {
		encoder.Encode(APIResponse{OK: false, Error: "reminder: 'text' is required"})
		return
	}

	owner := caller
	if a.Agent != "" && a.Agent != caller {
		// Only a secretary may set a reminder for another agent, and only within
		// its own group.
		if !mail.IsSecretary(caller) {
			encoder.Encode(APIResponse{OK: false, Error: "reminder: only the secretary may set reminders for other agents"})
			return
		}
		if cfg.Sessions[a.Agent] == nil {
			encoder.Encode(APIResponse{OK: false, Error: "reminder: unknown agent " + a.Agent})
			return
		}
		if sessionGroup(cfg.Sessions[a.Agent]) != sessionGroup(cfg.Sessions[caller]) {
			encoder.Encode(APIResponse{OK: false, Error: "reminder: " + a.Agent + " is in another group"})
			return
		}
		owner = a.Agent
	}

	fireAt, meta, err := parseReminderSchedule(a.Schedule, time.Now())
	if err != nil {
		encoder.Encode(APIResponse{OK: false, Error: "reminder: " + err.Error()})
		return
	}
	if mailScheduler == nil {
		encoder.Encode(APIResponse{OK: false, Error: "reminder: scheduler not running"})
		return
	}
	id := fmt.Sprintf("rem-%x", time.Now().UnixNano())
	if _, err := mailScheduler.Schedule(scheduler.Timer{
		ID: id, FireAt: fireAt, Kind: "reminder", Agent: owner, Prompt: a.Text, Meta: meta,
	}); err != nil {
		encoder.Encode(APIResponse{OK: false, Error: "reminder: " + err.Error()})
		return
	}
	out, _ := json.Marshal(reminderView{
		ID: id, Agent: owner, Text: a.Text, Schedule: scheduleDesc(meta),
		NextFire: time.Unix(fireAt, 0).Format(time.RFC3339),
	})
	encoder.Encode(APIResponse{OK: true, Result: out})
}

// handleReminderListCmd lists reminders. An ordinary agent sees only its own; a
// secretary sees all reminders for agents in its group (optionally filtered by
// the "agent" arg).
func handleReminderListCmd(encoder *json.Encoder, cfg *Config, req APIRequest) {
	caller := resolveCaller(cfg, req.Host, req.Cwd)
	if caller == "" {
		encoder.Encode(APIResponse{OK: false, Error: "reminder: caller cwd matches no agent"})
		return
	}
	var a struct {
		Agent string `json:"agent,omitempty"`
	}
	json.Unmarshal(req.Payload, &a)

	isSec := mail.IsSecretary(caller)
	secGroup := sessionGroup(cfg.Sessions[caller])

	var out []reminderView
	if mailScheduler != nil {
		for _, t := range mailScheduler.List() {
			if t.Kind != "reminder" {
				continue
			}
			if isSec {
				if sessionGroup(cfg.Sessions[t.Agent]) != secGroup {
					continue
				}
				if a.Agent != "" && t.Agent != a.Agent {
					continue
				}
			} else if t.Agent != caller {
				continue
			}
			out = append(out, reminderView{
				ID: t.ID, Agent: t.Agent, Text: t.Prompt, Schedule: scheduleDesc(t.Meta),
				NextFire: time.Unix(t.FireAt, 0).Format(time.RFC3339),
			})
		}
	}
	data, _ := json.Marshal(out)
	encoder.Encode(APIResponse{OK: true, Result: data})
}

// handleReminderDeleteCmd deletes a reminder by id, enforcing ownership (own
// reminder, or any in-group reminder for a secretary).
func handleReminderDeleteCmd(encoder *json.Encoder, cfg *Config, req APIRequest) {
	caller := resolveCaller(cfg, req.Host, req.Cwd)
	if caller == "" {
		encoder.Encode(APIResponse{OK: false, Error: "reminder: caller cwd matches no agent"})
		return
	}
	var a struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(req.Payload, &a); err != nil || a.ID == "" {
		encoder.Encode(APIResponse{OK: false, Error: "reminder: 'id' is required"})
		return
	}
	if mailScheduler == nil {
		encoder.Encode(APIResponse{OK: false, Error: "reminder: scheduler not running"})
		return
	}
	t, ok := mailScheduler.Get(a.ID)
	if !ok || t.Kind != "reminder" {
		encoder.Encode(APIResponse{OK: false, Error: "reminder: no such reminder " + a.ID})
		return
	}
	allowed := t.Agent == caller
	if !allowed && mail.IsSecretary(caller) {
		allowed = sessionGroup(cfg.Sessions[t.Agent]) == sessionGroup(cfg.Sessions[caller])
	}
	if !allowed {
		encoder.Encode(APIResponse{OK: false, Error: "reminder: not yours to delete"})
		return
	}
	mailScheduler.Cancel(a.ID)
	encoder.Encode(APIResponse{OK: true})
}
