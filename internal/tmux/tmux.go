// Package tmux provides tmux session management functionality.
package tmux

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Manager handles tmux session operations
type Manager struct {
	SocketPath string
	BinaryPath string
	CCCPath    string // Path to ccc binary for running inside sessions
}

// NewManager creates a new tmux manager with auto-detected paths
func NewManager() *Manager {
	m := &Manager{}
	m.detectPaths()
	return m
}

// detectPaths finds tmux socket and binary paths
func (m *Manager) detectPaths() {
	// Find tmux socket path using current UID
	// macOS uses /private/tmp, Linux uses /tmp
	uid := os.Getuid()
	macOSSocket := fmt.Sprintf("/private/tmp/tmux-%d/default", uid)
	linuxSocket := fmt.Sprintf("/tmp/tmux-%d/default", uid)

	// Check which socket exists, prefer Linux path first
	if _, err := os.Stat(linuxSocket); err == nil {
		m.SocketPath = linuxSocket
	} else if _, err := os.Stat(macOSSocket); err == nil {
		m.SocketPath = macOSSocket
	} else {
		// Default based on OS
		if _, err := os.Stat("/private"); err == nil {
			m.SocketPath = macOSSocket
		} else {
			m.SocketPath = linuxSocket
		}
	}

	// Find tmux binary
	if path, err := exec.LookPath("tmux"); err == nil {
		m.BinaryPath = path
	} else {
		// Fallback paths for common installations
		for _, p := range []string{"/opt/homebrew/bin/tmux", "/usr/local/bin/tmux", "/usr/bin/tmux"} {
			if _, err := os.Stat(p); err == nil {
				m.BinaryPath = p
				break
			}
		}
	}

	// Find ccc binary (self)
	if exe, err := os.Executable(); err == nil {
		m.CCCPath = exe
	}
}

// SessionExists checks if a tmux session exists
func (m *Manager) SessionExists(name string) bool {
	cmd := exec.Command(m.BinaryPath, "-S", m.SocketPath, "has-session", "-t", name)
	return cmd.Run() == nil
}

// CreateSession creates a new tmux session
func (m *Manager) CreateSession(name string, workDir string, continueSession bool) error {
	// Build the command to run inside tmux
	cccCmd := m.CCCPath + " run"
	if continueSession {
		cccCmd += " -c"
	}

	// Create tmux session with a login shell (don't run command directly - it kills session on exit)
	args := []string{"-S", m.SocketPath, "new-session", "-d", "-s", name, "-c", workDir}
	cmd := exec.Command(m.BinaryPath, args...)
	if err := cmd.Run(); err != nil {
		return err
	}

	// Enable mouse mode for this session (allows scrolling)
	exec.Command(m.BinaryPath, "-S", m.SocketPath, "set-option", "-t", name, "mouse", "on").Run()

	// Send the command to the session via send-keys (preserves TTY properly)
	time.Sleep(200 * time.Millisecond)
	exec.Command(m.BinaryPath, "-S", m.SocketPath, "send-keys", "-t", name, cccCmd, "C-m").Run()

	return nil
}

// AttachSession attaches to an existing session
func (m *Manager) AttachSession(name string) error {
	// Check if we're already inside tmux
	if os.Getenv("TMUX") != "" {
		// Inside tmux: switch to the session
		cmd := exec.Command(m.BinaryPath, "-S", m.SocketPath, "switch-client", "-t", name)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}
	// Outside tmux: attach to existing session
	cmd := exec.Command(m.BinaryPath, "-S", m.SocketPath, "attach-session", "-t", name)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// SendKeys sends text to a tmux session
func (m *Manager) SendKeys(session string, text string) error {
	// For long messages, use longer delay for Claude to process pasted content
	delay := 50 * time.Millisecond
	if len(text) > 200 {
		delay = 2 * time.Second
	}
	return m.SendKeysWithDelay(session, text, delay)
}

// SendKeysWithDelay sends text to a tmux session with a custom delay
func (m *Manager) SendKeysWithDelay(session string, text string, delay time.Duration) error {
	// Send text literally
	cmd := exec.Command(m.BinaryPath, "-S", m.SocketPath, "send-keys", "-t", session, "-l", text)
	if err := cmd.Run(); err != nil {
		return err
	}

	// Wait for content to load (e.g., images, long pasted text)
	time.Sleep(delay)

	// Send Enter twice (Claude Code needs double Enter)
	cmd = exec.Command(m.BinaryPath, "-S", m.SocketPath, "send-keys", "-t", session, "C-m")
	if err := cmd.Run(); err != nil {
		return err
	}
	time.Sleep(50 * time.Millisecond)
	cmd = exec.Command(m.BinaryPath, "-S", m.SocketPath, "send-keys", "-t", session, "C-m")
	return cmd.Run()
}

// KillSession kills a tmux session
func (m *Manager) KillSession(name string) error {
	cmd := exec.Command(m.BinaryPath, "-S", m.SocketPath, "kill-session", "-t", name)
	return cmd.Run()
}

// ListSessions lists all claude-prefixed sessions
func (m *Manager) ListSessions() ([]string, error) {
	cmd := exec.Command(m.BinaryPath, "-S", m.SocketPath, "list-sessions", "-F", "#{session_name}")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	var sessions []string
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		name := scanner.Text()
		if strings.HasPrefix(name, "claude-") {
			sessions = append(sessions, strings.TrimPrefix(name, "claude-"))
		}
	}
	return sessions, nil
}

// SessionName returns the tmux session name for a project
func SessionName(name string) string {
	return "claude-" + name
}
