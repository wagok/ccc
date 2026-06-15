package main

// secretary_bootstrap.go brings the smart secretary agent to life. The
// secretary's working directory is hardcoded (~/.ccc/secretary) and its template
// (instruction + .mcp.json) is shipped inside the binary via go:embed, so no
// repository checkout is needed at runtime. Bootstrap is idempotent: it installs
// missing template files (never clobbering edits), ensures the dedicated topic,
// and registers the protected "secretary" session. The server runs it on
// startup; `ccc secretary start` triggers the first launch.

import (
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
)

// Watchdog tuning. Variant 1 (skip-permissions autonomous secretary) means no
// interactive prompts to answer, so the watchdog is a thin process-alive safety
// net with flap protection.
const (
	secretaryWatchInterval = 30 * time.Second
	maxSecretaryRestarts   = 5
)

var secretaryRestarts int

// secretaryEnabledMarker is a file Vlad's `ccc secretary start` creates to opt
// the secretary into supervision. The watchdog restarts the session only while
// this marker exists, so a prepared-but-never-started secretary is left alone.
func secretaryEnabledMarker() string {
	return filepath.Join(mail.MailboxDir(mail.SecretaryAgent), ".enabled")
}

func secretaryEnabled() bool {
	_, err := os.Stat(secretaryEnabledMarker())
	return err == nil
}

func setSecretaryEnabled(on bool) error {
	if on {
		return os.WriteFile(secretaryEnabledMarker(), []byte("1\n"), 0644)
	}
	if err := os.Remove(secretaryEnabledMarker()); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// watchSecretary keeps the secretary session alive once enabled. It restarts a
// dead session (continuing its context with -c), with flap protection: after
// maxSecretaryRestarts consecutive failures it pauses and asks Vlad to step in.
// The counter resets whenever the session is found healthy. Runs in the server.
func watchSecretary() {
	for {
		time.Sleep(secretaryWatchInterval)
		if !secretaryEnabled() {
			secretaryRestarts = 0
			continue
		}
		cfg, err := loadConfig()
		if err != nil {
			continue
		}
		info := cfg.Sessions[mail.SecretaryAgent]
		if info == nil {
			continue
		}
		tmux := tmuxSessionName(mail.SecretaryAgent)
		if isClaudeRunning(tmux, "") {
			secretaryRestarts = 0
			continue
		}
		if secretaryRestarts >= maxSecretaryRestarts {
			continue // paused until the session comes back up or Vlad intervenes
		}
		secretaryRestarts++
		fmt.Fprintf(os.Stderr, "watchdog: secretary down, restarting (attempt %d)\n", secretaryRestarts)
		if err := launchSecretary(cfg, info); err != nil {
			fmt.Fprintf(os.Stderr, "watchdog: restart failed: %v\n", err)
			continue
		}
		if info.TopicID > 0 {
			sendMessage(cfg, cfg.GroupID, info.TopicID,
				fmt.Sprintf("🔁 Secretary was down; watchdog restarted it (attempt %d/%d).", secretaryRestarts, maxSecretaryRestarts))
			if secretaryRestarts >= maxSecretaryRestarts {
				sendMessage(cfg, cfg.GroupID, info.TopicID,
					"⚠️ Secretary keeps crashing; auto-restart paused. Investigate, then run `ccc secretary start`.")
			}
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

// prepareSecretaryDir ensures the secretary's working directory, mailbox
// subdirs, and template files exist. Returns the directory path. No Telegram or
// tmux side effects (so it is unit-testable on its own).
func prepareSecretaryDir() (string, error) {
	dir := mail.MailboxDir(mail.SecretaryAgent) // ~/.ccc/secretary
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

// bootstrapSecretary prepares the working dir and ensures the dedicated topic +
// protected session entry. Idempotent; safe to call on every server start.
func bootstrapSecretary(cfg *Config) error {
	dir, err := prepareSecretaryDir()
	if err != nil {
		return err
	}
	// Pre-accept the folder-trust dialog so the autonomous secretary never
	// blocks on it (skip-permissions does NOT cover folder trust). Best-effort.
	if err := ensureSecretaryTrusted(dir); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not pre-accept secretary folder trust: %v\n", err)
	}
	if _, err := getOrCreateTopic(cfg, mail.SecretaryAgent, dir, ""); err != nil {
		return fmt.Errorf("secretary topic: %w", err)
	}
	return nil
}

// ensureSecretaryTrusted sets projects[dir].hasTrustDialogAccepted=true in
// ~/.claude.json so Claude Code does not show the trust prompt for the
// secretary's folder (which --dangerously-skip-permissions does not suppress).
// Idempotent (no write if already set). Uses UseNumber + full preserve so the
// rest of the user's claude config is kept byte-faithful; atomic rename.
func ensureSecretaryTrusted(dir string) error {
	home, _ := os.UserHomeDir()
	path := filepath.Join(home, ".claude.json")

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

// launchSecretary starts a FRESH secretary session (no -c). The secretary's
// durable state is its files (inbox/journal), so it reconciles on start and
// needs no claude conversation continuity — which also avoids the "No
// conversation found to continue" failure of a first launch. A stale/dead
// session is killed first.
func launchSecretary(cfg *Config, info *SessionInfo) error {
	tmux := tmuxSessionName(mail.SecretaryAgent)
	if tmuxSessionExists(tmux) {
		tmuxCmd("kill-session", "-t", tmux).Run()
		time.Sleep(300 * time.Millisecond)
	}
	return createTmuxSession(tmux, info.Path, false)
}

// secretaryCommand implements `ccc secretary [start|status]`.
func secretaryCommand(args []string) error {
	sub := "status"
	if len(args) > 0 {
		sub = args[0]
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	switch sub {
	case "start":
		if err := bootstrapSecretary(cfg); err != nil {
			return err
		}
		cfg, err = loadConfig() // reload to pick up the session entry bootstrap created
		if err != nil {
			return err
		}
		info := cfg.Sessions[mail.SecretaryAgent]
		if info == nil {
			return fmt.Errorf("secretary not configured after bootstrap")
		}
		if err := setSecretaryEnabled(true); err != nil { // opt into watchdog supervision
			return err
		}
		secretaryRestarts = 0
		tmux := tmuxSessionName(mail.SecretaryAgent)
		if isClaudeRunning(tmux, "") {
			fmt.Printf("Secretary already running — topic %d.\n", info.TopicID)
			return nil
		}
		if err := launchSecretary(cfg, info); err != nil {
			return fmt.Errorf("start failed: %w", err)
		}
		fmt.Printf("✅ Secretary started (supervised) — topic %d, dir %s\n", info.TopicID, info.Path)
		return nil

	case "stop":
		if err := setSecretaryEnabled(false); err != nil { // disable watchdog first
			return err
		}
		if err := killSession(cfg, mail.SecretaryAgent); err != nil {
			return err
		}
		fmt.Println("⏹  Secretary stopped (watchdog disabled).")
		return nil

	case "status":
		info := cfg.Sessions[mail.SecretaryAgent]
		if info == nil {
			fmt.Println("Secretary: not bootstrapped (start the server or run `ccc secretary start`)")
			return nil
		}
		state := checkClaudeState(tmuxSessionName(mail.SecretaryAgent), "")
		fmt.Printf("Secretary: topic %d, dir %s, state %s, supervised %t\n",
			info.TopicID, info.Path, state, secretaryEnabled())
		return nil

	default:
		return fmt.Errorf("usage: ccc secretary [start|stop|status]")
	}
}
