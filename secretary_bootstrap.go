package main

// secretary_bootstrap.go brings the smart secretary agent to life. The
// secretary's working directory is hardcoded (~/.ccc/secretary) and its template
// (instruction + .mcp.json) is shipped inside the binary via go:embed, so no
// repository checkout is needed at runtime. Bootstrap is idempotent: it installs
// missing template files (never clobbering edits), ensures the dedicated topic,
// and registers the protected "secretary" session. The server runs it on
// startup; `ccc secretary start` triggers the first launch.

import (
	"bufio"
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kidandcat/ccc/internal/mail"
	"github.com/kidandcat/ccc/internal/scheduler"
)

// Watchdog tuning. Variant 1 (skip-permissions autonomous secretary) means no
// interactive prompts to answer, so the watchdog is a thin process-alive safety
// net with flap protection.
const (
	secretaryWatchInterval = 30 * time.Second
	maxSecretaryRestarts   = 5
)

// secretaryRestarts tracks consecutive watchdog restarts per secretary identity.
var secretaryRestarts = map[string]int{}

// Scheduled secretary maintenance restart (daily, quiet hour). A fresh session
// picks up Claude Code binary updates, the current default model, new hooks —
// and heals poisoned contexts (degeneration loops like the "court" spam).
// Secretaries are restart-safe by design: they reconcile from inbox/journal.
const (
	secretaryRestartTimerID = "secretary-restart"
	secretaryRestartHour    = 1 // 01:30 UTC ≈ 04:30 Kyiv — quiet hour
	secretaryRestartMinute  = 30
)

// Between the daily maintenance restarts a secretary can still wedge mid-day —
// most often the "court" tool-call glitch, where `deliver` calls are emitted as
// plain text and never execute, so letters land in inbox (event=received) but are
// never `queued`. This health check catches that: a letter sitting unprocessed
// past the threshold ⇒ the secretary is stuck ⇒ restart it fresh (which heals the
// poisoned context and re-reconciles the inbox). Threshold is generous so a
// secretary that is merely slow or awaiting the human isn't cut off prematurely.
const (
	secretaryHealthTimerID     = "secretary-health"
	secretaryHealthCheckSecs   = 300 // scan every 5 min
	secretaryStuckThresholdSec = 600 // inbox letter unqueued ≥10 min ⇒ wedged
)

// nextDailyUTC returns the next unix time at hh:mm UTC strictly in the future.
func nextDailyUTC(hh, mm int) int64 {
	now := time.Now().UTC()
	cand := time.Date(now.Year(), now.Month(), now.Day(), hh, mm, 0, 0, time.UTC)
	if !cand.After(now) {
		cand = cand.AddDate(0, 0, 1)
	}
	return cand.Unix()
}

// restartSecretarySession relaunches one secretary with a FRESH context: kills
// its tmux (config untouched — killSession would mark it deleted), starts a new
// session, dismisses any startup onboarding prompt, and nudges it to reconcile
// so letters that arrived during the restart window are picked up immediately.
func restartSecretarySession(cfg *Config, group string) error {
	sec := mail.SecretaryName(group)
	info := cfg.Sessions[sec]
	if info == nil {
		return fmt.Errorf("secretary for group %q not configured", group)
	}
	if err := launchSecretary(cfg, sec, info); err != nil {
		return err
	}
	// Synchronous on purpose: `ccc secretary restart` is a short-lived CLI —
	// a goroutine would be killed on exit before the nudge is sent.
	tmux := tmuxSessionName(sec)
	time.Sleep(8 * time.Second) // let claude boot
	if out, err := tmuxCmd("capture-pane", "-t", tmux, "-p").Output(); err == nil {
		pane := string(out)
		if strings.Contains(pane, "Enter to confirm") || strings.Contains(pane, "try it") {
			tmuxCmd("send-keys", "-t", tmux, "Escape").Run()
			time.Sleep(time.Second)
		}
	}
	sendToTmux(tmux, "You were restarted (scheduled maintenance). Reconcile now per your manual: check inbox/ for unrouted letters and journal.jsonl for anything awaiting ack/reply.")
	return nil
}

// ensureSecretaryRestartTimer arms the daily maintenance-restart timer if it is
// not already pending. Called on server start.
func ensureSecretaryRestartTimer() {
	if mailScheduler == nil {
		return
	}
	if _, ok := mailScheduler.Get(secretaryRestartTimerID); ok {
		return
	}
	mailScheduler.Schedule(scheduler.Timer{
		ID:     secretaryRestartTimerID,
		FireAt: nextDailyUTC(secretaryRestartHour, secretaryRestartMinute),
		Kind:   "secretary_restart",
	})
}

// onSecretaryRestartTimer fires the daily maintenance restart: every enabled,
// currently-running, IDLE secretary is relaunched fresh. Busy ones are skipped
// (they'll be caught tomorrow — the restart is routine, not urgent). Always
// reschedules itself for the next day.
func onSecretaryRestartTimer(t scheduler.Timer) {
	defer func() {
		if mailScheduler != nil {
			t.FireAt = nextDailyUTC(secretaryRestartHour, secretaryRestartMinute)
			mailScheduler.Schedule(t)
		}
	}()
	cfg, err := loadConfig()
	if err != nil {
		return
	}
	for _, group := range append([]string{"default"}, namedGroups(cfg)...) {
		sec := mail.SecretaryName(group)
		if !secretaryEnabled(sec) {
			continue
		}
		tmux := tmuxSessionName(sec)
		if !tmuxSessionExists(tmux) {
			continue // down — the watchdog's business, not maintenance's
		}
		if checkClaudeState(tmux, "") == "busy" {
			fmt.Fprintf(os.Stderr, "maintenance: secretary %s busy, skipping restart this cycle\n", sec)
			continue
		}
		if err := restartSecretarySession(cfg, group); err != nil {
			fmt.Fprintf(os.Stderr, "maintenance: restart %s: %v\n", sec, err)
			continue
		}
		fmt.Fprintf(os.Stderr, "maintenance: secretary %s restarted (fresh session)\n", sec)
		time.Sleep(15 * time.Second) // stagger restarts, don't thrash all at once
	}
}

// ensureSecretaryHealthTimer arms the periodic wedged-secretary check if it is not
// already pending. Called on server start.
func ensureSecretaryHealthTimer() {
	if mailScheduler == nil {
		return
	}
	if _, ok := mailScheduler.Get(secretaryHealthTimerID); ok {
		return
	}
	mailScheduler.Schedule(scheduler.Timer{
		ID:     secretaryHealthTimerID,
		FireAt: time.Now().Unix() + secretaryHealthCheckSecs,
		Kind:   "secretary_health",
	})
}

// onSecretaryHealthTimer scans every enabled, running secretary for a stuck inbox
// and restarts any that are wedged. Busy secretaries are skipped (they may be
// mid-processing — catch them next cycle). Always reschedules itself.
func onSecretaryHealthTimer(t scheduler.Timer) {
	defer func() {
		if mailScheduler != nil {
			t.FireAt = time.Now().Unix() + secretaryHealthCheckSecs
			mailScheduler.Schedule(t)
		}
	}()
	cfg, err := loadConfig()
	if err != nil {
		return
	}
	for _, group := range append([]string{"default"}, namedGroups(cfg)...) {
		sec := mail.SecretaryName(group)
		if !secretaryEnabled(sec) {
			continue
		}
		tmux := tmuxSessionName(sec)
		if !tmuxSessionExists(tmux) {
			continue // down — the watchdog's business, not the health check's
		}
		stuck := secretaryStuckTickets(mail.MailboxDir(sec), secretaryStuckThresholdSec)
		if len(stuck) == 0 {
			continue
		}
		// Don't cut off a secretary that is actively working right now; if it's
		// genuinely wedged it will be idle and caught this or the next cycle.
		if checkClaudeState(tmux, "") == "busy" {
			fmt.Fprintf(os.Stderr, "secretary_health: %s has %d stuck letter(s) but is busy — skipping this cycle\n", sec, len(stuck))
			continue
		}
		fmt.Fprintf(os.Stderr, "secretary_health: %s wedged — %d letter(s) unqueued ≥%dm (e.g. %s); auto-restarting\n",
			sec, len(stuck), secretaryStuckThresholdSec/60, stuck[0])
		if err := restartSecretarySession(cfg, group); err != nil {
			fmt.Fprintf(os.Stderr, "secretary_health: restart %s: %v\n", sec, err)
			continue
		}
		time.Sleep(15 * time.Second) // stagger, don't thrash all at once
	}
}

// secretaryStuckTickets returns inbox tickets that have been sitting unprocessed
// (no `queued`/`delivered` journal event) for longer than thresholdSecs — the
// signature of a wedged secretary. File mtime is the arrival time; a healthy
// secretary queues within seconds of waking, so an old-but-unqueued letter means
// the deliver never executed.
func secretaryStuckTickets(dir string, thresholdSecs int64) []string {
	entries, err := os.ReadDir(filepath.Join(dir, "inbox"))
	if err != nil {
		return nil
	}
	now := time.Now().Unix()
	old := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if now-info.ModTime().Unix() >= thresholdSecs {
			old[strings.TrimSuffix(e.Name(), ".json")] = true
		}
	}
	if len(old) == 0 {
		return nil
	}
	processed := journalProcessedTickets(filepath.Join(dir, "journal.jsonl"), old)
	var stuck []string
	for ticket := range old {
		if !processed[ticket] {
			stuck = append(stuck, ticket)
		}
	}
	return stuck
}

// journalProcessedTickets scans journal.jsonl and returns the subset of `want`
// tickets that have a `queued` or `delivered` event (i.e. the secretary did route
// them). Bounded memory: only tracks the tickets asked about.
func journalProcessedTickets(journalPath string, want map[string]bool) map[string]bool {
	processed := map[string]bool{}
	f, err := os.Open(journalPath)
	if err != nil {
		return processed
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024) // journal lines carry subjects
	for sc.Scan() {
		var ev struct {
			Ticket string `json:"ticket"`
			Event  string `json:"event"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		if want[ev.Ticket] && (ev.Event == "queued" || ev.Event == "delivered") {
			processed[ev.Ticket] = true
		}
	}
	return processed
}

// namedGroups returns the configured non-default group aliases.
func namedGroups(cfg *Config) []string {
	var gs []string
	for alias := range cfg.Groups {
		if alias != "default" {
			gs = append(gs, alias)
		}
	}
	return gs
}

// allSecretaries returns every secretary identity (default + one per named group).
func allSecretaries(cfg *Config) []string {
	secs := []string{mail.SecretaryAgent}
	for _, g := range namedGroups(cfg) {
		secs = append(secs, mail.SecretaryName(g))
	}
	return secs
}

// secretaryEnabledMarker is a file `ccc secretary start` creates to opt a
// secretary into supervision. The watchdog acts only while it exists.
func secretaryEnabledMarker(sec string) string {
	return filepath.Join(mail.MailboxDir(sec), ".enabled")
}

func secretaryEnabled(sec string) bool {
	_, err := os.Stat(secretaryEnabledMarker(sec))
	return err == nil
}

func setSecretaryEnabled(sec string, on bool) error {
	if on {
		return os.WriteFile(secretaryEnabledMarker(sec), []byte("1\n"), 0644)
	}
	if err := os.Remove(secretaryEnabledMarker(sec)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// watchSecretary keeps every enabled secretary (default + per-group) alive,
// restarting a dead session with flap protection. Runs in the server.
func watchSecretary() {
	for {
		time.Sleep(secretaryWatchInterval)
		cfg, err := loadConfig()
		if err != nil {
			continue
		}
		for _, sec := range allSecretaries(cfg) {
			superviseSecretary(cfg, sec)
		}
	}
}

func superviseSecretary(cfg *Config, sec string) {
	if !secretaryEnabled(sec) {
		secretaryRestarts[sec] = 0
		return
	}
	info := cfg.Sessions[sec]
	if info == nil {
		return
	}
	tmux := tmuxSessionName(sec)
	if isClaudeRunning(tmux, "") {
		secretaryRestarts[sec] = 0
		return
	}
	if secretaryRestarts[sec] >= maxSecretaryRestarts {
		return // paused until it recovers or Vlad intervenes
	}
	secretaryRestarts[sec]++
	fmt.Fprintf(os.Stderr, "watchdog: %s down, restarting (attempt %d)\n", sec, secretaryRestarts[sec])
	if err := launchSecretary(cfg, sec, info); err != nil {
		fmt.Fprintf(os.Stderr, "watchdog: restart failed: %v\n", err)
		return
	}
	if info.TopicID > 0 {
		chat := groupChatID(cfg, sessionGroup(info))
		sendMessage(cfg, chat, info.TopicID,
			fmt.Sprintf("🔁 Secretary was down; watchdog restarted it (attempt %d/%d).", secretaryRestarts[sec], maxSecretaryRestarts))
		if secretaryRestarts[sec] >= maxSecretaryRestarts {
			sendMessage(cfg, chat, info.TopicID,
				"⚠️ Secretary keeps crashing; auto-restart paused. Investigate, then run `ccc secretary start`.")
		}
	}
}

//go:embed all:secretary
var secretaryTemplateFS embed.FS

// installSecretaryTemplate copies the embedded template tree into dir, skipping
// any file that already exists (so Vlad's edits and accumulated mail survive).
func installSecretaryTemplate(dir string) error {
	return fs.WalkDir(secretaryTemplateFS, "secretary", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel := strings.TrimPrefix(path, "secretary/")
		target := filepath.Join(dir, rel)
		if _, statErr := os.Stat(target); statErr == nil {
			return nil // already present — do not clobber
		}
		data, err := secretaryTemplateFS.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0644)
	})
}

// prepareSecretaryDir ensures a secretary's working directory, mailbox subdirs,
// and template files exist. sec is the secretary identity ("secretary" or
// "secretary-<alias>"). No Telegram/tmux side effects (unit-testable).
func prepareSecretaryDir(sec string) (string, error) {
	dir := mail.MailboxDir(sec) // ~/.ccc/secretary or ~/.ccc/secretary-<alias>
	for _, sub := range []string{"", "inbox", "archive"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0755); err != nil {
			return "", err
		}
	}
	if err := installSecretaryTemplate(dir); err != nil {
		return "", err
	}
	return dir, nil
}

// bootstrapSecretary ensures a secretary for every group (default + named).
// Idempotent; safe to call on every server start. The default group's secretary
// stays at ~/.ccc/secretary with its existing topic.
func bootstrapSecretary(cfg *Config) error {
	for _, group := range append([]string{"default"}, namedGroups(cfg)...) {
		if err := bootstrapGroupSecretary(cfg, group); err != nil {
			fmt.Fprintf(os.Stderr, "warning: secretary bootstrap (group %q): %v\n", group, err)
		}
	}
	return nil
}

// bootstrapGroupSecretary ensures one group's secretary: working dir + template,
// folder trust, a topic in THAT group's chat, and a protected session entry.
func bootstrapGroupSecretary(cfg *Config, group string) error {
	sec := mail.SecretaryName(group)
	dir, err := prepareSecretaryDir(sec)
	if err != nil {
		return err
	}
	// Pre-accept the folder-trust dialog so the autonomous secretary never
	// blocks on it (skip-permissions does NOT cover folder trust). Best-effort.
	if err := ensureSecretaryTrusted(dir); err != nil {
		fmt.Fprintf(os.Stderr, "warning: trust %s: %v\n", sec, err)
	}
	groupField := group
	if group == "default" {
		groupField = "" // keep the default secretary's Group empty, as before
	}
	// Existing secretary: sync group/path, verify/repair its topic in its group.
	if info, ok := cfg.Sessions[sec]; ok && info != nil {
		info.Group, info.Path = groupField, dir
		saveConfig(cfg)
		if _, err := getOrCreateTopic(cfg, sec, dir, ""); err != nil {
			return fmt.Errorf("secretary topic: %w", err)
		}
		return nil
	}
	// New secretary: create its topic in the group's chat.
	chat := groupChatID(cfg, group)
	if chat == 0 {
		return fmt.Errorf("group %q has no chat_id configured", group)
	}
	topicID, err := createForumTopic(cfg, chat, sec)
	if err != nil {
		return fmt.Errorf("secretary topic: %w", err)
	}
	cfg.Sessions[sec] = &SessionInfo{TopicID: topicID, Path: dir, Group: groupField}
	saveConfig(cfg)
	return nil
}

// ensureSecretaryTrusted sets projects[dir].hasTrustDialogAccepted=true in
// ~/.claude.json so Claude Code does not show the trust prompt for the
// secretary's folder (which --dangerously-skip-permissions does not suppress).
// Idempotent (no write if already set). Uses UseNumber + full preserve so the
// rest of the user's claude config is kept byte-faithful; atomic rename.
func ensureSecretaryTrusted(dir string) error {
	home, _ := os.UserHomeDir()
	return ensureTrustedInConfigDir(filepath.Join(home, ".claude.json"), dir)
}

// ensureTrustedInConfigDir sets projects[dir].hasTrustDialogAccepted=true in the
// given .claude.json (a specific CLAUDE_CONFIG_DIR's config file), so Claude Code
// shows no folder-trust prompt for dir. This gate is NOT suppressed by
// --dangerously-skip-permissions, so a fresh account config would otherwise
// block every headless agent. Idempotent; atomic write.
func ensureTrustedInConfigDir(path, dir string) error {
	root := map[string]interface{}{}
	if data, err := os.ReadFile(path); err == nil {
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.UseNumber() // keep integers exact, do not turn them into floats
		if err := dec.Decode(&root); err != nil {
			return fmt.Errorf("parse ~/.claude.json: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	projects, ok := root["projects"].(map[string]interface{})
	if !ok || projects == nil {
		projects = map[string]interface{}{}
		root["projects"] = projects
	}
	entry, ok := projects[dir].(map[string]interface{})
	if !ok || entry == nil {
		entry = map[string]interface{}{}
		projects[dir] = entry
	}
	if trusted, _ := entry["hasTrustDialogAccepted"].(bool); trusted {
		return nil // already trusted — no write
	}
	entry["hasTrustDialogAccepted"] = true

	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".ccc.tmp"
	if err := os.WriteFile(tmp, out, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// canonicalSecretaryMcp is the stdio MCP entry that gives an agent the inter-agent
// mail tools. It is identity-agnostic — the `ccc mcp-secretary` process derives the
// caller's identity from its working directory at runtime — so the exact same entry
// works for every agent and every secretary. Matches the entry in the default
// ~/.claude.json; keeping it in one place means a future change to how mcp-secretary
// is invoked propagates to all account config dirs on their next `ccc run`.
func canonicalSecretaryMcp() map[string]interface{} {
	return map[string]interface{}{
		"type":    "stdio",
		"command": "ccc",
		"args":    []interface{}{"mcp-secretary"},
		"env":     map[string]interface{}{},
	}
}

// ensureSecretaryMcpInConfigDir makes sure mcpServers.secretary exists in the given
// .claude.json (a specific CLAUDE_CONFIG_DIR's config). A fresh subscription-account
// config dir has an empty mcpServers, so agents/secretaries launched under it boot
// WITHOUT the mail tool and silently cannot send or deliver letters. Claude Code reads
// mcpServers only at session start, so this must be in place BEFORE launch. Idempotent
// (no write if a secretary entry already points at `ccc`); atomic write; preserves the
// rest of the config byte-faithfully (UseNumber).
func ensureSecretaryMcpInConfigDir(path string) error {
	root := map[string]interface{}{}
	if data, err := os.ReadFile(path); err == nil {
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.UseNumber()
		if err := dec.Decode(&root); err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	servers, ok := root["mcpServers"].(map[string]interface{})
	if !ok || servers == nil {
		servers = map[string]interface{}{}
		root["mcpServers"] = servers
	}
	if existing, ok := servers["secretary"].(map[string]interface{}); ok {
		if cmd, _ := existing["command"].(string); cmd == "ccc" {
			return nil // already present — no write
		}
	}
	servers["secretary"] = canonicalSecretaryMcp()

	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".ccc.tmp"
	if err := os.WriteFile(tmp, out, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// launchSecretary starts a FRESH secretary session (no -c). The secretary's
// durable state is its files (inbox/journal), so it reconciles on start and
// needs no claude conversation continuity — which also avoids the "No
// conversation found to continue" failure of a first launch. A stale/dead
// session is killed first.
func launchSecretary(cfg *Config, sec string, info *SessionInfo) error {
	tmux := tmuxSessionName(sec)
	if tmuxSessionExists(tmux) {
		tmuxCmd("kill-session", "-t", tmux).Run()
		time.Sleep(300 * time.Millisecond)
	}
	return createTmuxSession(tmux, info.Path, false)
}

// secretaryCommand implements `ccc secretary [start|stop|status] [group-alias]`.
// The group alias defaults to "default".
func secretaryCommand(args []string) error {
	sub := "status"
	if len(args) > 0 {
		sub = args[0]
	}
	group := "default"
	if len(args) > 1 {
		group = args[1]
	}
	sec := mail.SecretaryName(group)
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	switch sub {
	case "start":
		if err := bootstrapGroupSecretary(cfg, group); err != nil {
			return err
		}
		cfg, err = loadConfig() // reload to pick up the session entry bootstrap created
		if err != nil {
			return err
		}
		info := cfg.Sessions[sec]
		if info == nil {
			return fmt.Errorf("secretary for group %q not configured after bootstrap", group)
		}
		if err := setSecretaryEnabled(sec, true); err != nil { // opt into supervision
			return err
		}
		secretaryRestarts[sec] = 0
		if isClaudeRunning(tmuxSessionName(sec), "") {
			fmt.Printf("Secretary [%s] already running — topic %d.\n", group, info.TopicID)
			return nil
		}
		if err := launchSecretary(cfg, sec, info); err != nil {
			return fmt.Errorf("start failed: %w", err)
		}
		fmt.Printf("✅ Secretary [%s] started (supervised) — topic %d, dir %s\n", group, info.TopicID, info.Path)
		return nil

	case "stop":
		if err := setSecretaryEnabled(sec, false); err != nil { // disable watchdog first
			return err
		}
		if err := killSession(cfg, sec); err != nil {
			return err
		}
		fmt.Printf("⏹  Secretary [%s] stopped (watchdog disabled).\n", group)
		return nil

	case "restart":
		// Fresh-context relaunch (config/supervision untouched). Safe: a
		// secretary keeps all state in inbox/journal files, not in context.
		if err := restartSecretarySession(cfg, group); err != nil {
			return fmt.Errorf("restart failed: %w", err)
		}
		fmt.Printf("🔄 Secretary [%s] restarted with a fresh session.\n", group)
		return nil

	case "status":
		if len(args) <= 1 { // no alias -> show all secretaries
			for _, g := range append([]string{"default"}, namedGroups(cfg)...) {
				printSecretaryStatus(cfg, g)
			}
			return nil
		}
		printSecretaryStatus(cfg, group)
		return nil

	default:
		return fmt.Errorf("usage: ccc secretary [start|stop|restart|status] [group-alias]")
	}
}

func printSecretaryStatus(cfg *Config, group string) {
	sec := mail.SecretaryName(group)
	info := cfg.Sessions[sec]
	if info == nil {
		fmt.Printf("Secretary [%s]: not bootstrapped\n", group)
		return
	}
	state := checkClaudeState(tmuxSessionName(sec), "")
	fmt.Printf("Secretary [%s]: topic %d, dir %s, state %s, supervised %t\n",
		group, info.TopicID, info.Path, state, secretaryEnabled(sec))
}
