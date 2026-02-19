package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kidandcat/ccc/internal/config"
)

const version = "1.0.0"

// Type aliases for backward compatibility during migration
type SessionInfo = config.SessionInfo
type HostInfo = config.HostInfo
type Config = config.Config

// TelegramMessage represents a Telegram message
type TelegramMessage struct {
	MessageID       int    `json:"message_id"`
	MessageThreadID int64  `json:"message_thread_id,omitempty"` // Topic ID
	Chat            struct {
		ID   int64  `json:"id"`
		Type string `json:"type"` // "private", "group", "supergroup"
	} `json:"chat"`
	From struct {
		ID       int64  `json:"id"`
		Username string `json:"username"`
	} `json:"from"`
	Text           string           `json:"text"`
	ReplyToMessage *TelegramMessage `json:"reply_to_message,omitempty"`
	Voice          *TelegramVoice   `json:"voice,omitempty"`
	Photo          []TelegramPhoto  `json:"photo,omitempty"`
	Caption        string           `json:"caption,omitempty"`
}

type TelegramVoice struct {
	FileID   string `json:"file_id"`
	Duration int    `json:"duration"`
}

type TelegramPhoto struct {
	FileID   string `json:"file_id"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	FileSize int    `json:"file_size"`
}

// CallbackQuery represents a Telegram callback query (button press)
type CallbackQuery struct {
	ID   string `json:"id"`
	From struct {
		ID int64 `json:"id"`
	} `json:"from"`
	Message *TelegramMessage `json:"message"`
	Data    string           `json:"data"`
}

// TelegramUpdate represents an update from Telegram
type TelegramUpdate struct {
	OK          bool   `json:"ok"`
	Description string `json:"description"`
	Result      []struct {
		UpdateID      int             `json:"update_id"`
		Message       TelegramMessage `json:"message"`
		CallbackQuery *CallbackQuery  `json:"callback_query"`
	} `json:"result"`
}

// TelegramResponse represents a response from Telegram API
type TelegramResponse struct {
	OK          bool            `json:"ok"`
	Description string          `json:"description,omitempty"`
	Result      json.RawMessage `json:"result,omitempty"`
}

// TopicResult represents the result of creating a forum topic
type TopicResult struct {
	MessageThreadID int64  `json:"message_thread_id"`
	Name            string `json:"name"`
}

// HookData represents data received from Claude hook
type HookData struct {
	Cwd            string `json:"cwd"`
	TranscriptPath string `json:"transcript_path"`
	SessionID      string `json:"session_id"`
	HookEventName  string `json:"hook_event_name"`
	ToolName       string `json:"tool_name"`
	Prompt         string `json:"prompt"` // For UserPromptSubmit hook
	ToolInput      struct {
		Questions []struct {
			Question    string `json:"question"`
			Header      string `json:"header"`
			MultiSelect bool   `json:"multiSelect"`
			Options     []struct {
				Label       string `json:"label"`
				Description string `json:"description"`
			} `json:"options"`
		} `json:"questions"`
	} `json:"tool_input"`
}

// ============================================================================
// Local API Types and Functions (Unix Socket)
// ============================================================================

// APIRequest represents an incoming request on the Unix socket
type APIRequest struct {
	Cmd        string   `json:"cmd"`                   // ping, sessions, ask, send, history, screenshot, subscribe
	Session    string   `json:"session,omitempty"`     // session name
	Text       string   `json:"text,omitempty"`        // message text
	From       string   `json:"from,omitempty"`        // agent identifier
	After      int64    `json:"after,omitempty"`       // for history: after message_id
	Limit      int      `json:"limit,omitempty"`       // for history: max messages
	FromFilter string   `json:"from_filter,omitempty"` // for history: filter by sender (human, claude, api)
	Sessions   []string `json:"sessions,omitempty"`    // for subscribe: session list
}

// APIResponse represents a response on the Unix socket
type APIResponse struct {
	OK             bool              `json:"ok"`
	Error          string            `json:"error,omitempty"`
	Sessions       []APISessionInfo  `json:"sessions,omitempty"`
	Response       string            `json:"response,omitempty"`
	MessageID      int64             `json:"message_id,omitempty"`
	Messages       []HistoryMessage  `json:"messages,omitempty"`
	Duration       int64             `json:"duration_ms,omitempty"`
	Version        string            `json:"version,omitempty"`
	UptimeSeconds  int64             `json:"uptime_seconds,omitempty"`
	SessionsActive int              `json:"sessions_active,omitempty"`
}

// APIEvent represents a streaming event for subscribe
type APIEvent struct {
	Event   string `json:"event"`             // subscribed, message, status
	Session string `json:"session,omitempty"`
	From    string `json:"from,omitempty"`    // human, claude, api
	Text    string `json:"text,omitempty"`
	Status  string `json:"status,omitempty"`  // active, idle
}

// APISessionInfo represents session info in API response
type APISessionInfo struct {
	Name         string `json:"name"`
	Host         string `json:"host"`                    // "local" or host name
	Status       string `json:"status"`                  // "active", "idle"
	Cwd          string `json:"cwd,omitempty"`           // project working directory
	LastActivity int64  `json:"last_activity,omitempty"` // unix timestamp of last history entry
}

// HistoryMessage represents a message stored in history
type HistoryMessage struct {
	ID            int64  `json:"id"`
	Timestamp     int64  `json:"ts"`
	From          string `json:"from"`                    // human, claude, api
	Text          string `json:"text,omitempty"`
	Type          string `json:"type,omitempty"`          // text, voice, photo, document
	Path          string `json:"path,omitempty"`          // artifact path
	Transcription string `json:"transcription,omitempty"` // for voice
	Caption       string `json:"caption,omitempty"`       // for photo/document
	Agent         string `json:"agent,omitempty"`         // for api messages
	Username      string `json:"username,omitempty"`      // telegram username
}

// Server start time for uptime calculation
var serverStartTime time.Time

// Global message ID counter (in-memory, initialized from history on start)
var (
	messageIDCounter int64
	messageIDMutex   sync.Mutex
)

// activeCaptures tracks ongoing background response captures per session
var activeCaptures sync.Map // key: session name (string), value: bool

func nextMessageID() int64 {
	messageIDMutex.Lock()
	defer messageIDMutex.Unlock()
	messageIDCounter++
	return messageIDCounter
}

// initMessageIDCounter initializes the counter from existing history files
func initMessageIDCounter() {
	homeDir, _ := os.UserHomeDir()
	historyBase := filepath.Join(homeDir, ".ccc", "history")

	var maxID int64

	// Walk through all history directories
	filepath.Walk(historyBase, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}

		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()

		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			var msg struct {
				ID int64 `json:"id"`
			}
			if json.Unmarshal(scanner.Bytes(), &msg) == nil && msg.ID > maxID {
				maxID = msg.ID
			}
		}
		return nil
	})

	messageIDMutex.Lock()
	messageIDCounter = maxID
	messageIDMutex.Unlock()

	if maxID > 0 {
		fmt.Printf("Message ID counter initialized to %d\n", maxID)
	}
}

// getHistoryDir returns the history directory for a topic
func getHistoryDir(topicID int64) string {
	homeDir, _ := os.UserHomeDir()
	return filepath.Join(homeDir, ".ccc", "history", fmt.Sprintf("%d", topicID), "messages")
}

// getHistoryFile returns the history file path for current hour
func getHistoryFile(topicID int64) string {
	hour := time.Now().Format("2006-01-02-15")
	return filepath.Join(getHistoryDir(topicID), hour+".jsonl")
}

// appendHistory appends a message to the history file
func appendHistory(topicID int64, msg HistoryMessage) error {
	if topicID == 0 {
		return nil // Skip private chats without topic
	}

	dir := getHistoryDir(topicID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	filePath := getHistoryFile(topicID)
	f, err := os.OpenFile(filePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	return json.NewEncoder(f).Encode(msg)
}

// readHistory reads messages from history files
func readHistory(topicID int64, afterID int64, limit int, fromFilter string) ([]HistoryMessage, error) {
	if limit <= 0 {
		limit = 100
	}

	dir := getHistoryDir(topicID)
	files, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil {
		return nil, err
	}

	// Sort files in reverse order (newest first) - filenames are sortable
	for i, j := 0, len(files)-1; i < j; i, j = i+1, j-1 {
		files[i], files[j] = files[j], files[i]
	}

	var messages []HistoryMessage
	for _, file := range files {
		if len(messages) >= limit {
			break
		}

		f, err := os.Open(file)
		if err != nil {
			continue
		}

		var fileMessages []HistoryMessage
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			var msg HistoryMessage
			if err := json.Unmarshal(scanner.Bytes(), &msg); err == nil {
				if msg.ID > afterID {
					if fromFilter != "" && msg.From != fromFilter {
						continue
					}
					fileMessages = append(fileMessages, msg)
				}
			}
		}
		f.Close()

		// Prepend to messages (older files first after reversal)
		messages = append(fileMessages, messages...)
	}

	// Trim to limit (keep newest)
	if len(messages) > limit {
		messages = messages[len(messages)-limit:]
	}

	return messages, nil
}

// socketPath returns the Unix socket path
func socketPath() string {
	homeDir, _ := os.UserHomeDir()
	return filepath.Join(homeDir, ".ccc.sock")
}

// Global socket listener for cleanup
var socketListener net.Listener

// startSocketServer starts the Unix socket API server
func startSocketServer(cfg *Config) error {
	serverStartTime = time.Now()
	path := socketPath()

	// Remove existing socket file
	os.Remove(path)

	listener, err := net.Listen("unix", path)
	if err != nil {
		return fmt.Errorf("failed to create socket: %w", err)
	}
	socketListener = listener

	// Set socket permissions (owner only)
	os.Chmod(path, 0600)

	fmt.Printf("API socket: %s\n", path)

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				// Check if listener was closed
				if strings.Contains(err.Error(), "use of closed") {
					return
				}
				continue
			}
			go handleSocketConnection(conn, cfg)
		}
	}()

	return nil
}

// stopSocketServer stops the Unix socket server
func stopSocketServer() {
	if socketListener != nil {
		socketListener.Close()
		os.Remove(socketPath())
	}
}

// handleSocketConnection handles a single socket connection
func handleSocketConnection(conn net.Conn, cfg *Config) {
	defer conn.Close()

	reader := bufio.NewReader(conn)
	encoder := json.NewEncoder(conn)

	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return // Connection closed
		}

		var req APIRequest
		if err := json.Unmarshal(line, &req); err != nil {
			encoder.Encode(APIResponse{OK: false, Error: "invalid JSON"})
			continue
		}

		switch req.Cmd {
		case "ping":
			handlePingCmd(encoder, cfg)
		case "sessions":
			handleSessionsCmd(encoder, cfg)
		case "ask":
			handleAskCmd(encoder, cfg, req)
		case "send":
			handleSendCmd(encoder, cfg, req)
		case "history":
			handleHistoryCmd(encoder, cfg, req)
		case "screenshot":
			handleScreenshotCmd(encoder, cfg, req)
		case "subscribe":
			handleSubscribeCmd(conn, encoder, cfg, req)
			return // Subscribe keeps connection open until done
		default:
			encoder.Encode(APIResponse{OK: false, Error: "unknown command"})
		}
	}
}

// handlePingCmd handles the "ping" command
func handlePingCmd(encoder *json.Encoder, cfg *Config) {
	// Count configured (non-deleted) sessions — no tmux/SSH calls for fast response
	total := 0
	for _, info := range cfg.Sessions {
		if !info.Deleted {
			total++
		}
	}

	encoder.Encode(APIResponse{
		OK:             true,
		Version:        version,
		UptimeSeconds:  int64(time.Since(serverStartTime).Seconds()),
		SessionsActive: total,
	})
}

// handleSessionsCmd handles the "sessions" command
func handleSessionsCmd(encoder *json.Encoder, cfg *Config) {
	var sessions []APISessionInfo

	for name, info := range cfg.Sessions {
		if info.Deleted {
			continue
		}

		status := "idle"
		tmuxName := tmuxSessionName(name)

		if info.Host != "" {
			// Remote session
			address := getHostAddress(cfg, info.Host)
			if address != "" && sshTmuxHasSession(address, tmuxName) {
				if checkClaudeState(tmuxName, address) == "busy" {
					status = "active"
				}
			}
		} else {
			// Local session
			if tmuxSessionExists(tmuxName) {
				if checkClaudeState(tmuxName, "") == "busy" {
					status = "active"
				}
			}
		}

		host := "local"
		if info.Host != "" {
			host = info.Host
		}

		// Get last activity time from latest history file
		var lastActivity int64
		histDir := getHistoryDir(info.TopicID)
		if histFiles, err := filepath.Glob(filepath.Join(histDir, "*.jsonl")); err == nil && len(histFiles) > 0 {
			// Files are date-sorted by name; last one is newest
			latestFile := histFiles[len(histFiles)-1]
			if fi, err := os.Stat(latestFile); err == nil {
				lastActivity = fi.ModTime().Unix()
			}
		}

		sessions = append(sessions, APISessionInfo{
			Name:         name,
			Host:         host,
			Status:       status,
			Cwd:          info.Path,
			LastActivity: lastActivity,
		})
	}

	encoder.Encode(APIResponse{OK: true, Sessions: sessions})
}

// ensureSessionRunning ensures the tmux session exists and Claude is running
// Returns error message or empty string on success
func ensureSessionRunning(cfg *Config, sessionName string, info *SessionInfo) string {
	// Extract project name for tmux session and workdir
	_, projectName := parseSessionTarget(sessionName)
	tmuxName := tmuxSessionName(extractProjectName(projectName))
	projectPath := info.Path
	if projectPath == "" {
		projectPath = resolveProjectPath(cfg, projectName)
	}

	if info.Host != "" {
		// Remote session
		address := getHostAddress(cfg, info.Host)
		if address == "" {
			return "host not configured"
		}

		if !sshTmuxHasSession(address, tmuxName) {
			// Session doesn't exist, create it with continue flag
			if err := sshTmuxNewSession(address, tmuxName, projectPath, true); err != nil {
				// Ignore "duplicate session" error - session may have been created by another process
				if !strings.Contains(err.Error(), "duplicate session") {
					return fmt.Sprintf("failed to start session: %v", err)
				}
			} else {
				// Wait for Claude to initialize only if we actually created the session
				time.Sleep(5 * time.Second)
			}
		}
		// Check if Claude is running (regardless of whether we just created the session)
		if !isClaudeRunning(tmuxName, address) {
			// Session exists but Claude crashed, restart
			if !restartClaudeInSession(tmuxName, address) {
				return "failed to restart Claude"
			}
		}
	} else {
		// Local session
		if !tmuxSessionExists(tmuxName) {
			// Session doesn't exist, create it with continue flag
			if err := createTmuxSession(tmuxName, projectPath, true); err != nil {
				// Ignore "duplicate session" error
				if !strings.Contains(err.Error(), "duplicate session") {
					return fmt.Sprintf("failed to start session: %v", err)
				}
			} else {
				// Wait for Claude to initialize only if we actually created the session
				time.Sleep(5 * time.Second)
			}
		}
		// Check if Claude is running (regardless of whether we just created the session)
		if !isClaudeRunning(tmuxName, "") {
			// Session exists but Claude crashed, restart
			if !restartClaudeInSession(tmuxName, "") {
				return "failed to restart Claude"
			}
		}
	}

	return ""
}

// handleAskCmd handles the "ask" command (blocking)
func handleAskCmd(encoder *json.Encoder, cfg *Config, req APIRequest) {
	if req.Session == "" || req.Text == "" {
		encoder.Encode(APIResponse{OK: false, Error: "session and text required"})
		return
	}

	info, exists := cfg.Sessions[req.Session]
	if !exists || info.Deleted {
		encoder.Encode(APIResponse{OK: false, Error: "session not found"})
		return
	}

	// Ensure session is running (auto-start if needed)
	if errMsg := ensureSessionRunning(cfg, req.Session, info); errMsg != "" {
		encoder.Encode(APIResponse{OK: false, Error: errMsg})
		return
	}

	// Extract correct tmux session name
	_, projectName := parseSessionTarget(req.Session)
	tmuxName := tmuxSessionName(extractProjectName(projectName))
	startTime := time.Now()

	// Format message with agent identifier
	agentLabel := req.From
	if agentLabel == "" {
		agentLabel = "api"
	}

	// Send to Telegram topic
	if info.TopicID > 0 {
		telegramMsg := fmt.Sprintf("🤖 [%s] %s", agentLabel, req.Text)
		sendMessage(cfg, cfg.GroupID, info.TopicID, telegramMsg)
	}

	// Store in history
	msgID := nextMessageID()
	appendHistory(info.TopicID, HistoryMessage{
		ID:        msgID,
		Timestamp: time.Now().Unix(),
		From:      "api",
		Text:      req.Text,
		Agent:     agentLabel,
	})

	// Mark as sent to suppress prompt hook echo (same mechanism as Telegram dedup)
	if info.TopicID > 0 {
		markTelegramSent(info.TopicID)
	}

	// Send to tmux
	var sendErr error
	if info.Host != "" {
		address := getHostAddress(cfg, info.Host)
		sendErr = sshTmuxSendKeys(address, tmuxName, req.Text)
	} else {
		sendErr = sendToTmux(tmuxName, req.Text)
	}

	if sendErr != nil {
		encoder.Encode(APIResponse{OK: false, Error: fmt.Sprintf("failed to send: %v", sendErr)})
		return
	}

	// Wait for Claude to finish (poll state)
	sshAddr := ""
	if info.Host != "" {
		sshAddr = getHostAddress(cfg, info.Host)
	}

	// Wait for Claude to become busy (started processing)
	time.Sleep(500 * time.Millisecond)

	// Wait for Claude to become idle (finished processing)
	timeout := time.After(5 * time.Minute)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	idleCount := 0
	for {
		select {
		case <-timeout:
			encoder.Encode(APIResponse{OK: false, Error: "timeout waiting for response"})
			return
		case <-ticker.C:
			state := checkClaudeState(tmuxName, sshAddr)
			if state == "idle" {
				idleCount++
				if idleCount >= 2 {
					// Claude is idle, get last response
					response := getLastClaudeResponse(tmuxName, sshAddr, req.Text)
					duration := time.Since(startTime).Milliseconds()

					// Store Claude's response in history
					responseMsgID := nextMessageID()
					appendHistory(info.TopicID, HistoryMessage{
						ID:        responseMsgID,
						Timestamp: time.Now().Unix(),
						From:      "claude",
						Text:      response,
					})

					encoder.Encode(APIResponse{
						OK:        true,
						Response:  response,
						MessageID: responseMsgID,
						Duration:  duration,
					})
					return
				}
			} else {
				idleCount = 0
			}
		}
	}
}

// handleSendCmd handles the "send" command (non-blocking)
func handleSendCmd(encoder *json.Encoder, cfg *Config, req APIRequest) {
	if req.Session == "" || req.Text == "" {
		encoder.Encode(APIResponse{OK: false, Error: "session and text required"})
		return
	}

	info, exists := cfg.Sessions[req.Session]
	if !exists || info.Deleted {
		encoder.Encode(APIResponse{OK: false, Error: "session not found"})
		return
	}

	// Ensure session is running (auto-start if needed)
	if errMsg := ensureSessionRunning(cfg, req.Session, info); errMsg != "" {
		encoder.Encode(APIResponse{OK: false, Error: errMsg})
		return
	}

	// Extract correct tmux session name
	_, projectName := parseSessionTarget(req.Session)
	tmuxName := tmuxSessionName(extractProjectName(projectName))

	// Format message with agent identifier
	agentLabel := req.From
	if agentLabel == "" {
		agentLabel = "api"
	}

	// Send to Telegram topic
	if info.TopicID > 0 {
		telegramMsg := fmt.Sprintf("🤖 [%s] %s", agentLabel, req.Text)
		sendMessage(cfg, cfg.GroupID, info.TopicID, telegramMsg)
	}

	// Store in history
	msgID := nextMessageID()
	appendHistory(info.TopicID, HistoryMessage{
		ID:        msgID,
		Timestamp: time.Now().Unix(),
		From:      "api",
		Text:      req.Text,
		Agent:     agentLabel,
	})

	// Mark as sent to suppress prompt hook echo (same mechanism as Telegram dedup)
	if info.TopicID > 0 {
		markTelegramSent(info.TopicID)
	}

	// Send to tmux
	var sendErr error
	if info.Host != "" {
		address := getHostAddress(cfg, info.Host)
		sendErr = sshTmuxSendKeys(address, tmuxName, req.Text)
	} else {
		sendErr = sendToTmux(tmuxName, req.Text)
	}

	if sendErr != nil {
		encoder.Encode(APIResponse{OK: false, Error: fmt.Sprintf("failed to send: %v", sendErr)})
		return
	}

	encoder.Encode(APIResponse{OK: true, MessageID: msgID})

	// Start background capture for remote sessions
	if info.Host != "" {
		address := getHostAddress(cfg, info.Host)
		captureResponseAsync(req.Session, tmuxName, address, info.TopicID)
	}
}

// captureResponseAsync polls a remote session in the background to capture
// Claude's response after a message is sent. It stores the response in history.
// Only one capture runs per session at a time (guarded by activeCaptures).
func captureResponseAsync(sessionName string, tmuxName string, sshAddress string, topicID int64) {
	// Per-session guard: skip if capture already running
	if _, loaded := activeCaptures.LoadOrStore(sessionName, true); loaded {
		return
	}

	go func() {
		defer activeCaptures.Delete(sessionName)

		// Wait for Claude to start processing
		time.Sleep(1 * time.Second)

		timeout := time.After(5 * time.Minute)
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()

		idleCount := 0
		for {
			select {
			case <-timeout:
				fmt.Printf("[capture] timeout for session=%s\n", sessionName)
				return
			case <-ticker.C:
				state := checkClaudeState(tmuxName, sshAddress)
				if state == "idle" {
					idleCount++
					if idleCount >= 2 {
						// Claude is idle, capture response
						response := getLastClaudeResponse(tmuxName, sshAddress, "")
						if response == "" {
							return
						}

						// Dedup: check last claude message in history
						msgs, err := readHistory(topicID, 0, 1, "claude")
						if err == nil && len(msgs) > 0 && msgs[len(msgs)-1].Text == response {
							fmt.Printf("[capture] dedup: response already in history for session=%s\n", sessionName)
							return
						}

						appendHistory(topicID, HistoryMessage{
							ID:        nextMessageID(),
							Timestamp: time.Now().Unix(),
							From:      "claude",
							Text:      response,
						})
						fmt.Printf("[capture] stored response for session=%s (%d chars)\n", sessionName, len(response))
						return
					}
				} else {
					idleCount = 0
				}
			}
		}
	}()
}

// handleHistoryCmd handles the "history" command
func handleHistoryCmd(encoder *json.Encoder, cfg *Config, req APIRequest) {
	if req.Session == "" {
		encoder.Encode(APIResponse{OK: false, Error: "session required"})
		return
	}

	info, exists := cfg.Sessions[req.Session]
	if !exists || info.Deleted {
		encoder.Encode(APIResponse{OK: false, Error: "session not found"})
		return
	}

	messages, err := readHistory(info.TopicID, req.After, req.Limit, req.FromFilter)
	if err != nil {
		encoder.Encode(APIResponse{OK: false, Error: fmt.Sprintf("failed to read history: %v", err)})
		return
	}

	encoder.Encode(APIResponse{OK: true, Messages: messages})
}

// handleScreenshotCmd handles the "screenshot" command — returns raw tmux capture-pane
func handleScreenshotCmd(encoder *json.Encoder, cfg *Config, req APIRequest) {
	if req.Session == "" {
		encoder.Encode(APIResponse{OK: false, Error: "session required"})
		return
	}

	info, exists := cfg.Sessions[req.Session]
	if !exists || info.Deleted {
		encoder.Encode(APIResponse{OK: false, Error: "session not found"})
		return
	}

	_, projectName := parseSessionTarget(req.Session)
	tmuxName := tmuxSessionName(extractProjectName(projectName))

	var sshAddress string
	if info.Host != "" {
		sshAddress = getHostAddress(cfg, info.Host)
		if sshAddress == "" {
			encoder.Encode(APIResponse{OK: false, Error: "host not configured: " + info.Host})
			return
		}
	}

	lines := req.Limit
	if lines <= 0 {
		lines = 50
	}

	content, err := captureTmuxPane(tmuxName, sshAddress, lines)
	if err != nil {
		encoder.Encode(APIResponse{OK: false, Error: fmt.Sprintf("capture failed: %v", err)})
		return
	}

	encoder.Encode(APIResponse{OK: true, Response: content})
}

// handleSubscribeCmd handles the "subscribe" command
func handleSubscribeCmd(conn net.Conn, encoder *json.Encoder, cfg *Config, req APIRequest) {
	// For now, implement basic subscription that sends events for specified sessions
	sessions := req.Sessions
	if len(sessions) == 0 {
		// Subscribe to all sessions
		for name, info := range cfg.Sessions {
			if !info.Deleted {
				sessions = append(sessions, name)
			}
		}
	}

	// Send subscribed confirmation
	encoder.Encode(APIEvent{Event: "subscribed", Session: strings.Join(sessions, ",")})

	// Keep connection open and poll for state changes
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	lastStatus := make(map[string]string)

	for {
		select {
		case <-ticker.C:
			for _, sessionName := range sessions {
				info, exists := cfg.Sessions[sessionName]
				if !exists || info.Deleted {
					continue
				}

				tmuxName := tmuxSessionName(sessionName)
				sshAddr := ""
				if info.Host != "" {
					sshAddr = getHostAddress(cfg, info.Host)
				}

				var status string
				if info.Host != "" && sshAddr != "" {
					if sshTmuxHasSession(sshAddr, tmuxName) {
						if checkClaudeState(tmuxName, sshAddr) == "busy" {
							status = "active"
						} else {
							status = "idle"
						}
					} else {
						status = "stopped"
					}
				} else if tmuxSessionExists(tmuxName) {
					if checkClaudeState(tmuxName, "") == "busy" {
						status = "active"
					} else {
						status = "idle"
					}
				} else {
					status = "stopped"
				}

				if lastStatus[sessionName] != status {
					lastStatus[sessionName] = status
					if err := encoder.Encode(APIEvent{Event: "status", Session: sessionName, Status: status}); err != nil {
						return // Connection closed
					}
				}
			}
		}
	}
}

// captureTmuxPane captures the last N lines from a tmux pane
func captureTmuxPane(tmuxName string, sshAddress string, lines int) (string, error) {
	linesArg := fmt.Sprintf("-%d", lines)

	if sshAddress != "" {
		cmd := fmt.Sprintf("tmux capture-pane -t %s -p -S %s", shellQuote(tmuxName), linesArg)
		result, err := runSSH(sshAddress, cmd, 10*time.Second)
		if err != nil {
			return "", fmt.Errorf("failed to capture pane: %w", err)
		}
		return strings.TrimRight(result, "\n"), nil
	}

	cmd := tmuxCmd("capture-pane", "-t", tmuxName, "-p", "-S", linesArg)
	result, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to capture pane: %w", err)
	}
	return strings.TrimRight(string(result), "\n"), nil
}

// truncateRepeatingChars compresses runs of repeated characters (>10) to char(count) format
func truncateRepeatingChars(s string) string {
	if len(s) == 0 {
		return s
	}

	var result strings.Builder
	runes := []rune(s)
	i := 0

	for i < len(runes) {
		char := runes[i]
		count := 1

		// Count consecutive occurrences
		for i+count < len(runes) && runes[i+count] == char {
			count++
		}

		if count > 10 {
			// Truncate: keep 10 chars, show total count in brackets
			for j := 0; j < 10; j++ {
				result.WriteRune(char)
			}
			result.WriteString(fmt.Sprintf("(%d)", count))
		} else {
			// Keep as is
			for j := 0; j < count; j++ {
				result.WriteRune(char)
			}
		}

		i += count
	}

	return result.String()
}

// truncateRepeatingCharsInLines applies truncateRepeatingChars to each line
func truncateRepeatingCharsInLines(s string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = truncateRepeatingChars(line)
	}
	return strings.Join(lines, "\n")
}

// getLastClaudeResponse captures the last response from Claude in tmux.
// sentText is the message that was sent — used to skip echo of our own message.
// It tries up to 3 times with increasing capture window if the result is empty,
// since Claude Code's terminal UI may overwrite the response with spinners/prompts.
func getLastClaudeResponse(tmuxName string, sshAddress string, sentText string) string {
	// Try with increasing capture sizes; retry if result is empty
	captureSizes := []int{200, 500, 500}
	for attempt, captureSize := range captureSizes {
		if attempt > 0 {
			fmt.Printf("[getLastClaudeResponse] retry #%d (capture -S -%d)\n", attempt, captureSize)
			time.Sleep(2 * time.Second)
		}

		result := captureClaudeResponse(tmuxName, sshAddress, captureSize, sentText)
		if result != "" {
			return result
		}
	}
	fmt.Printf("[getLastClaudeResponse] all retries exhausted, returning empty\n")
	return ""
}

// captureClaudeResponse does a single capture-pane and parses Claude's response.
// sentText is used to detect and skip echo of the sent message in the capture.
func captureClaudeResponse(tmuxName string, sshAddress string, captureLines int, sentText string) string {
	var output string

	if sshAddress != "" {
		cmd := fmt.Sprintf("tmux capture-pane -t %s -p -S -%d", shellQuote(tmuxName), captureLines)
		result, err := runSSH(sshAddress, cmd, 10*time.Second)
		if err != nil {
			return ""
		}
		output = result
	} else {
		cmd := tmuxCmd("capture-pane", "-t", tmuxName, "-p", "-S", fmt.Sprintf("-%d", captureLines))
		result, err := cmd.Output()
		if err != nil {
			return ""
		}
		output = string(result)
	}

	// Debug: log raw capture-pane output before any filtering
	fmt.Printf("[getLastClaudeResponse] raw capture-pane (%d bytes, %d lines, -S -%d):\n---RAW START---\n%s\n---RAW END---\n",
		len(output), len(strings.Split(output, "\n")), captureLines, output)

	// Parse output to find Claude's response
	// Look for content after the prompt marker (❯) and before the next prompt
	lines := strings.Split(output, "\n")
	var responseLines []string
	inResponse := false

	for i := len(lines) - 1; i >= 0; i-- {
		line := lines[i]

		// Found prompt - start collecting response going backwards
		if strings.Contains(line, "❯") && !inResponse {
			inResponse = true
			continue
		}

		// Found previous prompt - stop
		if strings.Contains(line, "❯") && inResponse {
			break
		}

		if inResponse && strings.TrimSpace(line) != "" {
			if isClaudeUIArtifact(line) {
				fmt.Printf("[getLastClaudeResponse] FILTERED: %q\n", line)
				continue
			}
			// Strip Claude Code UI bullet prefix (● ) from response text
			cleaned := line
			trimmedLine := strings.TrimSpace(line)
			if strings.HasPrefix(trimmedLine, "● ") {
				cleaned = strings.TrimPrefix(trimmedLine, "● ")
			}
			fmt.Printf("[getLastClaudeResponse] KEPT: %q -> %q\n", line, cleaned)
			responseLines = append([]string{cleaned}, responseLines...)
		}
	}

	result := strings.TrimSpace(strings.Join(responseLines, "\n"))

	// Skip if result is echo of the sent message (or its tail due to tmux line wrapping)
	if sentText != "" && result != "" {
		sentNorm := strings.TrimSpace(sentText)
		if result == sentNorm || strings.HasSuffix(sentNorm, result) || strings.HasSuffix(result, sentNorm) {
			fmt.Printf("[getLastClaudeResponse] echo detected, skipping: %q\n", result)
			return ""
		}
	}

	fmt.Printf("[getLastClaudeResponse] final result (%d bytes): %q\n", len(result), result)
	return result
}

// isClaudeUIArtifact returns true if a line is a Claude Code terminal UI element
// (spinners, separators, tool markers, status bars) rather than actual response text.
func isClaudeUIArtifact(line string) bool {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return true
	}

	// Separator lines (──────)
	if strings.TrimLeft(trimmed, "─") == "" {
		return true
	}

	// Tool use markers: ● Bash(...), ● Edit(...), ● Read(...), etc.
	// Also ● Searched for N patterns..., ● Wrote to file...
	// Only match tool patterns — bare ● prefix is also used for Claude's text response.
	if strings.HasPrefix(trimmed, "●") {
		rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "●"))
		// Tool calls: ToolName( or ToolName:
		for _, r := range rest {
			if r == '(' || r == ':' {
				return true
			}
			if r == ' ' || r < 'A' || (r > 'Z' && r < 'a') || r > 'z' {
				break
			}
		}
		// Tool summary lines: "Searched for N ...", "Wrote to ...", "Read N lines ..."
		if strings.HasPrefix(rest, "Searched ") || strings.HasPrefix(rest, "Wrote ") ||
			strings.HasPrefix(rest, "Read ") {
			return true
		}
	}

	// Nested tool output: ⎿ Added 16 lines..., ⎿ (No content), ⎿ (timeout 15s)
	if strings.HasPrefix(trimmed, "⎿") {
		return true
	}

	// Indented tool continuation lines (output under ⎿)
	if strings.HasPrefix(line, "     ") && !strings.HasPrefix(trimmed, "●") {
		// Lines deeply indented (5+ spaces) are typically tool output continuation
		// Only skip if not a ● prefixed line (which could be Claude's response)
		if strings.Contains(trimmed, "(ctrl+o to expand)") || strings.HasPrefix(trimmed, "…") ||
			strings.HasPrefix(trimmed, "… +") {
			return true
		}
	}

	// Spinners and thinking indicators: ✶ ✢ ✽ ✻ · * (both Unicode and ASCII variants)
	// Claude Code uses both Unicode (✶ Thinking…) and ASCII (* Cogitating…) formats
	if strings.HasPrefix(trimmed, "✶") || strings.HasPrefix(trimmed, "✢") ||
		strings.HasPrefix(trimmed, "✽") || strings.HasPrefix(trimmed, "✻") ||
		strings.HasPrefix(trimmed, "·") || strings.HasPrefix(trimmed, "* ") {
		return true
	}

	// Status bar
	if strings.HasPrefix(trimmed, "⏵") {
		return true
	}

	// Thinking statistics: "Cogitated for 2m", "Brewed for 1m 19s", "Cooked for 30s", etc.
	if strings.Contains(trimmed, " for ") && (strings.HasSuffix(trimmed, "s") || strings.HasSuffix(trimmed, "m")) &&
		(strings.HasPrefix(trimmed, "Cogitated") || strings.HasPrefix(trimmed, "Brewed") ||
			strings.HasPrefix(trimmed, "Cooked") || strings.HasPrefix(trimmed, "Churned") ||
			strings.HasPrefix(trimmed, "Marinated") || strings.HasPrefix(trimmed, "Cultivated")) {
		return true
	}

	// Activity/permission indicators
	if strings.Contains(line, "ctrl+c to interrupt") ||
		strings.Contains(line, "bypass permissions") {
		return true
	}

	// Collapsed output indicator: … +56 lines (ctrl+o to expand)
	// Also: Searched for N patterns (ctrl+o to expand)
	if strings.Contains(trimmed, "(ctrl+o to expand)") {
		return true
	}

	// Timeout annotations: (timeout 5m), (timeout 15s)
	if strings.HasPrefix(trimmed, "(timeout ") {
		return true
	}

	// Tool output annotations: (No content)
	if trimmed == "(No content)" {
		return true
	}

	return false
}

// Config function wrappers - delegate to config package
func getConfigPath() string                           { return config.Path() }
func loadOrCreateConfig() (*Config, error)            { return config.LoadOrCreate() }
func loadConfig() (*Config, error)                    { return config.Load() }
func saveConfig(cfg *Config) error                    { return config.Save(cfg) }
func getProjectsDir(cfg *Config) string               { return config.GetProjectsDir(cfg) }
func resolveProjectPath(cfg *Config, name string) string { return config.ResolveProjectPath(cfg, name) }

// Telegram API helpers

func telegramAPI(config *Config, method string, params url.Values) (*TelegramResponse, error) {
	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/%s", config.BotToken, method)
	resp, err := http.PostForm(apiURL, params)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var result TelegramResponse
	json.Unmarshal(body, &result)
	return &result, nil
}

func sendMessage(config *Config, chatID int64, threadID int64, text string) error {
	const maxLen = 4000

	// Split long messages
	messages := splitMessage(text, maxLen)

	for _, msg := range messages {
		params := url.Values{
			"chat_id": {fmt.Sprintf("%d", chatID)},
			"text":    {msg},
		}
		if threadID > 0 {
			params.Set("message_thread_id", fmt.Sprintf("%d", threadID))
		}

		result, err := telegramAPI(config, "sendMessage", params)
		if err != nil {
			return err
		}
		if !result.OK {
			return fmt.Errorf("telegram error: %s", result.Description)
		}

		// Small delay between messages to maintain order
		if len(messages) > 1 {
			time.Sleep(100 * time.Millisecond)
		}
	}
	return nil
}

// InlineKeyboardButton represents a Telegram inline keyboard button
type InlineKeyboardButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data"`
}

func sendMessageWithKeyboard(config *Config, chatID int64, threadID int64, text string, buttons [][]InlineKeyboardButton) error {
	keyboard := map[string]interface{}{
		"inline_keyboard": buttons,
	}
	keyboardJSON, _ := json.Marshal(keyboard)

	params := url.Values{
		"chat_id":      {fmt.Sprintf("%d", chatID)},
		"text":         {text},
		"reply_markup": {string(keyboardJSON)},
	}
	if threadID > 0 {
		params.Set("message_thread_id", fmt.Sprintf("%d", threadID))
	}

	result, err := telegramAPI(config, "sendMessage", params)
	if err != nil {
		return err
	}
	if !result.OK {
		return fmt.Errorf("telegram error: %s", result.Description)
	}
	return nil
}

func answerCallbackQuery(config *Config, callbackID string) {
	params := url.Values{
		"callback_query_id": {callbackID},
	}
	telegramAPI(config, "answerCallbackQuery", params)
}

func editMessageRemoveKeyboard(config *Config, chatID int64, messageID int, newText string) {
	params := url.Values{
		"chat_id":    {fmt.Sprintf("%d", chatID)},
		"message_id": {fmt.Sprintf("%d", messageID)},
		"text":       {newText},
	}
	telegramAPI(config, "editMessageText", params)
}

func sendTypingAction(config *Config, chatID int64, threadID int64) {
	params := url.Values{
		"chat_id": {fmt.Sprintf("%d", chatID)},
		"action":  {"typing"},
	}
	if threadID > 0 {
		params.Set("message_thread_id", fmt.Sprintf("%d", threadID))
	}
	telegramAPI(config, "sendChatAction", params)
}

// Continuous typing indicator management
var (
	typingCancelers = make(map[string]context.CancelFunc)
	typingMu        sync.Mutex
)

// checkClaudeState checks if Claude is busy or idle in a tmux session
// Returns: "busy", "idle", or "unknown"
// sshAddress is empty for local sessions, or SSH address for remote sessions
func checkClaudeState(tmuxName string, sshAddress string) string {
	var content string
	var err error

	if sshAddress != "" {
		// Remote session - use SSH
		cmd := fmt.Sprintf("tmux capture-pane -t %s -p -S -15", shellQuote(tmuxName))
		content, err = runSSH(sshAddress, cmd, 5*time.Second)
	} else {
		// Local session
		cmd := tmuxCmd("capture-pane", "-t", tmuxName, "-p", "-S", "-15")
		var output []byte
		output, err = cmd.Output()
		content = string(output)
	}

	if err != nil {
		return "unknown"
	}

	// Parse lines and find the prompt position
	lines := strings.Split(content, "\n")

	// Find the last occurrence of the input prompt ❯
	promptLineIdx := -1
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "❯" || line == "> " {
			promptLineIdx = i
			break
		}
	}

	// If no prompt found, state is unknown
	if promptLineIdx == -1 {
		return "unknown"
	}

	// Check lines AFTER the prompt for activity indicators
	// If "ctrl+c to interrupt" or spinners appear AFTER the prompt, Claude is busy
	for i := promptLineIdx + 1; i < len(lines); i++ {
		line := lines[i]
		// Skip separator lines and status bar
		if strings.HasPrefix(strings.TrimSpace(line), "─") || strings.Contains(line, "bypass permissions") {
			continue
		}
		// Activity indicators after prompt mean busy
		if strings.Contains(line, "ctrl+c to interrupt") {
			return "busy"
		}
		if strings.Contains(line, "✽") || strings.Contains(line, "✻") {
			return "busy"
		}
		if strings.Contains(line, "Running…") || strings.Contains(line, "Thinking…") {
			return "busy"
		}
	}

	// Check lines BEFORE the prompt - if recent activity indicator, might still be transitioning
	// Look only at the 3 lines before prompt
	startCheck := promptLineIdx - 3
	if startCheck < 0 {
		startCheck = 0
	}
	for i := startCheck; i < promptLineIdx; i++ {
		line := lines[i]
		// If there's an active spinner line right before prompt, still processing
		if strings.Contains(line, "ctrl+c to interrupt") {
			// This is historical - Claude finished. Check if prompt is truly last
			break
		}
	}

	// Prompt found and no activity after it - Claude is idle
	return "idle"
}

// isClaudeRunning checks if Claude Code is running in a tmux session
// Returns true if Claude UI elements are detected, false if it looks like plain bash
func isClaudeRunning(tmuxName string, sshAddress string) bool {
	var content string
	var err error

	if sshAddress != "" {
		// Remote session - use SSH
		cmd := fmt.Sprintf("tmux capture-pane -t %s -p -S -30", shellQuote(tmuxName))
		content, err = runSSH(sshAddress, cmd, 5*time.Second)
	} else {
		// Local session
		cmd := tmuxCmd("capture-pane", "-t", tmuxName, "-p", "-S", "-30")
		var output []byte
		output, err = cmd.Output()
		content = string(output)
	}

	if err != nil {
		return false
	}

	// Claude Code UI has distinctive elements:
	// - Input prompt: ❯
	// - Status bar: "bypass permissions" or "shift+tab to cycle"
	// - Activity indicators: ●, ✽, ✻
	// - Tool output markers: ⎿
	// If none of these are present, Claude is probably not running

	claudeIndicators := []string{
		"❯",                    // Input prompt
		"bypass permissions",   // Status bar
		"shift+tab to cycle",   // Status bar variant
		"ctrl+c to interrupt",  // Activity indicator
		"●",                    // Tool marker
		"✽",                    // Spinner
		"✻",                    // Spinner variant
		"⎿",                    // Tool output
	}

	for _, indicator := range claudeIndicators {
		if strings.Contains(content, indicator) {
			return true
		}
	}

	return false
}

// restartClaudeInSession restarts Claude Code in an existing tmux session where it crashed
// Returns true if restart was successful
func restartClaudeInSession(tmuxName string, sshAddress string) bool {
	restartCmd := cccPath + " run -c"

	if sshAddress != "" {
		// Remote session - send command via SSH
		cmd := fmt.Sprintf("tmux send-keys -t %s %s C-m", shellQuote(tmuxName), shellQuote(restartCmd))
		_, err := runSSH(sshAddress, cmd, 10*time.Second)
		if err != nil {
			return false
		}
	} else {
		// Local session - send command directly
		cmd := tmuxCmd("send-keys", "-t", tmuxName, restartCmd, "C-m")
		if err := cmd.Run(); err != nil {
			return false
		}
	}

	// Wait for Claude to start
	time.Sleep(3 * time.Second)

	// Verify Claude is now running
	return isClaudeRunning(tmuxName, sshAddress)
}

// startContinuousTyping starts sending typing indicator every 4 seconds
// until stopContinuousTyping is called or Claude becomes idle
func startContinuousTyping(cfg *Config, chatID, threadID int64, sessionName string) {
	fmt.Fprintf(os.Stderr, "[typing] START session=%s\n", sessionName)
	typingMu.Lock()
	// Cancel existing typing for this session
	if cancel, ok := typingCancelers[sessionName]; ok {
		cancel()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute) // Max 10 min
	typingCancelers[sessionName] = cancel
	typingMu.Unlock()

	// Determine tmux session name and SSH address (if remote)
	var tmuxName string
	var sshAddress string

	if idx := strings.Index(sessionName, ":"); idx != -1 {
		// Remote session (host:name format)
		hostName := sessionName[:idx]
		projectName := sessionName[idx+1:]
		tmuxName = tmuxSessionName(projectName)
		sshAddress = getHostAddress(cfg, hostName)
	} else {
		// Local session
		tmuxName = tmuxSessionName(sessionName)
		sshAddress = ""
	}

	go func() {
		typingTicker := time.NewTicker(4 * time.Second)
		stateTicker := time.NewTicker(2 * time.Second)
		defer typingTicker.Stop()
		defer stateTicker.Stop()

		// Send initial typing
		sendTypingAction(cfg, chatID, threadID)

		// Track consecutive idle checks to avoid false positives
		idleCount := 0

		for {
			select {
			case <-ctx.Done():
				return
			case <-stateTicker.C:
				// Check Claude state
				state := checkClaudeState(tmuxName, sshAddress)
				if state == "idle" {
					idleCount++
					// Require 2 consecutive idle checks to confirm
					if idleCount >= 2 {
						fmt.Fprintf(os.Stderr, "[typing] %s: Claude idle, stopping typing indicator\n", sessionName)
						stopContinuousTyping(sessionName)
						return
					}
				} else if state == "busy" {
					idleCount = 0
				}
				// On "unknown" state, don't reset counter (might be transient)
			case <-typingTicker.C:
				sendTypingAction(cfg, chatID, threadID)
			}
		}
	}()
}

// stopContinuousTyping stops the typing indicator for a session
func stopContinuousTyping(sessionName string) {
	typingMu.Lock()
	defer typingMu.Unlock()
	if cancel, ok := typingCancelers[sessionName]; ok {
		cancel()
		delete(typingCancelers, sessionName)
	}
}

func splitMessage(text string, maxLen int) []string {
	if len(text) <= maxLen {
		return []string{text}
	}

	var messages []string
	remaining := text

	for len(remaining) > 0 {
		if len(remaining) <= maxLen {
			messages = append(messages, remaining)
			break
		}

		// Find a good split point (newline or space)
		splitAt := maxLen

		// Try to split at a newline first
		if idx := strings.LastIndex(remaining[:maxLen], "\n"); idx > maxLen/2 {
			splitAt = idx + 1
		} else if idx := strings.LastIndex(remaining[:maxLen], " "); idx > maxLen/2 {
			// Fall back to space
			splitAt = idx + 1
		}

		messages = append(messages, strings.TrimRight(remaining[:splitAt], " \n"))
		remaining = remaining[splitAt:]
	}

	return messages
}

// Download file from Telegram
func downloadTelegramFile(config *Config, fileID string, destPath string) error {
	// Get file path from Telegram
	resp, err := http.Get(fmt.Sprintf("https://api.telegram.org/bot%s/getFile?file_id=%s", config.BotToken, fileID))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var result struct {
		OK     bool `json:"ok"`
		Result struct {
			FilePath string `json:"file_path"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return err
	}
	if !result.OK {
		return fmt.Errorf("failed to get file path")
	}

	// Download the file
	fileURL := fmt.Sprintf("https://api.telegram.org/file/bot%s/%s", config.BotToken, result.Result.FilePath)
	fileResp, err := http.Get(fileURL)
	if err != nil {
		return err
	}
	defer fileResp.Body.Close()

	out, err := os.Create(destPath)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, fileResp.Body)
	return err
}

// Transcribe audio file using configured command or fallback to whisper
func transcribeAudio(config *Config, audioPath string) (string, error) {
	// Use configured transcription command if set
	if config.TranscriptionCmd != "" {
		cmdPath := expandPath(config.TranscriptionCmd)
		cmd := exec.Command(cmdPath, audioPath)
		output, err := cmd.Output()
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				return "", fmt.Errorf("%s: %s", err, string(exitErr.Stderr))
			}
			return "", err
		}
		return strings.TrimSpace(string(output)), nil
	}

	// Fallback: try to find whisper in PATH or known locations
	whisperPath := "whisper"
	if _, err := exec.LookPath("whisper"); err != nil {
		// Try common locations
		for _, p := range []string{"/opt/homebrew/bin/whisper", "/usr/local/bin/whisper"} {
			if _, err := os.Stat(p); err == nil {
				whisperPath = p
				break
			}
		}
	}

	cmd := exec.Command(whisperPath, audioPath, "--model", "small", "--output_format", "txt", "--output_dir", filepath.Dir(audioPath))
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("whisper failed: %w (set transcription_cmd in config for custom transcription)", err)
	}

	// Read the transcription
	txtPath := strings.TrimSuffix(audioPath, filepath.Ext(audioPath)) + ".txt"
	content, err := os.ReadFile(txtPath)
	if err != nil {
		return "", err
	}

	// Cleanup
	os.Remove(txtPath)

	return strings.TrimSpace(string(content)), nil
}

// expandPath expands ~ to home directory
func expandPath(path string) string { return config.ExpandPath(path) }

// SSH utilities for remote host operations

const (
	sshConnectTimeout = 5  // seconds
	sshCommandTimeout = 10 // seconds
)

// runSSH executes a command on a remote host via SSH
func runSSH(address string, command string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// Wrap command in interactive login shell for full environment (nvm, etc.)
	wrappedCmd := fmt.Sprintf("bash -i -l -c %s", shellQuote(command))

	cmd := exec.CommandContext(ctx, "ssh",
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", fmt.Sprintf("ConnectTimeout=%d", sshConnectTimeout),
		address,
		wrappedCmd,
	)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("timeout after %v", timeout)
	}
	if err != nil {
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg != "" {
			return "", fmt.Errorf("%s: %s", err, errMsg)
		}
		return "", err
	}

	return strings.TrimSpace(stdout.String()), nil
}

// scpToHost copies a file to a remote host via scp
func scpToHost(address string, localPath string, remotePath string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "scp",
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", fmt.Sprintf("ConnectTimeout=%d", sshConnectTimeout),
		localPath,
		address+":"+remotePath,
	)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("timeout after %v", timeout)
	}
	if err != nil {
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg != "" {
			return fmt.Errorf("%s: %s", err, errMsg)
		}
		return err
	}

	return nil
}

// shellQuote quotes a string for safe shell usage
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

// sshCheckConnection verifies SSH connection to a host
func sshCheckConnection(address string) error {
	_, err := runSSH(address, "echo ok", time.Duration(sshConnectTimeout+2)*time.Second)
	return err
}

// sshCheckCommand checks if a command exists on remote host, returns path
func sshCheckCommand(address string, cmdName string) (string, error) {
	return runSSH(address, "which "+cmdName, time.Duration(sshCommandTimeout)*time.Second)
}

// sshResolvePath resolves a path on remote host (expands ~, gets absolute path)
func sshResolvePath(address string, path string) (string, error) {
	// Use eval to expand ~ and readlink to get absolute path
	cmd := fmt.Sprintf("eval echo %s | xargs readlink -f 2>/dev/null || eval echo %s", path, path)
	return runSSH(address, cmd, time.Duration(sshCommandTimeout)*time.Second)
}

// sshMkdir creates a directory on remote host
func sshMkdir(address string, path string) error {
	// Use eval to expand ~ in path
	cmd := fmt.Sprintf("mkdir -p \"$(eval echo %s)\"", shellQuote(path))
	_, err := runSSH(address, cmd, time.Duration(sshCommandTimeout)*time.Second)
	return err
}

// sshDirExists checks if a directory exists on remote host
func sshDirExists(address string, path string) bool {
	// Use eval to expand ~ in path
	cmd := fmt.Sprintf("test -d \"$(eval echo %s)\"", shellQuote(path))
	_, err := runSSH(address, cmd, time.Duration(sshCommandTimeout)*time.Second)
	return err == nil
}

// sshTmuxHasSession checks if a tmux session exists on remote host
func sshTmuxHasSession(address string, sessionName string) bool {
	_, err := runSSH(address, "tmux has-session -t "+shellQuote(sessionName), time.Duration(sshCommandTimeout)*time.Second)
	return err == nil
}

// sshTmuxNewSession creates a new tmux session on remote host
func sshTmuxNewSession(address string, name string, workDir string, continueSession bool) error {
	// Create session
	cmd := fmt.Sprintf("tmux new-session -d -s %s -c %s", shellQuote(name), shellQuote(workDir))
	if _, err := runSSH(address, cmd, time.Duration(sshCommandTimeout)*time.Second); err != nil {
		return fmt.Errorf("failed to create tmux session: %w", err)
	}

	// Enable mouse mode
	runSSH(address, fmt.Sprintf("tmux set-option -t %s mouse on", shellQuote(name)), time.Duration(sshCommandTimeout)*time.Second)

	// Start claude
	claudeCmd := "claude --dangerously-skip-permissions"
	if continueSession {
		claudeCmd += " -c"
	}
	sendCmd := fmt.Sprintf("tmux send-keys -t %s %s C-m", shellQuote(name), shellQuote(claudeCmd))
	if _, err := runSSH(address, sendCmd, time.Duration(sshCommandTimeout)*time.Second); err != nil {
		return err
	}

	// Confirm bypass permissions prompt (first run only)
	// Default is "No, exit" so we need Down arrow to select "Yes", then Enter
	// Down is safer than Up - if no prompt exists, Down does nothing
	time.Sleep(2 * time.Second)
	// Send Down arrow first
	downCmd := fmt.Sprintf("tmux send-keys -t %s Down", shellQuote(name))
	runSSH(address, downCmd, time.Duration(sshCommandTimeout)*time.Second)
	time.Sleep(100 * time.Millisecond)
	// Then send Enter
	enterCmd := fmt.Sprintf("tmux send-keys -t %s Enter", shellQuote(name))
	runSSH(address, enterCmd, time.Duration(sshCommandTimeout)*time.Second)

	return nil
}

// sshTmuxSendKeys sends text to a tmux session on remote host using Base64
func sshTmuxSendKeys(address string, sessionName string, text string) error {
	// Encode text as Base64 to avoid escaping issues
	encoded := base64.StdEncoding.EncodeToString([]byte(text))

	// Decode on remote and send to tmux
	cmd := fmt.Sprintf(
		"echo %s | base64 -d | xargs -0 tmux send-keys -t %s -l",
		encoded, shellQuote(sessionName),
	)
	if _, err := runSSH(address, cmd, time.Duration(sshCommandTimeout)*time.Second); err != nil {
		return err
	}

	// Always wait 2 seconds before Enter to ensure text is fully processed
	// Without this delay, Enter may be interpreted as newline instead of submit
	time.Sleep(2 * time.Second)

	// Send Enter twice (Claude needs double Enter)
	enterCmd := fmt.Sprintf(
		"tmux send-keys -t %s C-m && sleep 0.05 && tmux send-keys -t %s C-m",
		shellQuote(sessionName), shellQuote(sessionName),
	)
	_, err := runSSH(address, enterCmd, time.Duration(sshCommandTimeout)*time.Second)
	return err
}

// sshTmuxKillSession kills a tmux session on remote host
func sshTmuxKillSession(address string, sessionName string) error {
	_, err := runSSH(address, "tmux kill-session -t "+shellQuote(sessionName), time.Duration(sshCommandTimeout)*time.Second)
	return err
}

// sshRunCommand executes an arbitrary command on remote host (for /rc)
func sshRunCommand(address string, command string, timeout time.Duration) (string, error) {
	return runSSH(address, command, timeout)
}

// Session name parsing utilities

// parseSessionTarget parses "host:name" or "name" format
// Returns (host, name) where host is empty for local sessions
func parseSessionTarget(input string) (host string, name string) {
	// Check for host:name format
	// But be careful: ~/path and /path are not host prefixes
	if strings.HasPrefix(input, "~/") || strings.HasPrefix(input, "/") {
		return "", input
	}

	idx := strings.Index(input, ":")
	if idx > 0 {
		host = input[:idx]
		name = input[idx+1:]
		return host, name
	}

	return "", input
}

// fullSessionName creates full session name from host and name
func fullSessionName(host string, name string) string {
	if host == "" {
		return name
	}
	return host + ":" + name
}

// getHostAddress returns SSH address for a host, or empty if local/not found
func getHostAddress(cfg *Config, hostName string) string { return config.GetHostAddress(cfg, hostName) }

// getHostProjectsDir returns projects dir for a host
func getHostProjectsDir(cfg *Config, hostName string) string { return config.GetHostProjectsDir(cfg, hostName) }

// resolveSessionPath resolves project path for a session
// For local: uses config.ProjectsDir
// For remote: uses host's projects_dir and resolves via SSH
func resolveSessionPath(config *Config, hostName string, nameOrPath string) (string, error) {
	// Check if it's already an absolute or home-relative path
	if strings.HasPrefix(nameOrPath, "/") || strings.HasPrefix(nameOrPath, "~/") {
		if hostName == "" {
			// Local: expand ~ and return
			return expandPath(nameOrPath), nil
		}
		// Remote: resolve via SSH
		address := getHostAddress(config, hostName)
		if address == "" {
			return "", fmt.Errorf("host '%s' not found", hostName)
		}
		return sshResolvePath(address, nameOrPath)
	}

	// Relative name - use projects_dir
	projectsDir := getHostProjectsDir(config, hostName)
	fullPath := filepath.Join(projectsDir, nameOrPath)

	if hostName == "" {
		// Local
		return expandPath(fullPath), nil
	}

	// Remote: resolve via SSH
	address := getHostAddress(config, hostName)
	if address == "" {
		return "", fmt.Errorf("host '%s' not found", hostName)
	}
	return sshResolvePath(address, fullPath)
}

// extractProjectName extracts project name from path
func extractProjectName(path string) string {
	return filepath.Base(path)
}

// tmuxSessionName returns a safe tmux session name for a project
// Replaces dots with underscores because tmux 3.5+ interprets dots as window/pane separators
func tmuxSessionName(name string) string {
	safeName := strings.ReplaceAll(name, ".", "_")
	return "claude-" + safeName
}

func createForumTopic(config *Config, name string) (int64, error) {
	if config.GroupID == 0 {
		return 0, fmt.Errorf("no group configured. Add bot to a group with topics enabled and run: ccc setgroup")
	}

	params := url.Values{
		"chat_id": {fmt.Sprintf("%d", config.GroupID)},
		"name":    {name},
	}

	result, err := telegramAPI(config, "createForumTopic", params)
	if err != nil {
		return 0, err
	}
	if !result.OK {
		return 0, fmt.Errorf("failed to create topic: %s", result.Description)
	}

	var topic TopicResult
	if err := json.Unmarshal(result.Result, &topic); err != nil {
		return 0, fmt.Errorf("failed to parse topic result: %w", err)
	}

	return topic.MessageThreadID, nil
}

// editForumTopic renames a topic and verifies it exists
func editForumTopic(config *Config, topicID int64, name string) error {
	if config.GroupID == 0 {
		return fmt.Errorf("no group configured")
	}

	params := url.Values{
		"chat_id":           {fmt.Sprintf("%d", config.GroupID)},
		"message_thread_id": {fmt.Sprintf("%d", topicID)},
		"name":              {name},
	}

	result, err := telegramAPI(config, "editForumTopic", params)
	if err != nil {
		return err
	}
	if !result.OK {
		return fmt.Errorf("failed to edit topic: %s", result.Description)
	}

	return nil
}

// deleteForumTopic deletes a topic
func deleteForumTopic(config *Config, topicID int64) error {
	if config.GroupID == 0 {
		return fmt.Errorf("no group configured")
	}

	params := url.Values{
		"chat_id":           {fmt.Sprintf("%d", config.GroupID)},
		"message_thread_id": {fmt.Sprintf("%d", topicID)},
	}

	result, err := telegramAPI(config, "deleteForumTopic", params)
	if err != nil {
		return err
	}
	if !result.OK {
		return fmt.Errorf("failed to delete topic: %s", result.Description)
	}

	return nil
}

// getOrCreateTopic finds existing topic or creates new one
// Also syncs topic name and updates path if changed
func getOrCreateTopic(config *Config, fullName string, path string, host string) (int64, error) {
	// Check if session exists in config (including deleted)
	if info, exists := config.Sessions[fullName]; exists {
		// Try to rename topic to verify it exists and sync name
		err := editForumTopic(config, info.TopicID, fullName)
		if err != nil {
			errStr := err.Error()
			// Check if error indicates topic doesn't exist vs just "not modified"
			if strings.Contains(errStr, "not found") || strings.Contains(errStr, "TOPIC_CLOSED") ||
				strings.Contains(errStr, "TOPIC_DELETED") || strings.Contains(errStr, "invalid") {
				// Topic was deleted by user, create new one
				fmt.Fprintf(os.Stderr, "Topic %d gone, creating new: %v\n", info.TopicID, err)
				topicID, err := createForumTopic(config, fullName)
				if err != nil {
					return 0, err
				}
				info.TopicID = topicID
			}
			// Otherwise (e.g., "not modified"), topic exists - just continue
		}
		// Update path and undelete
		info.Path = path
		info.Deleted = false
		saveConfig(config)
		return info.TopicID, nil
	}

	// Create new topic
	topicID, err := createForumTopic(config, fullName)
	if err != nil {
		return 0, err
	}

	// Save to config
	config.Sessions[fullName] = &SessionInfo{
		TopicID: topicID,
		Path:    path,
		Host:    host,
		Deleted: false,
	}
	saveConfig(config)

	return topicID, nil
}

// Tmux session management

var (
	tmuxSocket string
	tmuxPath   string
	cccPath    string
	claudePath string
)

func init() {
	// Find tmux socket path using current UID
	// macOS uses /private/tmp, Linux uses /tmp
	uid := os.Getuid()
	macOSSocket := fmt.Sprintf("/private/tmp/tmux-%d/default", uid)
	linuxSocket := fmt.Sprintf("/tmp/tmux-%d/default", uid)

	// Check which socket exists, prefer Linux path first (more common in headless)
	if _, err := os.Stat(linuxSocket); err == nil {
		tmuxSocket = linuxSocket
	} else if _, err := os.Stat(macOSSocket); err == nil {
		tmuxSocket = macOSSocket
	} else {
		// Default based on OS
		if _, err := os.Stat("/private"); err == nil {
			tmuxSocket = macOSSocket
		} else {
			tmuxSocket = linuxSocket
		}
	}

	// Find tmux binary
	if path, err := exec.LookPath("tmux"); err == nil {
		tmuxPath = path
	} else {
		// Fallback paths for common installations
		for _, p := range []string{"/opt/homebrew/bin/tmux", "/usr/local/bin/tmux", "/usr/bin/tmux"} {
			if _, err := os.Stat(p); err == nil {
				tmuxPath = p
				break
			}
		}
	}

	// Find ccc binary (self)
	if exe, err := os.Executable(); err == nil {
		cccPath = exe
	}

	// Find claude binary - first try PATH, then fallback paths
	if path, err := exec.LookPath("claude"); err == nil {
		claudePath = path
	} else {
		home, _ := os.UserHomeDir()
		claudePaths := []string{
			filepath.Join(home, ".claude", "local", "claude"),
			"/usr/local/bin/claude",
		}
		for _, p := range claudePaths {
			if _, err := os.Stat(p); err == nil {
				claudePath = p
				break
			}
		}
	}
}

// markTelegramSent creates a marker file indicating a message was just sent
// from Telegram to this topic's tmux session. Used by hook-prompt to avoid
// echoing Telegram-originated messages back to Telegram.
func markTelegramSent(topicID int64) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	dir := filepath.Join(home, ".ccc", "telegram-sent")
	os.MkdirAll(dir, 0755)
	marker := filepath.Join(dir, fmt.Sprintf("%d", topicID))
	os.WriteFile(marker, nil, 0644)
}

// wasTelegramSent checks if a message was sent from Telegram to this topic
// within the cooldown period (10 seconds).
func wasTelegramSent(topicID int64) bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	marker := filepath.Join(home, ".ccc", "telegram-sent", fmt.Sprintf("%d", topicID))
	info, err := os.Stat(marker)
	if err != nil {
		return false
	}
	return time.Since(info.ModTime()) < 10*time.Second
}

// tmuxVerbose returns true if CCC_TMUX_VERBOSE env is set
func tmuxVerbose() bool {
	return os.Getenv("CCC_TMUX_VERBOSE") != ""
}

// tmuxBaseArgs returns base tmux arguments including socket and optional verbose flag
func tmuxBaseArgs() []string {
	args := []string{"-S", tmuxSocket}
	if tmuxVerbose() {
		args = append([]string{"-v"}, args...)
	}
	return args
}

// tmuxLogDir returns the directory for tmux verbose logs (~/.ccc/tmux-logs/)
// and ensures it exists.
func tmuxLogDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	dir := filepath.Join(home, ".ccc", "tmux-logs")
	os.MkdirAll(dir, 0755)
	return dir
}

// tmuxCmd creates an exec.Cmd for tmux with proper base args
func tmuxCmd(cmdArgs ...string) *exec.Cmd {
	args := append(tmuxBaseArgs(), cmdArgs...)
	cmd := exec.Command(tmuxPath, args...)
	if tmuxVerbose() {
		if dir := tmuxLogDir(); dir != "" {
			cmd.Dir = dir
		}
	}
	return cmd
}

// ensureTmuxServer ensures tmux server is running by checking if socket exists
// If not, starts a new tmux server. This handles the case after system reboot
// when tmux hasn't been started yet and socket doesn't exist.
func ensureTmuxServer() error {
	// Check if socket directory exists
	socketDir := filepath.Dir(tmuxSocket)
	if _, err := os.Stat(socketDir); os.IsNotExist(err) {
		// Socket directory doesn't exist - create it with proper permissions (700)
		if err := os.MkdirAll(socketDir, 0700); err != nil {
			return fmt.Errorf("failed to create tmux socket directory: %w", err)
		}

		// Start tmux server by creating and immediately killing a temporary session
		cmd := tmuxCmd("new-session", "-d", "-s", "ccc-init")
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("failed to start tmux server: %w", err)
		}
		// Kill the temporary session
		tmuxCmd("kill-session", "-t", "ccc-init").Run()

		if tmuxVerbose() {
			fmt.Fprintf(os.Stderr, "tmux verbose logging enabled, logs in current directory\n")
		}
	}
	return nil
}

func tmuxSessionExists(name string) bool {
	// Ensure tmux server is running first
	if err := ensureTmuxServer(); err != nil {
		return false
	}
	cmd := tmuxCmd("has-session", "-t", name)
	return cmd.Run() == nil
}

func createTmuxSession(name string, workDir string, continueSession bool) error {
	// Ensure tmux server is running (handles post-reboot case)
	if err := ensureTmuxServer(); err != nil {
		return err
	}

	// Build the command to run inside tmux
	cccCmd := cccPath + " run"
	if continueSession {
		cccCmd += " -c"
	}

	// Create tmux session with a login shell (don't run command directly - it kills session on exit)
	cmd := tmuxCmd("new-session", "-d", "-s", name, "-c", workDir)
	if err := cmd.Run(); err != nil {
		return err
	}

	// Enable mouse mode for this session (allows scrolling)
	tmuxCmd("set-option", "-t", name, "mouse", "on").Run()

	// Send the command to the session via send-keys (preserves TTY properly)
	time.Sleep(200 * time.Millisecond)
	tmuxCmd( "send-keys", "-t", name, cccCmd, "C-m").Run()

	return nil
}

// runClaudeRaw runs claude directly (used inside tmux sessions)
func runClaudeRaw(continueSession bool) error {
	if claudePath == "" {
		return fmt.Errorf("claude binary not found")
	}

	args := []string{"--dangerously-skip-permissions"}
	if continueSession {
		args = append(args, "-c")
	}

	cmd := exec.Command(claudePath, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

// startSession creates/attaches to a tmux session with Telegram topic
func startSession(continueSession bool) error {
	// Get current directory name as session name
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	name := filepath.Base(cwd)
	tmuxName := tmuxSessionName(name)

	// Load config to check/create topic
	config, err := loadConfig()
	if err != nil {
		// No config, just run claude directly
		return runClaudeRaw(continueSession)
	}

	// Create topic if it doesn't exist and we have a group configured
	if config.GroupID != 0 {
		if _, exists := config.Sessions[name]; !exists {
			topicID, err := createForumTopic(config, name)
			if err == nil {
				config.Sessions[name] = &SessionInfo{
					TopicID: topicID,
					Path:    cwd,
				}
				saveConfig(config)
				fmt.Printf("📱 Created Telegram topic: %s\n", name)
			}
		}
	}

	// Check if tmux session exists
	if tmuxSessionExists(tmuxName) {
		// Check if we're already inside tmux
		if os.Getenv("TMUX") != "" {
			// Inside tmux: switch to the session
			cmd := tmuxCmd( "switch-client", "-t", tmuxName)
			cmd.Stdin = os.Stdin
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			return cmd.Run()
		}
		// Outside tmux: attach to existing session
		cmd := tmuxCmd( "attach-session", "-t", tmuxName)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}

	// Create new tmux session and attach
	if err := createTmuxSession(tmuxName, cwd, continueSession); err != nil {
		return err
	}

	// Check if we're already inside tmux
	if os.Getenv("TMUX") != "" {
		cmd := tmuxCmd( "switch-client", "-t", tmuxName)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}
	cmd := tmuxCmd( "attach-session", "-t", tmuxName)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func sendToTmux(session string, text string) error {
	// Always use 2 second delay before Enter to ensure text is fully processed
	// Without this delay, Enter may be interpreted as newline instead of submit
	return sendToTmuxWithDelay(session, text, 2*time.Second)
}

func sendToTmuxWithDelay(session string, text string, delay time.Duration) error {
	// Send text literally
	cmd := tmuxCmd( "send-keys", "-t", session, "-l", text)
	if err := cmd.Run(); err != nil {
		return err
	}

	// Wait for content to load (e.g., images, long pasted text)
	time.Sleep(delay)

	// Send Enter twice (Claude Code needs double Enter)
	cmd = tmuxCmd( "send-keys", "-t", session, "C-m")
	if err := cmd.Run(); err != nil {
		return err
	}
	time.Sleep(50 * time.Millisecond)
	cmd = tmuxCmd( "send-keys", "-t", session, "C-m")
	return cmd.Run()
}

func killTmuxSession(name string) error {
	cmd := tmuxCmd( "kill-session", "-t", name)
	return cmd.Run()
}

func listTmuxSessions() ([]string, error) {
	cmd := tmuxCmd( "list-sessions", "-F", "#{session_name}")
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

// TmuxSessionInfo holds information about a tmux session
type TmuxSessionInfo struct {
	Created  time.Time
	Activity time.Time
	Path     string
}

// getTmuxSessionInfo returns detailed info about a tmux session
func getTmuxSessionInfo(name string) (*TmuxSessionInfo, error) {
	cmd := tmuxCmd( "list-sessions", "-F",
		"#{session_name}\t#{session_created}\t#{session_activity}\t#{pane_current_path}",
		"-f", fmt.Sprintf("#{==:#{session_name},%s}", name))
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	line := strings.TrimSpace(string(out))
	if line == "" {
		return nil, fmt.Errorf("session not found")
	}

	parts := strings.Split(line, "\t")
	if len(parts) < 4 {
		return nil, fmt.Errorf("invalid tmux output")
	}

	created, _ := strconv.ParseInt(parts[1], 10, 64)
	activity, _ := strconv.ParseInt(parts[2], 10, 64)

	return &TmuxSessionInfo{
		Created:  time.Unix(created, 0),
		Activity: time.Unix(activity, 0),
		Path:     parts[3],
	}, nil
}

// sshGetTmuxSessionInfo returns detailed info about a remote tmux session
func sshGetTmuxSessionInfo(address string, name string) (*TmuxSessionInfo, error) {
	cmd := fmt.Sprintf("tmux list-sessions -F '#{session_name}\t#{session_created}\t#{session_activity}\t#{pane_current_path}' -f '#{==:#{session_name},%s}'", name)
	out, err := runSSH(address, cmd, 10*time.Second)
	if err != nil {
		return nil, err
	}

	line := strings.TrimSpace(out)
	if line == "" {
		return nil, fmt.Errorf("session not found")
	}

	parts := strings.Split(line, "\t")
	if len(parts) < 4 {
		return nil, fmt.Errorf("invalid tmux output")
	}

	created, _ := strconv.ParseInt(parts[1], 10, 64)
	activity, _ := strconv.ParseInt(parts[2], 10, 64)

	return &TmuxSessionInfo{
		Created:  time.Unix(created, 0),
		Activity: time.Unix(activity, 0),
		Path:     parts[3],
	}, nil
}

// formatDuration formats a duration in human-readable format
func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		hours := int(d.Hours())
		mins := int(d.Minutes()) % 60
		if mins > 0 {
			return fmt.Sprintf("%dh %dm", hours, mins)
		}
		return fmt.Sprintf("%dh", hours)
	}
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	if hours > 0 {
		return fmt.Sprintf("%dd %dh", days, hours)
	}
	return fmt.Sprintf("%dd", days)
}

// Session management

func createSession(config *Config, name string) error {
	// Check if session already exists
	if _, exists := config.Sessions[name]; exists {
		return fmt.Errorf("session '%s' already exists", name)
	}

	// Create Telegram topic
	topicID, err := createForumTopic(config, name)
	if err != nil {
		return fmt.Errorf("failed to create topic: %w", err)
	}

	// Create tmux session
	workDir := resolveProjectPath(config, name)
	if _, err := os.Stat(workDir); os.IsNotExist(err) {
		// Create project directory
		os.MkdirAll(workDir, 0755)
	}

	if err := createTmuxSession(tmuxSessionName(name), workDir, false); err != nil {
		return fmt.Errorf("failed to create tmux session: %w", err)
	}

	// Save mapping with full path
	config.Sessions[name] = &SessionInfo{
		TopicID: topicID,
		Path:    workDir,
	}
	if err := saveConfig(config); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}

	return nil
}

func killSession(config *Config, name string) error {
	sessionInfo, exists := config.Sessions[name]
	if !exists {
		return fmt.Errorf("session '%s' not found", name)
	}

	// Extract project name for tmux session (without host prefix)
	_, projectName := parseSessionTarget(name)
	tmuxName := tmuxSessionName(extractProjectName(projectName))

	// Kill tmux session (remote or local)
	if sessionInfo != nil && sessionInfo.Host != "" {
		address := getHostAddress(config, sessionInfo.Host)
		if address != "" {
			sshTmuxKillSession(address, tmuxName)
		}
	} else {
		killTmuxSession(tmuxName)
	}

	// Mark as deleted but keep in config to preserve topic mapping
	sessionInfo.Deleted = true
	saveConfig(config)

	return nil
}

func getSessionByTopic(cfg *Config, topicID int64) string { return config.GetSessionByTopic(cfg, topicID) }

// Client session management

// startClientSession starts a claude session on the client
// 1. Determines project path from args or cwd
// 2. Registers session on server via SSH (creates Telegram topic)
// 3. Creates/attaches tmux session with claude
func startClientSession(config *Config, args []string) error {
	// Check for -c flag
	continueSession := false
	filteredArgs := []string{}
	for _, arg := range args {
		if arg == "-c" {
			continueSession = true
		} else {
			filteredArgs = append(filteredArgs, arg)
		}
	}

	// Determine project path
	var projectPath string
	if len(filteredArgs) > 0 && filteredArgs[0] != "" {
		// Project name or path provided
		arg := filteredArgs[0]
		if strings.HasPrefix(arg, "/") || strings.HasPrefix(arg, "~") || strings.HasPrefix(arg, ".") {
			// Absolute or relative path
			projectPath = arg
		} else {
			// Just a name - create in home directory
			home, _ := os.UserHomeDir()
			projectPath = filepath.Join(home, arg)
		}
	} else {
		// Use current directory
		var err error
		projectPath, err = os.Getwd()
		if err != nil {
			return fmt.Errorf("cannot get current directory: %v", err)
		}
	}

	// Expand ~ if present
	if strings.HasPrefix(projectPath, "~") {
		home, _ := os.UserHomeDir()
		projectPath = filepath.Join(home, projectPath[1:])
	}

	// Make path absolute
	absPath, err := filepath.Abs(projectPath)
	if err != nil {
		return fmt.Errorf("cannot resolve path: %v", err)
	}
	projectPath = absPath

	// Check if directory exists, create if not
	if _, err := os.Stat(projectPath); os.IsNotExist(err) {
		fmt.Printf("Creating directory: %s\n", projectPath)
		if err := os.MkdirAll(projectPath, 0755); err != nil {
			return fmt.Errorf("cannot create directory: %v", err)
		}
	}

	// Session name for tmux
	name := filepath.Base(projectPath)
	tmuxName := tmuxSessionName(name)

	// Register session on server (creates Telegram topic)
	fmt.Printf("Registering session on server...\n")
	cmd := fmt.Sprintf("ccc register-session %s %s",
		shellQuote(config.HostName), shellQuote(projectPath))

	output, err := runSSH(config.Server, cmd, 10*time.Second)
	if err != nil {
		return fmt.Errorf("failed to register session: %v", err)
	}

	topicID := strings.TrimSpace(output)
	fmt.Printf("Session registered (topic: %s)\n", topicID)

	// Check if tmux session already exists
	if tmuxSessionExists(tmuxName) {
		fmt.Printf("Attaching to existing session: %s\n", tmuxName)
		if os.Getenv("TMUX") != "" {
			// Inside tmux: switch to the session
			switchCmd := tmuxCmd( "switch-client", "-t", tmuxName)
			switchCmd.Stdin = os.Stdin
			switchCmd.Stdout = os.Stdout
			switchCmd.Stderr = os.Stderr
			return switchCmd.Run()
		}
		// Outside tmux: attach to existing session
		attachCmd := tmuxCmd( "attach-session", "-t", tmuxName)
		attachCmd.Stdin = os.Stdin
		attachCmd.Stdout = os.Stdout
		attachCmd.Stderr = os.Stderr
		return attachCmd.Run()
	}

	// Create new tmux session
	fmt.Printf("Creating session: %s\n", tmuxName)
	if err := createTmuxSession(tmuxName, projectPath, continueSession); err != nil {
		return err
	}

	// Attach to the session
	if os.Getenv("TMUX") != "" {
		attachCmd := tmuxCmd( "switch-client", "-t", tmuxName)
		attachCmd.Stdin = os.Stdin
		attachCmd.Stdout = os.Stdout
		attachCmd.Stderr = os.Stderr
		return attachCmd.Run()
	}
	attachCmd := tmuxCmd( "attach-session", "-t", tmuxName)
	attachCmd.Stdin = os.Stdin
	attachCmd.Stdout = os.Stdout
	attachCmd.Stderr = os.Stderr
	return attachCmd.Run()
}

// Hook handling

// extractProjectDirFromTranscript extracts the encoded project directory from transcript path
// e.g., "/home/user/.claude/projects/-home-user-Projects-myapp/transcript.json" -> "-home-user-Projects-myapp"
func extractProjectDirFromTranscript(transcriptPath string) string {
	if transcriptPath == "" {
		return ""
	}
	// Find "/projects/" in the path
	idx := strings.Index(transcriptPath, "/projects/")
	if idx == -1 {
		return ""
	}
	// Get the part after "/projects/"
	rest := transcriptPath[idx+len("/projects/"):]
	// Take only the directory name (before next /)
	if slashIdx := strings.Index(rest, "/"); slashIdx != -1 {
		return rest[:slashIdx]
	}
	return rest
}

// resolveProjectPathFromTranscript finds the actual project path by matching encoded transcript dir with cwd segments
// Returns the project path if found, empty string otherwise
func resolveProjectPathFromTranscript(encodedProjectDir string, cwd string) string {
	if encodedProjectDir == "" || cwd == "" {
		return ""
	}

	// Normalize cwd (expand ~)
	if strings.HasPrefix(cwd, "~") {
		home, _ := os.UserHomeDir()
		cwd = home + cwd[1:]
	}

	// Split into segments
	segments := strings.Split(cwd, "/")

	// Build path incrementally and check for match
	currentPath := ""
	for _, segment := range segments {
		if segment == "" {
			continue
		}
		currentPath += "/" + segment

		// Encode current path the same way Claude does: replace / with -
		encoded := strings.ReplaceAll(currentPath, "/", "-")

		if encoded == encodedProjectDir {
			return currentPath
		}
	}

	return ""
}

// logHook writes hook events to ~/.ccc/hooks.log for debugging
func logHook(hookType string, format string, args ...interface{}) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}

	logDir := filepath.Join(home, ".ccc")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return
	}

	logPath := filepath.Join(logDir, "hooks.log")
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()

	timestamp := time.Now().Format("2006-01-02 15:04:05")
	message := fmt.Sprintf(format, args...)
	fmt.Fprintf(f, "[%s] [%s] %s\n", timestamp, hookType, message)
}

// forwardToServer forwards a message to the server in client mode
// Returns true if forwarded (client mode), false otherwise
func forwardToServer(config *Config, cwd string, transcriptPath string, message string) bool {
	if config.Mode != "client" || config.Server == "" || config.HostName == "" {
		return false
	}

	// Extract encoded project dir from transcript path
	projectDir := extractProjectDirFromTranscript(transcriptPath)

	// Truncate message for log
	logMsg := message
	if len(logMsg) > 100 {
		logMsg = logMsg[:100] + "..."
	}
	logHook("Forward", "server=%s cwd=%s project=%s msg=%s", config.Server, cwd, projectDir, logMsg)

	// Forward to server via SSH
	// Use base64 to safely encode the message
	encoded := base64.StdEncoding.EncodeToString([]byte(message))
	cmd := fmt.Sprintf("ccc --from=%s --cwd=%s --project=%s \"$(echo %s | base64 -d)\"",
		shellQuote(config.HostName), shellQuote(cwd), shellQuote(projectDir), encoded)

	fmt.Fprintf(os.Stderr, "hook: forwarding to server %s (project=%s)\n", config.Server, projectDir)
	_, err := runSSH(config.Server, cmd, 10*time.Second)
	if err != nil {
		logHook("Forward", "ERROR: %v", err)
		fmt.Fprintf(os.Stderr, "hook: forward error: %v\n", err)
	} else {
		logHook("Forward", "SUCCESS")
	}
	return true
}

func handleHook() error {
	logHook("Stop", "hook started")

	config, err := loadConfig()
	if err != nil {
		logHook("Stop", "ERROR: no config")
		fmt.Fprintf(os.Stderr, "hook: no config\n")
		return nil
	}

	// Read hook data from stdin
	var hookData HookData
	decoder := json.NewDecoder(os.Stdin)
	if err := decoder.Decode(&hookData); err != nil {
		logHook("Stop", "ERROR: decode error: %v", err)
		fmt.Fprintf(os.Stderr, "hook: decode error: %v\n", err)
		return nil
	}

	logHook("Stop", "cwd=%s transcript=%s", hookData.Cwd, hookData.TranscriptPath)
	fmt.Fprintf(os.Stderr, "hook: cwd=%s transcript=%s\n", hookData.Cwd, hookData.TranscriptPath)

	// Delay to allow transcript file to be fully written
	// (race condition: hook fires before final message is flushed to disk)
	time.Sleep(2 * time.Second)

	// Read last message from transcript
	lastMessage := "Session ended"
	if hookData.TranscriptPath != "" {
		if msg := getLastAssistantMessage(hookData.TranscriptPath); msg != "" {
			lastMessage = msg
		}
	}

	// Truncate message for log (first 100 chars)
	logMsg := lastMessage
	if len(logMsg) > 100 {
		logMsg = logMsg[:100] + "..."
	}
	logHook("Stop", "message=%s", logMsg)

	// In client mode, forward to server
	if forwardToServer(config, hookData.Cwd, hookData.TranscriptPath, lastMessage) {
		logHook("Stop", "forwarded to server %s", config.Server)
		return nil
	}

	// Find session by matching cwd with saved path
	// Prefer local sessions (Host=="") over remote sessions with same path
	var sessionName string
	var topicID int64
	var foundRemote string // Track remote match in case no local match
	var remoteTopicID int64
	for name, info := range config.Sessions {
		if info == nil || info.Deleted {
			continue
		}
		// Match against saved path, subdirectories of saved path, or suffix
		if hookData.Cwd == info.Path || strings.HasPrefix(hookData.Cwd, info.Path+"/") || strings.HasSuffix(hookData.Cwd, "/"+name) {
			if info.Host == "" {
				// Local session - use immediately
				sessionName = name
				topicID = info.TopicID
				break
			} else if foundRemote == "" {
				// Remote session - save as fallback
				foundRemote = name
				remoteTopicID = info.TopicID
			}
		}
	}
	// Use remote match if no local match found
	if sessionName == "" && foundRemote != "" {
		sessionName = foundRemote
		topicID = remoteTopicID
	}
	if sessionName == "" || config.GroupID == 0 {
		logHook("Stop", "ERROR: no session found for cwd=%s", hookData.Cwd)
		fmt.Fprintf(os.Stderr, "hook: no session found for cwd=%s\n", hookData.Cwd)
		return nil
	}

	logHook("Stop", "session=%s topic=%d, sending to telegram", sessionName, topicID)
	fmt.Fprintf(os.Stderr, "hook: session=%s topic=%d\n", sessionName, topicID)
	fmt.Fprintf(os.Stderr, "hook: sending message to telegram\n")

	// Stop typing indicator for this session
	stopContinuousTyping(sessionName)

	// Store Claude's response in history
	appendHistory(topicID, HistoryMessage{
		ID:        nextMessageID(),
		Timestamp: time.Now().Unix(),
		From:      "claude",
		Text:      lastMessage,
	})

	return sendMessage(config, config.GroupID, topicID, fmt.Sprintf("✅ %s\n\n%s", sessionName, lastMessage))
}

func handlePermissionHook() error {
	// Recover from any panic - hooks must never crash
	defer func() {
		recover()
	}()

	// Read stdin with timeout
	stdinData := make(chan []byte, 1)
	go func() {
		defer func() { recover() }()
		data, _ := io.ReadAll(os.Stdin)
		stdinData <- data
	}()

	var rawData []byte
	select {
	case rawData = <-stdinData:
	case <-time.After(2 * time.Second):
		return nil // Timeout, exit silently
	}

	if len(rawData) == 0 {
		return nil
	}

	// Parse JSON - ignore errors
	var hookData HookData
	if err := json.Unmarshal(rawData, &hookData); err != nil {
		return nil
	}

	// Load config - ignore errors
	config, err := loadConfig()
	if err != nil || config == nil {
		return nil
	}

	// Find session by matching cwd suffix
	var sessionName string
	var topicID int64
	for name, info := range config.Sessions {
		if name == "" || info == nil {
			continue
		}
		if hookData.Cwd == info.Path || strings.HasPrefix(hookData.Cwd, info.Path+"/") || strings.HasSuffix(hookData.Cwd, "/"+name) {
			sessionName = name
			topicID = info.TopicID
			break
		}
	}

	if sessionName == "" || config.GroupID == 0 {
		return nil
	}

	// Handle AskUserQuestion (plan approval, etc.) - in goroutine to not block
	fmt.Fprintf(os.Stderr, "hook-permission: tool=%s questions=%d\n", hookData.ToolName, len(hookData.ToolInput.Questions))
	if hookData.ToolName == "AskUserQuestion" && len(hookData.ToolInput.Questions) > 0 {
		go func() {
			defer func() { recover() }()
			for qIdx, q := range hookData.ToolInput.Questions {
				if q.Question == "" {
					continue
				}
				// Build message
				msg := fmt.Sprintf("❓ %s\n\n%s", q.Header, q.Question)

				// Build inline keyboard buttons
				var buttons [][]InlineKeyboardButton
				for i, opt := range q.Options {
					if opt.Label == "" {
						continue
					}
					// Callback data format: session:questionIndex:optionIndex
					// Telegram limits callback_data to 64 bytes
					callbackData := fmt.Sprintf("%s:%d:%d", sessionName, qIdx, i)
					if len(callbackData) > 64 {
						callbackData = callbackData[:64]
					}
					buttons = append(buttons, []InlineKeyboardButton{
						{Text: opt.Label, CallbackData: callbackData},
					})
				}

				if len(buttons) > 0 {
					sendMessageWithKeyboard(config, config.GroupID, topicID, msg, buttons)
				}
			}
		}()
		return nil
	}

	// Generic permission request - in goroutine to not block
	go func() {
		defer func() { recover() }()
		if hookData.ToolName != "" {
			msg := fmt.Sprintf("🔐 Permission requested: %s", hookData.ToolName)
			sendMessage(config, config.GroupID, topicID, msg)
		}
	}()

	return nil
}

func getLastAssistantMessage(transcriptPath string) string {
	file, err := os.Open(transcriptPath)
	if err != nil {
		logHook("Parse", "failed to open transcript: %v", err)
		return ""
	}
	defer file.Close()

	var allTexts []string
	var linesProcessed, assistantCount, textCount int
	scanner := bufio.NewScanner(file)
	// Increase buffer size for large lines (up to 16MB for transcripts with images/PDFs)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 16*1024*1024)

	for scanner.Scan() {
		linesProcessed++
		var entry map[string]interface{}
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}

		entryType, _ := entry["type"].(string)

		// Reset on actual user message (not tool_result) - start fresh collection
		if entryType == "user" {
			if msg, ok := entry["message"].(map[string]interface{}); ok {
				// Case 1: content is a string (simple user message)
				if _, ok := msg["content"].(string); ok {
					allTexts = nil
				} else if content, ok := msg["content"].([]interface{}); ok && len(content) > 0 {
					// Case 2: content is an array
					if block, ok := content[0].(map[string]interface{}); ok {
						// Only reset if first content block is "text" (real user message),
						// not "tool_result" which is just a response to tool_use
						if block["type"] == "text" {
							allTexts = nil
						}
					}
				}
			}
		}

		if entryType == "assistant" {
			assistantCount++
			if msg, ok := entry["message"].(map[string]interface{}); ok {
				if content, ok := msg["content"].([]interface{}); ok {
					for _, c := range content {
						if block, ok := c.(map[string]interface{}); ok {
							if block["type"] == "text" {
								if text, ok := block["text"].(string); ok {
									textCount++
									allTexts = append(allTexts, text)
								}
							}
						}
					}
				}
			}
		}
	}

	if err := scanner.Err(); err != nil {
		logHook("Parse", "scanner error after %d lines: %v", linesProcessed, err)
	}
	logHook("Parse", "processed %d lines, %d assistant entries, %d text blocks since last user msg", linesProcessed, assistantCount, len(allTexts))

	// Join all text blocks from the last turn
	return strings.Join(allTexts, "\n\n")
}

func handlePromptHook() error {
	config, err := loadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "hook-prompt: no config\n")
		return nil
	}

	var hookData HookData
	decoder := json.NewDecoder(os.Stdin)
	if err := decoder.Decode(&hookData); err != nil {
		fmt.Fprintf(os.Stderr, "hook-prompt: decode error: %v\n", err)
		return nil
	}

	if hookData.Prompt == "" {
		fmt.Fprintf(os.Stderr, "hook-prompt: empty prompt\n")
		return nil
	}

	// Prepare prompt message
	prompt := hookData.Prompt
	if len(prompt) > 500 {
		prompt = prompt[:500] + "..."
	}

	// In client mode, forward to server
	if forwardToServer(config, hookData.Cwd, hookData.TranscriptPath, fmt.Sprintf("💬 %s", prompt)) {
		return nil
	}

	// Find session by matching cwd suffix
	var topicID int64
	for name, info := range config.Sessions {
		if info == nil {
			continue
		}
		if hookData.Cwd == info.Path || strings.HasPrefix(hookData.Cwd, info.Path+"/") || strings.HasSuffix(hookData.Cwd, "/"+name) {
			topicID = info.TopicID
			break
		}
	}

	if topicID == 0 || config.GroupID == 0 {
		fmt.Fprintf(os.Stderr, "hook-prompt: no topic found for cwd=%s\n", hookData.Cwd)
		return nil
	}

	// Check if this prompt was just sent from Telegram (cooldown 10s)
	if wasTelegramSent(topicID) {
		fmt.Fprintf(os.Stderr, "hook-prompt: skipping (telegram cooldown) topic=%d\n", topicID)
		return nil
	}

	// This is a locally-typed prompt — save to history
	appendHistory(topicID, HistoryMessage{
		ID:        nextMessageID(),
		Timestamp: time.Now().Unix(),
		From:      "human",
		Text:      hookData.Prompt,
		Username:  "terminal",
	})

	// Send typing action
	sendTypingAction(config, config.GroupID, topicID)

	fmt.Fprintf(os.Stderr, "hook-prompt: sending local prompt to topic %d\n", topicID)
	return sendMessage(config, config.GroupID, topicID, fmt.Sprintf("💬 %s", prompt))
}

func handleOutputHook() error {
	config, err := loadConfig()
	if err != nil {
		return nil
	}

	rawData, _ := io.ReadAll(os.Stdin)
	if len(rawData) == 0 {
		return nil
	}

	var hookData HookData
	if err := json.Unmarshal(rawData, &hookData); err != nil {
		return nil
	}

	// Skip certain tools that don't produce interesting output
	skipTools := map[string]bool{
		"Read": true, "Glob": true, "Grep": true, "LSP": true,
		"TodoWrite": true, "Task": true, "TaskOutput": true,
	}
	if skipTools[hookData.ToolName] {
		return nil
	}

	// Get last message from transcript
	var msg string
	if hookData.TranscriptPath != "" {
		msg = getLastAssistantMessage(hookData.TranscriptPath)
	}
	if msg == "" {
		return nil
	}

	// Truncate long messages
	if len(msg) > 1000 {
		msg = msg[:1000] + "..."
	}

	// In client mode, forward to server
	if forwardToServer(config, hookData.Cwd, hookData.TranscriptPath, msg) {
		return nil
	}

	// Find session
	var sessionName string
	var topicID int64
	for name, info := range config.Sessions {
		if info == nil {
			continue
		}
		if hookData.Cwd == info.Path || strings.HasPrefix(hookData.Cwd, info.Path+"/") || strings.HasSuffix(hookData.Cwd, "/"+name) {
			sessionName = name
			topicID = info.TopicID
			break
		}
	}

	if topicID == 0 || config.GroupID == 0 || sessionName == "" {
		return nil
	}

	// Check cache to avoid duplicate messages
	cacheFile := filepath.Join(os.TempDir(), "ccc-cache-"+sessionName)
	lastSent, _ := os.ReadFile(cacheFile)
	if string(lastSent) == msg {
		return nil // Skip duplicate
	}
	os.WriteFile(cacheFile, []byte(msg), 0600)

	sendMessage(config, config.GroupID, topicID, msg)
	return nil
}

func handleQuestionHook() error {
	config, err := loadConfig()
	if err != nil {
		return nil
	}

	rawData, _ := io.ReadAll(os.Stdin)
	if len(rawData) == 0 {
		return nil
	}

	var hookData HookData
	if err := json.Unmarshal(rawData, &hookData); err != nil {
		return nil
	}

	// Find session by matching cwd suffix
	var sessionName string
	var topicID int64
	for name, info := range config.Sessions {
		if info == nil {
			continue
		}
		if hookData.Cwd == info.Path || strings.HasPrefix(hookData.Cwd, info.Path+"/") || strings.HasSuffix(hookData.Cwd, "/"+name) {
			sessionName = name
			topicID = info.TopicID
			break
		}
	}

	if sessionName == "" || config.GroupID == 0 || topicID == 0 {
		return nil
	}

	// Send questions to Telegram
	for qIdx, q := range hookData.ToolInput.Questions {
		if q.Question == "" {
			continue
		}
		msg := fmt.Sprintf("❓ %s\n\n%s", q.Header, q.Question)

		var buttons [][]InlineKeyboardButton
		for i, opt := range q.Options {
			if opt.Label == "" {
				continue
			}
			callbackData := fmt.Sprintf("%s:%d:%d", sessionName, qIdx, i)
			if len(callbackData) > 64 {
				callbackData = callbackData[:64]
			}
			buttons = append(buttons, []InlineKeyboardButton{
				{Text: opt.Label, CallbackData: callbackData},
			})
		}

		if len(buttons) > 0 {
			sendMessageWithKeyboard(config, config.GroupID, topicID, msg, buttons)
		} else {
			sendMessage(config, config.GroupID, topicID, msg)
		}
	}

	return nil
}

// Install hook in Claude settings

// addHookToEvent adds a hook command to an event without overwriting existing hooks.
// Returns true if the hook was added, false if it already exists.
func addHookToEvent(hooks map[string]interface{}, eventName string, command string) bool {
	// Get existing entries for this event
	entries, ok := hooks[eventName].([]interface{})
	if !ok {
		// No entries for this event - create new
		hooks[eventName] = []interface{}{
			map[string]interface{}{
				"matcher": "",
				"hooks": []interface{}{
					map[string]interface{}{
						"type":    "command",
						"command": command,
					},
				},
			},
		}
		return true
	}

	// Find entry with empty matcher (global hook)
	for _, entry := range entries {
		entryMap, ok := entry.(map[string]interface{})
		if !ok {
			continue
		}
		matcher, _ := entryMap["matcher"].(string)
		if matcher != "" {
			continue
		}

		// Found global entry, check if hook already exists
		hooksList, ok := entryMap["hooks"].([]interface{})
		if !ok {
			hooksList = []interface{}{}
		}

		for _, h := range hooksList {
			hookMap, ok := h.(map[string]interface{})
			if !ok {
				continue
			}
			if hookMap["command"] == command {
				// Hook already exists
				return false
			}
		}

		// Add hook to existing entry
		hooksList = append(hooksList, map[string]interface{}{
			"type":    "command",
			"command": command,
		})
		entryMap["hooks"] = hooksList
		return true
	}

	// No global entry found - create new one
	entries = append(entries, map[string]interface{}{
		"matcher": "",
		"hooks": []interface{}{
			map[string]interface{}{
				"type":    "command",
				"command": command,
			},
		},
	})
	hooks[eventName] = entries
	return true
}

func installHook() error {
	home, _ := os.UserHomeDir()
	claudeDir := filepath.Join(home, ".claude")
	settingsPath := filepath.Join(claudeDir, "settings.json")
	cccPath := filepath.Join(home, "bin", "ccc")

	// Create .claude directory if it doesn't exist
	if err := os.MkdirAll(claudeDir, 0755); err != nil {
		return fmt.Errorf("failed to create .claude directory: %w", err)
	}

	var settings map[string]interface{}

	data, err := os.ReadFile(settingsPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Create empty settings if file doesn't exist
			settings = make(map[string]interface{})
		} else {
			return fmt.Errorf("failed to read settings.json: %w", err)
		}
	} else {
		if err := json.Unmarshal(data, &settings); err != nil {
			return fmt.Errorf("failed to parse settings.json: %w", err)
		}
	}

	hooks, ok := settings["hooks"].(map[string]interface{})
	if !ok {
		hooks = make(map[string]interface{})
	}

	// Add Stop hook (doesn't overwrite existing hooks)
	stopAdded := addHookToEvent(hooks, "Stop", cccPath+" hook")

	settings["hooks"] = hooks

	newData, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal settings: %w", err)
	}

	if err := os.WriteFile(settingsPath, newData, 0600); err != nil {
		return fmt.Errorf("failed to write settings.json: %w", err)
	}

	if stopAdded {
		fmt.Println("✅ Claude hook installed!")
	} else {
		fmt.Println("✅ Claude hook already installed")
	}
	return nil
}

// Bot commands

func setBotCommands(botToken string) {
	commands := `{
		"commands": [
			{"command": "help", "description": "Show all commands"},
			{"command": "new", "description": "Create session: /new [host:]<name>"},
			{"command": "continue", "description": "Continue session: /continue [host:]<name>"},
			{"command": "kill", "description": "Kill session: /kill <name>"},
			{"command": "list", "description": "List sessions with status"},
			{"command": "status", "description": "Show current session details"},
			{"command": "host", "description": "Manage hosts: /host add|del|list|check"},
			{"command": "rc", "description": "Remote command: /rc <host> <cmd>"},
			{"command": "setdir", "description": "Set projects dir: /setdir [host:]<path>"},
			{"command": "away", "description": "Toggle notifications"},
			{"command": "c", "description": "Local command: /c <cmd>"},
			{"command": "screenshot", "description": "Take screenshot of display"},
			{"command": "ping", "description": "Check bot status"}
		]
	}`

	resp, err := http.Post(
		fmt.Sprintf("https://api.telegram.org/bot%s/setMyCommands", botToken),
		"application/json",
		strings.NewReader(commands),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to set bot commands: %v\n", err)
		return
	}
	resp.Body.Close()
}

// Execute shell command

func executeCommand(cmdStr string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "bash", "-c", cmdStr)
	cmd.Dir, _ = os.UserHomeDir()

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	output := stdout.String()
	if stderr.Len() > 0 {
		if output != "" {
			output += "\n"
		}
		output += stderr.String()
	}

	if output == "" {
		if err != nil {
			output = fmt.Sprintf("Error: %v", err)
		} else {
			output = "(no output)"
		}
	}

	return strings.TrimSpace(output), err
}

// One-shot Claude run (for private chat)

func runClaude(prompt string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	home, _ := os.UserHomeDir()
	workDir := home

	words := strings.Fields(prompt)
	if len(words) > 0 {
		firstWord := words[0]
		potentialDir := filepath.Join(home, firstWord)
		if info, err := os.Stat(potentialDir); err == nil && info.IsDir() {
			workDir = potentialDir
			prompt = strings.TrimSpace(strings.TrimPrefix(prompt, firstWord))
			if prompt == "" {
				return "Error: no prompt provided after directory name", nil
			}
		}
	}

	if claudePath == "" {
		return "Error: claude binary not found", fmt.Errorf("claude not found")
	}
	cmd := exec.CommandContext(ctx, claudePath, "--dangerously-skip-permissions", "-p", prompt)
	cmd.Dir = workDir

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	output := stdout.String()
	if stderr.Len() > 0 {
		if output != "" {
			output += "\n"
		}
		output += stderr.String()
	}

	if output == "" {
		if err != nil {
			output = fmt.Sprintf("Error: %v", err)
		} else {
			output = "(no output)"
		}
	}

	return strings.TrimSpace(output), err
}

// Setup - complete setup process

func installService() error {
	home, _ := os.UserHomeDir()

	// Detect OS and install appropriate service
	if _, err := os.Stat("/Library"); err == nil {
		// macOS - use launchd
		return installLaunchdService(home)
	}
	// Linux - use systemd
	return installSystemdService(home)
}

func installLaunchdService(home string) error {
	plistDir := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(plistDir, 0755); err != nil {
		return fmt.Errorf("failed to create LaunchAgents dir: %w", err)
	}

	plistPath := filepath.Join(plistDir, "com.ccc.plist")
	logPath := filepath.Join(home, ".ccc.log")

	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.ccc</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
        <string>listen</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>%s</string>
    <key>StandardErrorPath</key>
    <string>%s</string>
</dict>
</plist>
`, cccPath, logPath, logPath)

	if err := os.WriteFile(plistPath, []byte(plist), 0644); err != nil {
		return fmt.Errorf("failed to write plist: %w", err)
	}

	// Unload if exists, then load
	exec.Command("launchctl", "unload", plistPath).Run()
	if err := exec.Command("launchctl", "load", plistPath).Run(); err != nil {
		return fmt.Errorf("failed to load service: %w", err)
	}

	fmt.Println("✅ Service installed and started (launchd)")
	return nil
}

func installSystemdService(home string) error {
	serviceDir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(serviceDir, 0755); err != nil {
		return fmt.Errorf("failed to create systemd dir: %w", err)
	}

	servicePath := filepath.Join(serviceDir, "ccc.service")
	service := fmt.Sprintf(`[Unit]
Description=Claude Code Companion
After=network.target

[Service]
ExecStart=%s listen
Restart=always
RestartSec=10

[Install]
WantedBy=default.target
`, cccPath)

	if err := os.WriteFile(servicePath, []byte(service), 0644); err != nil {
		return fmt.Errorf("failed to write service file: %w", err)
	}

	// Reload and start
	exec.Command("systemctl", "--user", "daemon-reload").Run()
	exec.Command("systemctl", "--user", "enable", "ccc").Run()
	if err := exec.Command("systemctl", "--user", "start", "ccc").Run(); err != nil {
		return fmt.Errorf("failed to start service: %w", err)
	}

	fmt.Println("✅ Service installed and started (systemd)")
	return nil
}

func setup(botToken string) error {
	fmt.Println("🚀 Claude Code Companion Setup")
	fmt.Println("==============================")
	fmt.Println()

	config := &Config{BotToken: botToken, Sessions: make(map[string]*SessionInfo)}

	// Step 1: Get chat ID
	fmt.Println("Step 1/4: Connecting to Telegram...")
	fmt.Println("📱 Send any message to your bot in Telegram")
	fmt.Println("   Waiting...")

	offset := 0
	for {
		resp, err := http.Get(fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates?offset=%d&timeout=30", botToken, offset))
		if err != nil {
			return fmt.Errorf("failed to get updates: %w", err)
		}

		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		var updates TelegramUpdate
		if err := json.Unmarshal(body, &updates); err != nil {
			return fmt.Errorf("failed to parse response: %w", err)
		}

		if !updates.OK {
			return fmt.Errorf("telegram API error - check your bot token")
		}

		for _, update := range updates.Result {
			offset = update.UpdateID + 1
			if update.Message.Chat.ID != 0 {
				config.ChatID = update.Message.Chat.ID
				if err := saveConfig(config); err != nil {
					return fmt.Errorf("failed to save config: %w", err)
				}
				fmt.Printf("✅ Connected! (User: @%s)\n\n", update.Message.From.Username)
				goto step2
			}
		}

		time.Sleep(time.Second)
	}

step2:
	// Step 2: Group setup (optional)
	fmt.Println("Step 2/4: Group setup (optional)")
	fmt.Println("   For session topics, create a Telegram group with Topics enabled,")
	fmt.Println("   add your bot as admin, and send a message there.")
	fmt.Println("   Or press Enter to skip...")

	// Non-blocking check for group message with timeout
	fmt.Println("   Waiting 30 seconds for group message...")

	client := &http.Client{Timeout: 35 * time.Second}
	deadline := time.Now().Add(30 * time.Second)

	for time.Now().Before(deadline) {
		reqURL := fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates?offset=%d&timeout=5", config.BotToken, offset)
		resp, err := client.Get(reqURL)
		if err != nil {
			continue
		}

		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		var updates TelegramUpdate
		json.Unmarshal(body, &updates)

		for _, update := range updates.Result {
			offset = update.UpdateID + 1
			chat := update.Message.Chat
			if chat.Type == "supergroup" {
				config.GroupID = chat.ID
				saveConfig(config)
				fmt.Printf("✅ Group configured!\n\n")
				goto step3
			}
		}
	}
	fmt.Println("⏭️  Skipped (you can run 'ccc setgroup' later)")

step3:
	// Step 3: Install Claude hook
	fmt.Println("Step 3/4: Installing Claude hook...")
	if err := installHook(); err != nil {
		fmt.Printf("⚠️  Hook installation failed: %v\n", err)
		fmt.Println("   You can install it later with: ccc install")
	} else {
		fmt.Println()
	}

	// Step 4: Install service
	fmt.Println("Step 4/4: Installing background service...")
	if err := installService(); err != nil {
		fmt.Printf("⚠️  Service installation failed: %v\n", err)
		fmt.Println("   You can start manually with: ccc listen")
	} else {
		fmt.Println()
	}

	// Done!
	fmt.Println("==============================")
	fmt.Println("✅ Setup complete!")
	fmt.Println()
	fmt.Println("Usage:")
	fmt.Println("  ccc           Start Claude Code in current directory")
	fmt.Println("  ccc -c        Continue previous session")
	fmt.Println()
	if config.GroupID != 0 {
		fmt.Println("Telegram commands (in your group):")
		fmt.Println("  /new <name>   Create new session")
		fmt.Println("  /list         List sessions")
	} else {
		fmt.Println("To enable Telegram session topics:")
		fmt.Println("  1. Create a group with Topics enabled")
		fmt.Println("  2. Add bot as admin")
		fmt.Println("  3. Run: ccc setgroup")
	}

	return nil
}

func setGroup(config *Config) error {
	fmt.Println("Send a message in the group where you want to use topics...")
	fmt.Println("(Make sure Topics are enabled in group settings)")

	offset := 0
	client := &http.Client{Timeout: 35 * time.Second}

	for {
		reqURL := fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates?offset=%d&timeout=30", config.BotToken, offset)
		resp, err := client.Get(reqURL)
		if err != nil {
			return err
		}

		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		var updates TelegramUpdate
		if err := json.Unmarshal(body, &updates); err != nil {
			continue
		}

		for _, update := range updates.Result {
			offset = update.UpdateID + 1
			chat := update.Message.Chat
			if chat.Type == "supergroup" && update.Message.From.ID == config.ChatID {
				config.GroupID = chat.ID
				if err := saveConfig(config); err != nil {
					return err
				}
				fmt.Printf("Group set: %d\n", chat.ID)
				fmt.Println("You can now create sessions with: /new <name>")
				return nil
			}
		}
	}
}

// Doctor - check all dependencies

func doctor() {
	fmt.Println("🩺 ccc doctor")
	fmt.Println("=============")
	fmt.Println()

	allGood := true

	// Check tmux
	fmt.Print("tmux.............. ")
	if tmuxPath != "" {
		fmt.Printf("✅ %s\n", tmuxPath)
	} else {
		fmt.Println("❌ not found")
		fmt.Println("   Install: brew install tmux (macOS) or apt install tmux (Linux)")
		allGood = false
	}

	// Check claude
	fmt.Print("claude............ ")
	if claudePath != "" {
		fmt.Printf("✅ %s\n", claudePath)
	} else {
		fmt.Println("❌ not found")
		fmt.Println("   Install: npm install -g @anthropic-ai/claude-code")
		allGood = false
	}

	// Check ccc is in ~/bin (for hooks)
	fmt.Print("ccc in ~/bin...... ")
	home, _ := os.UserHomeDir()
	expectedCccPath := filepath.Join(home, "bin", "ccc")
	if _, err := os.Stat(expectedCccPath); err == nil {
		fmt.Printf("✅ %s\n", expectedCccPath)
	} else {
		fmt.Println("❌ not found")
		fmt.Println("   Run: mkdir -p ~/bin && cp ccc ~/bin/")
		allGood = false
	}

	// Check config
	fmt.Print("config............ ")
	config, err := loadConfig()
	if err != nil {
		fmt.Println("❌ not found")
		fmt.Println("   Run: ccc setup <bot_token>")
		allGood = false
	} else {
		fmt.Printf("✅ %s\n", getConfigPath())

		// Check bot token
		fmt.Print("  bot_token....... ")
		if config.BotToken != "" {
			fmt.Println("✅ configured")
		} else {
			fmt.Println("❌ missing")
			allGood = false
		}

		// Check chat ID
		fmt.Print("  chat_id......... ")
		if config.ChatID != 0 {
			fmt.Printf("✅ %d\n", config.ChatID)
		} else {
			fmt.Println("❌ missing")
			allGood = false
		}

		// Check group ID (optional)
		fmt.Print("  group_id........ ")
		if config.GroupID != 0 {
			fmt.Printf("✅ %d\n", config.GroupID)
		} else {
			fmt.Println("⚠️  not set (optional, run: ccc setgroup)")
		}
	}

	// Check Claude hook
	fmt.Print("claude hook....... ")
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	if data, err := os.ReadFile(settingsPath); err == nil {
		var settings map[string]interface{}
		if json.Unmarshal(data, &settings) == nil {
			if hooks, ok := settings["hooks"].(map[string]interface{}); ok {
				if _, hasStop := hooks["Stop"]; hasStop {
					fmt.Println("✅ installed")
				} else {
					fmt.Println("❌ not installed")
					fmt.Println("   Run: ccc install")
					allGood = false
				}
			} else {
				fmt.Println("❌ not installed")
				fmt.Println("   Run: ccc install")
				allGood = false
			}
		} else {
			fmt.Println("⚠️  settings.json parse error")
		}
	} else {
		fmt.Println("⚠️  ~/.claude/settings.json not found")
	}

	// Check service
	fmt.Print("service........... ")
	if _, err := os.Stat("/Library"); err == nil {
		// macOS - check launchd
		plistPath := filepath.Join(home, "Library", "LaunchAgents", "com.ccc.plist")
		if _, err := os.Stat(plistPath); err == nil {
			// Check if loaded
			cmd := exec.Command("launchctl", "list", "com.ccc")
			if cmd.Run() == nil {
				fmt.Println("✅ running (launchd)")
			} else {
				fmt.Println("⚠️  installed but not running")
				fmt.Println("   Run: launchctl load ~/Library/LaunchAgents/com.ccc.plist")
			}
		} else {
			fmt.Println("❌ not installed")
			fmt.Println("   Run: ccc setup <token> (or manually create plist)")
			allGood = false
		}
	} else {
		// Linux - check systemd
		cmd := exec.Command("systemctl", "--user", "is-active", "ccc")
		if output, err := cmd.Output(); err == nil && strings.TrimSpace(string(output)) == "active" {
			fmt.Println("✅ running (systemd)")
		} else {
			servicePath := filepath.Join(home, ".config", "systemd", "user", "ccc.service")
			if _, err := os.Stat(servicePath); err == nil {
				fmt.Println("⚠️  installed but not running")
				fmt.Println("   Run: systemctl --user start ccc")
			} else {
				fmt.Println("❌ not installed")
				fmt.Println("   Run: ccc setup <token> (or manually create service)")
				allGood = false
			}
		}
	}

	// Check transcription (optional)
	fmt.Print("transcription..... ")
	if config != nil && config.TranscriptionCmd != "" {
		cmdPath := expandPath(config.TranscriptionCmd)
		if _, err := os.Stat(cmdPath); err == nil {
			fmt.Printf("✅ %s\n", cmdPath)
		} else if _, err := exec.LookPath(config.TranscriptionCmd); err == nil {
			fmt.Printf("✅ %s (in PATH)\n", config.TranscriptionCmd)
		} else {
			fmt.Printf("❌ %s not found\n", config.TranscriptionCmd)
			fmt.Println("   Check transcription_cmd in ~/.ccc.json")
		}
	} else if whisperPath, err := exec.LookPath("whisper"); err == nil {
		fmt.Printf("✅ %s (fallback)\n", whisperPath)
	} else if _, err := os.Stat("/opt/homebrew/bin/whisper"); err == nil {
		fmt.Println("✅ /opt/homebrew/bin/whisper (fallback)")
	} else {
		fmt.Println("⚠️  not configured (optional, for voice messages)")
		fmt.Println("   Set transcription_cmd in ~/.ccc.json or install whisper")
	}

	fmt.Println()
	if allGood {
		fmt.Println("✅ All checks passed!")
	} else {
		fmt.Println("❌ Some issues found. Fix them and run 'ccc doctor' again.")
	}
}

// Send notification (only if away)

func send(message string) error {
	config, err := loadConfig()
	if err != nil {
		return fmt.Errorf("not configured. Run: ccc setup <bot_token>")
	}

	if !config.Away {
		fmt.Println("Away mode off, skipping notification.")
		return nil
	}

	// Try to send to session topic if we're in a session directory
	if config.GroupID != 0 {
		cwd, _ := os.Getwd()
		for name, info := range config.Sessions {
			if info == nil {
				continue
			}
			if cwd == info.Path || strings.HasPrefix(cwd, info.Path+"/") || strings.HasSuffix(cwd, "/"+name) {
				return sendMessage(config, config.GroupID, info.TopicID, message)
			}
		}
	}

	// Fallback to private chat
	return sendMessage(config, config.ChatID, 0, message)
}

// handleRemoteMessage handles messages forwarded from remote clients via --from flag
func handleRemoteMessage(fromHost string, cwd string, encodedProjectDir string, message string) error {
	// Truncate message for log
	logMsg := message
	if len(logMsg) > 100 {
		logMsg = logMsg[:100] + "..."
	}
	logHook("Remote", "from=%s cwd=%s project=%s msg=%s", fromHost, cwd, encodedProjectDir, logMsg)

	config, err := loadConfig()
	if err != nil {
		logHook("Remote", "ERROR: not configured: %v", err)
		return fmt.Errorf("not configured: %v", err)
	}

	// cwd is passed from remote client via --cwd flag
	if cwd == "" {
		logHook("Remote", "ERROR: missing --cwd parameter")
		return fmt.Errorf("missing --cwd parameter")
	}

	// Resolve actual project path from encoded project dir
	// This handles cases where Claude cd'd into a subdirectory
	projectPath := cwd
	if encodedProjectDir != "" {
		if resolved := resolveProjectPathFromTranscript(encodedProjectDir, cwd); resolved != "" {
			projectPath = resolved
			fmt.Printf("[remote] resolved project path: %s (from cwd=%s)\n", projectPath, cwd)
		}
	}

	// Find session matching fromHost and path
	for name, info := range config.Sessions {
		if info == nil {
			continue
		}
		// Check if this is a session from the specified host
		if info.Host != fromHost {
			continue
		}
		// Check if path matches (use resolved projectPath)
		if info.Path == projectPath {
			// Skip prompt messages that were just sent from Telegram (cooldown 10s)
			if strings.HasPrefix(message, "💬") && wasTelegramSent(info.TopicID) {
				logHook("Remote", "skipping prompt (telegram cooldown) session=%s topic=%d", name, info.TopicID)
				return nil
			}
			logHook("Remote", "matched session=%s topic=%d, sending", name, info.TopicID)
			fmt.Printf("[remote] from=%s session=%s\n", fromHost, name)
			// Store forwarded message in history (with dedup)
			histFrom, histText := parseRemoteMessagePrefix(message)
			appendHistoryDedup(info.TopicID, histFrom, histText)
			return sendMessage(config, config.GroupID, info.TopicID, message)
		}
	}

	// No matching session found - auto-create topic (fallback for client-initiated sessions)
	logHook("Remote", "no session for path=%s, creating topic", projectPath)
	fmt.Printf("[remote] from=%s no session for path=%s, creating topic\n", fromHost, projectPath)

	// Generate session name: host:projectDir
	fullName := fromHost + ":" + filepath.Base(projectPath)

	topicID, err := getOrCreateTopic(config, fullName, projectPath, fromHost)
	if err != nil {
		// Fallback to private chat if topic creation fails
		fmt.Fprintf(os.Stderr, "Failed to create topic: %v\n", err)
		return sendMessage(config, config.ChatID, 0, fmt.Sprintf("[%s] %s", fromHost, message))
	}

	fmt.Printf("[remote] created/reused topic %d for session %s\n", topicID, fullName)
	// Store forwarded message in history (with dedup)
	histFrom, histText := parseRemoteMessagePrefix(message)
	appendHistoryDedup(topicID, histFrom, histText)
	return sendMessage(config, config.GroupID, topicID, message)
}

// parseRemoteMessagePrefix determines the sender and clean text from a
// forwarded remote message. Messages from client-mode hooks have prefixes:
//   - "✅ sessionName\n\n..." → from claude (stop hook = response)
//   - "💬 ..." → from human (prompt hook)
//   - everything else → from claude (output hook / tool output)
func parseRemoteMessagePrefix(message string) (from string, text string) {
	if strings.HasPrefix(message, "✅") {
		// Stop hook: "✅ sessionName\n\n<response>"
		if idx := strings.Index(message, "\n\n"); idx != -1 {
			return "claude", message[idx+2:]
		}
		return "claude", message
	}
	if strings.HasPrefix(message, "💬") {
		// Prompt hook: "💬 <user message>"
		text = strings.TrimPrefix(message, "💬 ")
		text = strings.TrimPrefix(text, "💬")
		return "human", strings.TrimSpace(text)
	}
	// Output hook or other → claude
	return "claude", message
}

// appendHistoryDedup stores a message in history, but skips if the last
// message with the same "from" already has identical text. This prevents
// duplicates when both handleAskCmd (inline) and handleRemoteMessage
// (stop hook forwarding) store the same response.
func appendHistoryDedup(topicID int64, from string, text string) {
	msgs, err := readHistory(topicID, 0, 1, from)
	if err == nil && len(msgs) > 0 && msgs[len(msgs)-1].Text == text {
		fmt.Printf("[history] dedup: skipping duplicate %s message for topic=%d\n", from, topicID)
		return
	}
	appendHistory(topicID, HistoryMessage{
		ID:        nextMessageID(),
		Timestamp: time.Now().Unix(),
		From:      from,
		Text:      text,
	})
}

// handleHostCommand handles /host subcommands
func handleHostCommand(config *Config, chatID int64, threadID int64, text string) {
	args := strings.Fields(text)
	if len(args) < 2 {
		sendMessage(config, chatID, threadID, `Host management commands:
/host add <name> <address> [projects_dir]
/host set <name> <address>
/host del <name>
/host list
/host check <name>`)
		return
	}

	subCmd := args[1]

	switch subCmd {
	case "add":
		// /host add <name> <address> [projects_dir]
		if len(args) < 4 {
			sendMessage(config, chatID, threadID, "Usage: /host add <name> <address> [projects_dir]\nExample: /host add laptop wlad@192.168.1.50 ~/Dev")
			return
		}
		name := args[2]
		address := args[3]
		projectsDir := "~"
		if len(args) >= 5 {
			projectsDir = args[4]
		}

		// Check if host already exists
		if config.Hosts != nil {
			if _, exists := config.Hosts[name]; exists {
				sendMessage(config, chatID, threadID, fmt.Sprintf("❌ Host '%s' already exists. Use /host set to update.", name))
				return
			}
		}

		sendMessage(config, chatID, threadID, fmt.Sprintf("🔄 Checking connection to %s...", address))

		// Check SSH connection
		if err := sshCheckConnection(address); err != nil {
			sendMessage(config, chatID, threadID, fmt.Sprintf("❌ Cannot connect to %s: %v\nCheck SSH key setup.", address, err))
			return
		}

		// Check tmux
		tmuxPath, err := sshCheckCommand(address, "tmux")
		if err != nil {
			sendMessage(config, chatID, threadID, fmt.Sprintf("⚠️ tmux not found on %s", name))
			tmuxPath = "not found"
		}

		// Check claude
		claudePath, err := sshCheckCommand(address, "claude")
		if err != nil {
			sendMessage(config, chatID, threadID, fmt.Sprintf("⚠️ claude not found on %s", name))
			claudePath = "not found"
		}

		// Save host
		if config.Hosts == nil {
			config.Hosts = make(map[string]*HostInfo)
		}
		config.Hosts[name] = &HostInfo{
			Address:     address,
			ProjectsDir: projectsDir,
		}
		saveConfig(config)

		msg := fmt.Sprintf(`✅ Host '%s' added!

Address: %s
Projects dir: %s
tmux: %s
claude: %s`, name, address, projectsDir, tmuxPath, claudePath)
		sendMessage(config, chatID, threadID, msg)

	case "set":
		// /host set <name> <address>
		if len(args) < 4 {
			sendMessage(config, chatID, threadID, "Usage: /host set <name> <address>")
			return
		}
		name := args[2]
		address := args[3]

		if config.Hosts == nil || config.Hosts[name] == nil {
			sendMessage(config, chatID, threadID, fmt.Sprintf("❌ Host '%s' not found", name))
			return
		}

		sendMessage(config, chatID, threadID, fmt.Sprintf("🔄 Checking connection to %s...", address))

		// Check SSH connection
		if err := sshCheckConnection(address); err != nil {
			sendMessage(config, chatID, threadID, fmt.Sprintf("❌ Cannot connect to %s: %v", address, err))
			return
		}

		config.Hosts[name].Address = address
		saveConfig(config)
		sendMessage(config, chatID, threadID, fmt.Sprintf("✅ Host '%s' updated to %s", name, address))

	case "del":
		// /host del <name>
		if len(args) < 3 {
			sendMessage(config, chatID, threadID, "Usage: /host del <name>")
			return
		}
		name := args[2]

		if config.Hosts == nil || config.Hosts[name] == nil {
			sendMessage(config, chatID, threadID, fmt.Sprintf("❌ Host '%s' not found", name))
			return
		}

		// Check if there are active sessions on this host
		for sessName, info := range config.Sessions {
			if info != nil && info.Host == name {
				sendMessage(config, chatID, threadID, fmt.Sprintf("❌ Cannot delete: session '%s' uses this host", sessName))
				return
			}
		}

		delete(config.Hosts, name)
		saveConfig(config)
		sendMessage(config, chatID, threadID, fmt.Sprintf("✅ Host '%s' deleted", name))

	case "list":
		// /host list
		if config.Hosts == nil || len(config.Hosts) == 0 {
			sendMessage(config, chatID, threadID, "No hosts configured.\nUse /host add <name> <address> to add one.")
			return
		}

		var lines []string
		for name, info := range config.Hosts {
			lines = append(lines, fmt.Sprintf("• %s → %s (%s)", name, info.Address, info.ProjectsDir))
		}
		sendMessage(config, chatID, threadID, "Configured hosts:\n"+strings.Join(lines, "\n"))

	case "check":
		// /host check <name>
		if len(args) < 3 {
			sendMessage(config, chatID, threadID, "Usage: /host check <name>")
			return
		}
		name := args[2]

		if config.Hosts == nil || config.Hosts[name] == nil {
			sendMessage(config, chatID, threadID, fmt.Sprintf("❌ Host '%s' not found", name))
			return
		}

		host := config.Hosts[name]
		sendMessage(config, chatID, threadID, fmt.Sprintf("🔄 Checking %s (%s)...", name, host.Address))

		var results []string

		// Check SSH
		if err := sshCheckConnection(host.Address); err != nil {
			results = append(results, fmt.Sprintf("❌ SSH connection: %v", err))
		} else {
			results = append(results, "✅ SSH connection: OK")
		}

		// Check tmux
		if tmuxPath, err := sshCheckCommand(host.Address, "tmux"); err != nil {
			results = append(results, "❌ tmux: not found")
		} else {
			results = append(results, fmt.Sprintf("✅ tmux: %s", tmuxPath))
		}

		// Check claude
		if claudePath, err := sshCheckCommand(host.Address, "claude"); err != nil {
			results = append(results, "❌ claude: not found")
		} else {
			results = append(results, fmt.Sprintf("✅ claude: %s", claudePath))
		}

		// Check projects_dir
		if sshDirExists(host.Address, host.ProjectsDir) {
			results = append(results, fmt.Sprintf("✅ projects_dir: %s (exists)", host.ProjectsDir))
		} else {
			results = append(results, fmt.Sprintf("⚠️ projects_dir: %s (will be created)", host.ProjectsDir))
		}

		sendMessage(config, chatID, threadID, strings.Join(results, "\n"))

	default:
		sendMessage(config, chatID, threadID, fmt.Sprintf("Unknown subcommand: %s\nUse /host for help.", subCmd))
	}
}

// Main listen loop

func listen() error {
	// Kill any other ccc listen instances to avoid Telegram API conflicts
	myPid := os.Getpid()
	cmd := exec.Command("pgrep", "-f", "ccc listen")
	output, _ := cmd.Output()
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if pid, err := strconv.Atoi(line); err == nil && pid != myPid {
			syscall.Kill(pid, syscall.SIGTERM)
		}
	}

	config, err := loadConfig()
	if err != nil {
		return fmt.Errorf("not configured. Run: ccc setup <bot_token>")
	}

	fmt.Printf("Bot listening... (chat: %d, group: %d)\n", config.ChatID, config.GroupID)
	fmt.Printf("Active sessions: %d\n", len(config.Sessions))
	fmt.Println("Press Ctrl+C to stop")

	// Initialize message ID counter from history
	initMessageIDCounter()

	// Start Unix socket API server
	if err := startSocketServer(config); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to start API socket: %v\n", err)
	}

	setBotCommands(config.BotToken)

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	offset := 0
	client := &http.Client{Timeout: 35 * time.Second}

	go func() {
		<-sigChan
		fmt.Println("\nShutting down...")
		stopSocketServer()
		os.Exit(0)
	}()

	for {
		reqURL := fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates?offset=%d&timeout=30", config.BotToken, offset)
		resp, err := client.Get(reqURL)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Network error: %v (retrying...)\n", err)
			time.Sleep(5 * time.Second)
			continue
		}

		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		var updates TelegramUpdate
		if err := json.Unmarshal(body, &updates); err != nil {
			fmt.Fprintf(os.Stderr, "Parse error: %v\n", err)
			time.Sleep(time.Second)
			continue
		}

		if !updates.OK {
			fmt.Fprintf(os.Stderr, "Telegram API error: %s\n", updates.Description)
			time.Sleep(5 * time.Second)
			continue
		}

		for _, update := range updates.Result {
			offset = update.UpdateID + 1

			// Handle callback queries (button presses)
			if update.CallbackQuery != nil {
				cb := update.CallbackQuery
				// Only accept from authorized user
				if cb.From.ID != config.ChatID {
					continue
				}

				answerCallbackQuery(config, cb.ID)

				// Parse callback data: session:questionIndex:optionIndex
				parts := strings.Split(cb.Data, ":")
				if len(parts) == 3 {
					sessionName := parts[0]
					// questionIndex := parts[1] // for multi-question support
					optionIndex, _ := strconv.Atoi(parts[2])

					// Edit message to show selection and remove buttons
					if cb.Message != nil {
						originalText := cb.Message.Text
						newText := fmt.Sprintf("%s\n\n✓ Selected option %d", originalText, optionIndex+1)
						editMessageRemoveKeyboard(config, cb.Message.Chat.ID, cb.Message.MessageID, newText)
					}

					tmuxName := tmuxSessionName(sessionName)
					if tmuxSessionExists(tmuxName) {
						// Send arrow down keys to select option, then Enter
						for i := 0; i < optionIndex; i++ {
							tmuxCmd( "send-keys", "-t", tmuxName, "Down").Run()
							time.Sleep(50 * time.Millisecond)
						}
						tmuxCmd( "send-keys", "-t", tmuxName, "Enter").Run()
						fmt.Printf("[callback] Selected option %d for %s\n", optionIndex, sessionName)
					}
				}
				continue
			}

			msg := update.Message

			// Only accept from authorized user
			if msg.From.ID != config.ChatID {
				continue
			}

			chatID := msg.Chat.ID
			threadID := msg.MessageThreadID
			isGroup := msg.Chat.Type == "supergroup"

			// Handle voice messages
			if msg.Voice != nil && isGroup && threadID > 0 {
				config, _ = loadConfig()
				sessionName := getSessionByTopic(config, threadID)
				if sessionName != "" {
					// Get session info to check if remote
					sessionInfo := config.Sessions[sessionName]
					hostName := ""
					if sessionInfo != nil {
						hostName = sessionInfo.Host
					}

					// Extract project name for tmux session
					_, projectName := parseSessionTarget(sessionName)
					tmuxName := tmuxSessionName(extractProjectName(projectName))

					// Check if session is running
					sessionRunning := false
					var address string
					if hostName != "" {
						address = getHostAddress(config, hostName)
						if address != "" {
							sessionRunning = sshTmuxHasSession(address, tmuxName)
						}
					} else {
						sessionRunning = tmuxSessionExists(tmuxName)
					}

					if sessionRunning {
						// Check if Claude is actually running (not crashed to bash)
						sshAddr := ""
						if hostName != "" {
							sshAddr = address
						}
						if !isClaudeRunning(tmuxName, sshAddr) {
							// Auto-restart Claude
							sendMessage(config, chatID, threadID, "🔄 Session interrupted, restarting...")
							if !restartClaudeInSession(tmuxName, sshAddr) {
								sendMessage(config, chatID, threadID, "❌ Failed to restart Claude. Use /continue to restart manually.")
								continue
							}
							sendMessage(config, chatID, threadID, "✅ Session restarted")
						}

						sendMessage(config, chatID, threadID, "🎤 Transcribing...")
						// Download and transcribe
						audioPath := filepath.Join(os.TempDir(), fmt.Sprintf("voice_%d.ogg", time.Now().UnixNano()))
						if err := downloadTelegramFile(config, msg.Voice.FileID, audioPath); err != nil {
							sendMessage(config, chatID, threadID, fmt.Sprintf("❌ Download failed: %v", err))
						} else {
							transcription, err := transcribeAudio(config, audioPath)
							os.Remove(audioPath)
							if err != nil {
								sendMessage(config, chatID, threadID, fmt.Sprintf("❌ Transcription failed: %v", err))
							} else if transcription != "" {
								fmt.Printf("[voice] @%s: %s\n", msg.From.Username, transcription)
								sendMessage(config, chatID, threadID, fmt.Sprintf("📝 %s", transcription))
								// Store in history
								appendHistory(threadID, HistoryMessage{
									ID:            nextMessageID(),
									Timestamp:     time.Now().Unix(),
									From:          "human",
									Type:          "voice",
									Transcription: transcription,
									Username:      msg.From.Username,
								})
								// Start typing indicator and send to appropriate tmux
								startContinuousTyping(config, chatID, threadID, sessionName)
								if hostName != "" {
									sshTmuxSendKeys(address, tmuxName, transcription)
								} else {
									sendToTmux(tmuxName, transcription)
								}
							}
						}
					}
				}
				continue
			}

			// Handle photo messages
			if len(msg.Photo) > 0 && isGroup && threadID > 0 {
				config, _ = loadConfig()
				sessionName := getSessionByTopic(config, threadID)
				if sessionName != "" {
					// Get session info to check if remote
					sessionInfo := config.Sessions[sessionName]
					hostName := ""
					if sessionInfo != nil {
						hostName = sessionInfo.Host
					}

					// Extract project name for tmux session
					_, projectName := parseSessionTarget(sessionName)
					tmuxName := tmuxSessionName(extractProjectName(projectName))

					// Get largest photo (last in array)
					photo := msg.Photo[len(msg.Photo)-1]
					imgPath := filepath.Join(os.TempDir(), fmt.Sprintf("telegram_%d.jpg", time.Now().UnixNano()))
					if err := downloadTelegramFile(config, photo.FileID, imgPath); err != nil {
						sendMessage(config, chatID, threadID, fmt.Sprintf("❌ Download failed: %v", err))
						continue
					}

					caption := msg.Caption
					if caption == "" {
						caption = "Analyze this image:"
					}

					// Handle remote sessions
					if hostName != "" {
						hostInfo := config.Hosts[hostName]
						if hostInfo == nil {
							sendMessage(config, chatID, threadID, fmt.Sprintf("❌ Host %s not found in config", hostName))
							continue
						}

						// Check if Claude is actually running
						if !isClaudeRunning(tmuxName, hostInfo.Address) {
							// Auto-restart Claude
							sendMessage(config, chatID, threadID, "🔄 Session interrupted, restarting...")
							if !restartClaudeInSession(tmuxName, hostInfo.Address) {
								sendMessage(config, chatID, threadID, "❌ Failed to restart Claude. Use /continue to restart manually.")
								continue
							}
							sendMessage(config, chatID, threadID, "✅ Session restarted")
						}

						// SCP file to remote host
						sendMessage(config, chatID, threadID, "📷 Transferring image to remote host...")
						remotePath := imgPath // Use same path on remote
						if err := scpToHost(hostInfo.Address, imgPath, remotePath, 30*time.Second); err != nil {
							sendMessage(config, chatID, threadID, fmt.Sprintf("❌ SCP failed: %v", err))
							continue
						}

						// Send to remote tmux
						prompt := fmt.Sprintf("%s %s", caption, remotePath)
						// Store in history
						appendHistory(threadID, HistoryMessage{
							ID:        nextMessageID(),
							Timestamp: time.Now().Unix(),
							From:      "human",
							Type:      "photo",
							Path:      remotePath,
							Caption:   caption,
							Username:  msg.From.Username,
						})
						startContinuousTyping(config, chatID, threadID, sessionName)
						sshTmuxSendKeys(hostInfo.Address, tmuxName, prompt)
						// Clean up local file
						os.Remove(imgPath)
						continue
					}

					// Local session
					if tmuxSessionExists(tmuxName) {
						// Check if Claude is actually running
						if !isClaudeRunning(tmuxName, "") {
							// Auto-restart Claude
							sendMessage(config, chatID, threadID, "🔄 Session interrupted, restarting...")
							if !restartClaudeInSession(tmuxName, "") {
								sendMessage(config, chatID, threadID, "❌ Failed to restart Claude. Use /continue to restart manually.")
								continue
							}
							sendMessage(config, chatID, threadID, "✅ Session restarted")
						}
						prompt := fmt.Sprintf("%s %s", caption, imgPath)
						// Store in history
						appendHistory(threadID, HistoryMessage{
							ID:        nextMessageID(),
							Timestamp: time.Now().Unix(),
							From:      "human",
							Type:      "photo",
							Path:      imgPath,
							Caption:   caption,
							Username:  msg.From.Username,
						})
						sendMessage(config, chatID, threadID, "📷 Image saved, sending to Claude...")
						startContinuousTyping(config, chatID, threadID, sessionName)
						// Send text first, wait for image to load, then send Enter
						sendToTmuxWithDelay(tmuxName, prompt, 2*time.Second)
					}
				}
				continue
			}

			text := strings.TrimSpace(msg.Text)
			if text == "" {
				continue
			}

			// Strip bot mention from commands (e.g., /ping@botname -> /ping)
			if strings.HasPrefix(text, "/") {
				if idx := strings.Index(text, "@"); idx != -1 {
					spaceIdx := strings.Index(text, " ")
					if spaceIdx == -1 || idx < spaceIdx {
						text = text[:idx] + text[strings.Index(text+" ", " "):]
					}
				}
				text = strings.TrimSpace(text)
			}

			fmt.Printf("[%s] @%s: %s\n", msg.Chat.Type, msg.From.Username, text)

			// Handle commands
			if text == "/help" || text == "/start" {
				helpText := `📚 *CCC Commands*

*Session Management:*
• /new \[host:\]<name> — Create new session
• /new ~/path/name — Create with custom path
• /new — Restart session in current topic
• /continue \[host:\]<name> — Create with history
• /continue — Restart with -c flag
• /kill <name> — Kill session (keeps topic)
• /list — List sessions (🟢 running, ⚪ stopped)
• /status — Show current session details
• /movehere <name> — Move session to this topic

*Remote Hosts:*
• /host add <name> <addr> \[dir\] — Add host
• /host del <name> — Remove host
• /host list — List hosts
• /host check <name> — Check connectivity
• /rc <host> <cmd> — Run command on host

*Settings:*
• /setdir \[host:\]<path> — Set projects directory
• /away — Toggle notifications
• /c <cmd> — Run local command
• /ping — Check bot status`
				sendMessage(config, chatID, threadID, helpText)
				continue
			}

			if text == "/ping" {
				sendMessage(config, chatID, threadID, "pong!")
				continue
			}

			if text == "/away" {
				config.Away = !config.Away
				saveConfig(config)
				if config.Away {
					sendMessage(config, chatID, threadID, "🚶 Away mode ON")
				} else {
					sendMessage(config, chatID, threadID, "🏠 Away mode OFF")
				}
				continue
			}

			// Handle /host commands
			if strings.HasPrefix(text, "/host") {
				handleHostCommand(config, chatID, threadID, text)
				config, _ = loadConfig() // Reload after potential changes
				continue
			}

			if text == "/list" {
				var lines []string

				// List configured sessions with status (skip deleted)
				for name, info := range config.Sessions {
					if info == nil || info.Deleted {
						continue
					}

					// Check if tmux session is running
					_, projectName := parseSessionTarget(name)
					tmuxName := tmuxSessionName(extractProjectName(projectName))

					var status string
					if info.Host != "" {
						// Remote session
						address := getHostAddress(config, info.Host)
						if address != "" && sshTmuxHasSession(address, tmuxName) {
							status = "🟢"
						} else {
							status = "⚪"
						}
					} else {
						// Local session
						if tmuxSessionExists(tmuxName) {
							status = "🟢"
						} else {
							status = "⚪"
						}
					}

					lines = append(lines, fmt.Sprintf("%s %s", status, name))
				}

				if len(lines) == 0 {
					sendMessage(config, chatID, threadID, "No sessions configured")
				} else {
					sendMessage(config, chatID, threadID, "Sessions:\n"+strings.Join(lines, "\n"))
				}
				continue
			}

			// /status - show detailed session info for current topic
			if text == "/status" && isGroup {
				sessionName := getSessionByTopic(config, threadID)
				if sessionName == "" {
					sendMessage(config, chatID, threadID, "❌ No session mapped to this topic")
					continue
				}

				sessionInfo := config.Sessions[sessionName]
				if sessionInfo == nil {
					sendMessage(config, chatID, threadID, "❌ Session info not found")
					continue
				}

				_, projectName := parseSessionTarget(sessionName)
				tmuxName := tmuxSessionName(extractProjectName(projectName))

				var msg strings.Builder
				msg.WriteString(fmt.Sprintf("📊 *Session: %s*\n\n", sessionName))

				// Get tmux session info
				var tmuxInfo *TmuxSessionInfo
				var err error

				if sessionInfo.Host != "" {
					address := getHostAddress(config, sessionInfo.Host)
					if address != "" {
						tmuxInfo, err = sshGetTmuxSessionInfo(address, tmuxName)
						msg.WriteString(fmt.Sprintf("🖥️ Host: %s\n", sessionInfo.Host))
					}
				} else {
					tmuxInfo, err = getTmuxSessionInfo(tmuxName)
					msg.WriteString("🖥️ Host: local\n")
				}

				msg.WriteString(fmt.Sprintf("📁 Path: %s\n", sessionInfo.Path))

				if err != nil || tmuxInfo == nil {
					msg.WriteString("\n⚪ Status: stopped\n")
				} else {
					msg.WriteString("\n🟢 Status: running\n")
					msg.WriteString(fmt.Sprintf("📂 CWD: %s\n", tmuxInfo.Path))

					now := time.Now()
					uptime := now.Sub(tmuxInfo.Created)
					idle := now.Sub(tmuxInfo.Activity)

					msg.WriteString(fmt.Sprintf("⏱️ Uptime: %s\n", formatDuration(uptime)))
					msg.WriteString(fmt.Sprintf("💤 Idle: %s\n", formatDuration(idle)))
					msg.WriteString(fmt.Sprintf("🕐 Started: %s\n", tmuxInfo.Created.Format("2006-01-02 15:04")))
				}

				sendMessage(config, chatID, threadID, msg.String())
				continue
			}

			// /screenshot - capture last 50 lines from tmux session
			if text == "/screenshot" && isGroup {
				sessionName := getSessionByTopic(config, threadID)
				if sessionName == "" {
					sendMessage(config, chatID, threadID, "❌ No session mapped to this topic")
					continue
				}

				sessionInfo := config.Sessions[sessionName]
				if sessionInfo == nil {
					sendMessage(config, chatID, threadID, "❌ Session info not found")
					continue
				}

				_, projectName := parseSessionTarget(sessionName)
				tmuxName := tmuxSessionName(extractProjectName(projectName))

				var sshAddress string
				if sessionInfo.Host != "" {
					sshAddress = getHostAddress(config, sessionInfo.Host)
					if sshAddress == "" {
						sendMessage(config, chatID, threadID, "❌ Host not found: "+sessionInfo.Host)
						continue
					}
				}

				content, err := captureTmuxPane(tmuxName, sshAddress, 50)
				if err != nil {
					sendMessage(config, chatID, threadID, fmt.Sprintf("❌ Failed to capture: %v", err))
					continue
				}

				if content == "" {
					sendMessage(config, chatID, threadID, "📸 (empty screen)")
					continue
				}

				// Send as monospace code block
				// Truncate repeating characters for cleaner display
				content = truncateRepeatingCharsInLines(content)
				sendMessage(config, chatID, threadID, fmt.Sprintf("📸 Last 50 lines:\n```\n%s\n```", content))
				continue
			}

			if strings.HasPrefix(text, "/setdir") {
				arg := strings.TrimSpace(strings.TrimPrefix(text, "/setdir"))
				if arg == "" {
					// Show current projects directories
					var msg strings.Builder
					msg.WriteString(fmt.Sprintf("📁 Local projects directory: %s\n", getProjectsDir(config)))
					if config.Hosts != nil && len(config.Hosts) > 0 {
						msg.WriteString("\n📁 Remote hosts:\n")
						for hostName, hostInfo := range config.Hosts {
							dir := hostInfo.ProjectsDir
							if dir == "" {
								dir = "~ (default)"
							}
							msg.WriteString(fmt.Sprintf("  %s: %s\n", hostName, dir))
						}
					}
					msg.WriteString("\nUsage: /setdir ~/path or /setdir host:~/path")
					sendMessage(config, chatID, threadID, msg.String())
				} else {
					// Parse host:path format
					hostName, dirPath := parseSessionTarget(arg)

					if hostName != "" {
						// Set for remote host
						if config.Hosts == nil || config.Hosts[hostName] == nil {
							sendMessage(config, chatID, threadID, fmt.Sprintf("❌ Host '%s' not found. Use /host add to configure it.", hostName))
							continue
						}
						config.Hosts[hostName].ProjectsDir = dirPath
						saveConfig(config)
						sendMessage(config, chatID, threadID, fmt.Sprintf("✅ Projects directory for %s set to: %s", hostName, dirPath))
					} else {
						// Set for local
						config.ProjectsDir = arg
						saveConfig(config)
						resolvedPath := getProjectsDir(config)
						sendMessage(config, chatID, threadID, fmt.Sprintf("✅ Projects directory set to: %s", resolvedPath))
					}
				}
				continue
			}

			if strings.HasPrefix(text, "/kill ") {
				name := strings.TrimPrefix(text, "/kill ")
				name = strings.TrimSpace(name)
				if err := killSession(config, name); err != nil {
					sendMessage(config, chatID, threadID, fmt.Sprintf("❌ %v", err))
				} else {
					sendMessage(config, chatID, threadID, fmt.Sprintf("🗑️ Session '%s' killed", name))
					config, _ = loadConfig()
				}
				continue
			}

			// /movehere <session> - move session to current topic (fix duplicates)
			if strings.HasPrefix(text, "/movehere ") {
				name := strings.TrimPrefix(text, "/movehere ")
				name = strings.TrimSpace(name)

				info, exists := config.Sessions[name]
				if !exists {
					sendMessage(config, chatID, threadID, fmt.Sprintf("❌ Session '%s' not found", name))
					continue
				}

				oldTopicID := info.TopicID
				if oldTopicID == threadID {
					sendMessage(config, chatID, threadID, fmt.Sprintf("ℹ️ Session '%s' is already in this topic", name))
					continue
				}

				// Rename current topic to session name
				if err := editForumTopic(config, threadID, name); err != nil {
					sendMessage(config, chatID, threadID, fmt.Sprintf("⚠️ Could not rename topic: %v", err))
				}

				// Update session to point to current topic
				info.TopicID = threadID
				info.Deleted = false
				if err := saveConfig(config); err != nil {
					sendMessage(config, chatID, threadID, fmt.Sprintf("❌ Failed to save: %v", err))
					continue
				}

				// Try to delete the old topic
				deleteErr := deleteForumTopic(config, oldTopicID)
				if deleteErr != nil {
					sendMessage(config, chatID, threadID, fmt.Sprintf("✅ Session '%s' moved here\n⚠️ Old topic %d not deleted: %v", name, oldTopicID, deleteErr))
				} else {
					sendMessage(config, chatID, threadID, fmt.Sprintf("✅ Session '%s' moved here\n🗑️ Old topic deleted", name))
				}
				config, _ = loadConfig()
				continue
			}

			if strings.HasPrefix(text, "/c ") {
				cmdStr := strings.TrimPrefix(text, "/c ")
				output, err := executeCommand(cmdStr)
				if err != nil {
					output = fmt.Sprintf("⚠️ %s\n\nExit: %v", output, err)
				}
				sendMessage(config, chatID, threadID, output)
				continue
			}

			// /rc <host> <cmd> - remote command
			if strings.HasPrefix(text, "/rc ") {
				remainder := strings.TrimSpace(strings.TrimPrefix(text, "/rc "))
				parts := strings.SplitN(remainder, " ", 2)
				if len(parts) < 2 || parts[0] == "" {
					sendMessage(config, chatID, threadID, "Usage: /rc <host> <command>")
					continue
				}
				hostName := parts[0]
				cmdStr := strings.TrimSpace(parts[1])

				// Get host address
				if config.Hosts == nil || config.Hosts[hostName] == nil {
					sendMessage(config, chatID, threadID, fmt.Sprintf("❌ Host '%s' not found. Use /host add to configure it.", hostName))
					continue
				}
				address := config.Hosts[hostName].Address

				output, err := sshRunCommand(address, cmdStr, 30*time.Second)
				if err != nil {
					output = fmt.Sprintf("⚠️ %s\n\nExit: %v", output, err)
				}
				if output == "" {
					output = "(no output)"
				}
				sendMessage(config, chatID, threadID, fmt.Sprintf("📤 %s:\n%s", hostName, output))
				continue
			}

			// /new and /continue commands - create/restart session
			isNewCmd := strings.HasPrefix(text, "/new")
			isContinueCmd := strings.HasPrefix(text, "/continue")
			if (isNewCmd || isContinueCmd) && isGroup {
				config, _ = loadConfig()
				continueSession := isContinueCmd
				var arg string
				if isNewCmd {
					arg = strings.TrimSpace(strings.TrimPrefix(text, "/new"))
				} else {
					arg = strings.TrimSpace(strings.TrimPrefix(text, "/continue"))
				}
				cmdName := "/new"
				if continueSession {
					cmdName = "/continue"
				}

				// /new <name> or /continue <name> - create brand new session + topic
				// Supports host:name format for remote sessions
				if arg != "" {
					// Parse host:name format
					hostName, projectName := parseSessionTarget(arg)

					// Validate host if specified
					if hostName != "" {
						if config.Hosts == nil || config.Hosts[hostName] == nil {
							sendMessage(config, chatID, threadID, fmt.Sprintf("❌ Host '%s' not found. Use /host add to configure it.", hostName))
							continue
						}
					}

					// Build full session name (host:name or just name)
					fullName := fullSessionName(hostName, projectName)

					var topicID int64
					var workDir string

					// Check if session already exists (may be stopped after /kill)
					if existingSession, exists := config.Sessions[fullName]; exists {
						// Reuse existing topic
						topicID = existingSession.TopicID
						workDir = existingSession.Path
					} else {
						// Create new Telegram topic
						var err error
						topicID, err = createForumTopic(config, fullName)
						if err != nil {
							sendMessage(config, chatID, threadID, fmt.Sprintf("❌ Failed to create topic: %v", err))
							continue
						}

						// Resolve work directory path
						workDir, err = resolveSessionPath(config, hostName, projectName)
						if err != nil {
							sendMessage(config, config.GroupID, topicID, fmt.Sprintf("❌ Failed to resolve path: %v", err))
							continue
						}

						// Save mapping with full path
						config.Sessions[fullName] = &SessionInfo{
							TopicID: topicID,
							Path:    workDir,
							Host:    hostName,
						}
						saveConfig(config)
					}

					// Create work directory and tmux session
					tmuxName := tmuxSessionName(extractProjectName(projectName))

					// Kill existing tmux session if running (for restart)
					if hostName != "" {
						address := getHostAddress(config, hostName)
						if sshTmuxHasSession(address, tmuxName) {
							sshTmuxKillSession(address, tmuxName)
							time.Sleep(300 * time.Millisecond)
						}
					} else {
						if tmuxSessionExists(tmuxName) {
							killTmuxSession(tmuxName)
							time.Sleep(300 * time.Millisecond)
						}
					}

					if hostName != "" {
						// Remote session
						address := getHostAddress(config, hostName)

						// Create directory on remote host
						if err := sshMkdir(address, workDir); err != nil {
							sendMessage(config, config.GroupID, topicID, fmt.Sprintf("❌ Failed to create directory: %v", err))
							continue
						}

						// Create tmux session on remote host
						if err := sshTmuxNewSession(address, tmuxName, workDir, continueSession); err != nil {
							sendMessage(config, config.GroupID, topicID, fmt.Sprintf("❌ Failed to start tmux: %v", err))
						} else {
							time.Sleep(500 * time.Millisecond)
							if sshTmuxHasSession(address, tmuxName) {
								sendMessage(config, config.GroupID, topicID, fmt.Sprintf("🚀 Session '%s' started on %s!\n\nSend messages here to interact with Claude.", fullName, hostName))
							} else {
								sendMessage(config, config.GroupID, topicID, fmt.Sprintf("⚠️ Session '%s' created but died immediately. Check if claude works on %s.", fullName, hostName))
							}
						}
					} else {
						// Local session
						if _, err := os.Stat(workDir); os.IsNotExist(err) {
							os.MkdirAll(workDir, 0755)
						}

						if err := createTmuxSession(tmuxName, workDir, continueSession); err != nil {
							sendMessage(config, config.GroupID, topicID, fmt.Sprintf("❌ Failed to start tmux: %v", err))
						} else {
							time.Sleep(500 * time.Millisecond)
							if tmuxSessionExists(tmuxName) {
								sendMessage(config, config.GroupID, topicID, fmt.Sprintf("🚀 Session '%s' started!\n\nSend messages here to interact with Claude.", fullName))
							} else {
								sendMessage(config, config.GroupID, topicID, fmt.Sprintf("⚠️ Session '%s' created but died immediately. Check if ~/bin/ccc works.", fullName))
							}
						}
					}
					continue
				}

				// Without args - restart session in current topic
				if threadID > 0 {
					sessionName := getSessionByTopic(config, threadID)
					if sessionName == "" {
						sendMessage(config, chatID, threadID, fmt.Sprintf("❌ No session mapped to this topic. Use %s <name> to create one.", cmdName))
						continue
					}

					// Get session info to check if remote
					sessionInfo := config.Sessions[sessionName]
					hostName := ""
					if sessionInfo != nil {
						hostName = sessionInfo.Host
					}

					// Extract project name for tmux session (without host prefix)
					_, projectName := parseSessionTarget(sessionName)
					tmuxName := tmuxSessionName(extractProjectName(projectName))

					// Get work directory from stored session info
					workDir := ""
					if sessionInfo != nil && sessionInfo.Path != "" {
						workDir = sessionInfo.Path
					}

					if hostName != "" {
						// Remote session
						address := getHostAddress(config, hostName)
						if address == "" {
							sendMessage(config, chatID, threadID, fmt.Sprintf("❌ Host '%s' not configured", hostName))
							continue
						}

						// Kill existing session if running
						if sshTmuxHasSession(address, tmuxName) {
							sshTmuxKillSession(address, tmuxName)
							time.Sleep(300 * time.Millisecond)
						}

						// Create directory if needed
						if workDir != "" {
							sshMkdir(address, workDir)
						}

						// Create tmux session on remote
						if err := sshTmuxNewSession(address, tmuxName, workDir, continueSession); err != nil {
							sendMessage(config, chatID, threadID, fmt.Sprintf("❌ Failed to start: %v", err))
						} else {
							time.Sleep(500 * time.Millisecond)
							if sshTmuxHasSession(address, tmuxName) {
								action := "restarted"
								if continueSession {
									action = "continued"
								}
								sendMessage(config, chatID, threadID, fmt.Sprintf("🚀 Session '%s' %s on %s", sessionName, action, hostName))
							} else {
								sendMessage(config, chatID, threadID, fmt.Sprintf("⚠️ Session died immediately"))
							}
						}
					} else {
						// Local session
						// Kill existing session if running
						if tmuxSessionExists(tmuxName) {
							killTmuxSession(tmuxName)
							time.Sleep(300 * time.Millisecond)
						}

						// Get work directory
						if workDir == "" {
							workDir = resolveProjectPath(config, sessionName)
						}
						if _, err := os.Stat(workDir); os.IsNotExist(err) {
							os.MkdirAll(workDir, 0755)
						}

						if err := createTmuxSession(tmuxName, workDir, continueSession); err != nil {
							sendMessage(config, chatID, threadID, fmt.Sprintf("❌ Failed to start: %v", err))
						} else {
							time.Sleep(500 * time.Millisecond)
							if tmuxSessionExists(tmuxName) {
								action := "restarted"
								if continueSession {
									action = "continued"
								}
								sendMessage(config, chatID, threadID, fmt.Sprintf("🚀 Session '%s' %s", sessionName, action))
							} else {
								sendMessage(config, chatID, threadID, fmt.Sprintf("⚠️ Session died immediately"))
							}
						}
					}
				} else {
					sendMessage(config, chatID, threadID, fmt.Sprintf("Usage: %s <name> to create a new session", cmdName))
				}
				continue
			}

			// Check if message is in a topic (interactive session)
			if isGroup && threadID > 0 {
				// Reload config to get latest sessions
				config, _ = loadConfig()
				sessionName := getSessionByTopic(config, threadID)
				fmt.Fprintf(os.Stderr, "[msg] threadID=%d sessionName=%q\n", threadID, sessionName)
				if sessionName != "" {
					// Get session info to check if remote
					sessionInfo := config.Sessions[sessionName]
					hostName := ""
					if sessionInfo != nil {
						hostName = sessionInfo.Host
					}

					// Extract project name for tmux session (without host prefix)
					_, projectName := parseSessionTarget(sessionName)
					tmuxName := tmuxSessionName(extractProjectName(projectName))

					if hostName != "" {
						// Remote session
						address := getHostAddress(config, hostName)
						if address == "" {
							sendMessage(config, chatID, threadID, fmt.Sprintf("❌ Host '%s' not configured", hostName))
							continue
						}

						if sshTmuxHasSession(address, tmuxName) {
							// Check if Claude is actually running (not crashed to bash)
							if !isClaudeRunning(tmuxName, address) {
								// Auto-restart Claude
								sendMessage(config, chatID, threadID, "🔄 Session interrupted, restarting...")
								if !restartClaudeInSession(tmuxName, address) {
									sendMessage(config, chatID, threadID, "❌ Failed to restart Claude. Use /continue to restart manually.")
									continue
								}
								sendMessage(config, chatID, threadID, "✅ Session restarted")
							}
							startContinuousTyping(config, chatID, threadID, sessionName)
							// Store in history
							appendHistory(threadID, HistoryMessage{
								ID:        nextMessageID(),
								Timestamp: time.Now().Unix(),
								From:      "human",
								Text:      text,
								Username:  msg.From.Username,
							})
							markTelegramSent(threadID)
							if err := sshTmuxSendKeys(address, tmuxName, text); err != nil {
								stopContinuousTyping(sessionName)
								sendMessage(config, chatID, threadID, fmt.Sprintf("❌ Failed to send: %v", err))
							} else {
								// Start background capture for remote session response
								captureResponseAsync(sessionName, tmuxName, address, threadID)
							}
						} else {
							sendMessage(config, chatID, threadID, "⚠️ Session not running. Use /new or /continue to restart.")
						}
					} else {
						// Local session
						if tmuxSessionExists(tmuxName) {
							// Check if Claude is actually running (not crashed to bash)
							if !isClaudeRunning(tmuxName, "") {
								// Auto-restart Claude
								sendMessage(config, chatID, threadID, "🔄 Session interrupted, restarting...")
								if !restartClaudeInSession(tmuxName, "") {
									sendMessage(config, chatID, threadID, "❌ Failed to restart Claude. Use /continue to restart manually.")
									continue
								}
								sendMessage(config, chatID, threadID, "✅ Session restarted")
							}
							startContinuousTyping(config, chatID, threadID, sessionName)
							// Store in history
							appendHistory(threadID, HistoryMessage{
								ID:        nextMessageID(),
								Timestamp: time.Now().Unix(),
								From:      "human",
								Text:      text,
								Username:  msg.From.Username,
							})
							markTelegramSent(threadID)
							if err := sendToTmux(tmuxName, text); err != nil {
								stopContinuousTyping(sessionName)
								sendMessage(config, chatID, threadID, fmt.Sprintf("❌ Failed to send: %v", err))
							}
						} else {
							sendMessage(config, chatID, threadID, "⚠️ Session not running. Use /new or /continue to restart.")
						}
					}
					continue
				}
			}

			// Private chat: run one-shot Claude
			if !isGroup {
				sendMessage(config, chatID, threadID, "🤖 Running Claude...")

				prompt := text
				if msg.ReplyToMessage != nil && msg.ReplyToMessage.Text != "" {
					origText := msg.ReplyToMessage.Text
					origWords := strings.Fields(origText)
					if len(origWords) > 0 {
						home, _ := os.UserHomeDir()
						potentialDir := filepath.Join(home, origWords[0])
						if info, err := os.Stat(potentialDir); err == nil && info.IsDir() {
							prompt = origWords[0] + " " + text
						}
					}
					prompt = fmt.Sprintf("Original message:\n%s\n\nReply:\n%s", origText, prompt)
				}

				go func(p string, cid int64) {
					defer func() {
						if r := recover(); r != nil {
							sendMessage(config, cid, 0, fmt.Sprintf("💥 Panic: %v", r))
						}
					}()
					output, err := runClaude(p)
					if err != nil {
						if strings.Contains(err.Error(), "context deadline exceeded") {
							output = fmt.Sprintf("⏱️ Timeout (10min)\n\n%s", output)
						} else {
							output = fmt.Sprintf("⚠️ %s\n\nExit: %v", output, err)
						}
					}
					sendMessage(config, cid, 0, output)
				}(prompt, chatID)
			}
		}
	}
}

func printHelp() {
	fmt.Printf(`ccc - Claude Code Companion v%s

Your companion for Claude Code - control sessions remotely via Telegram and tmux.

USAGE:
    ccc                     Start/attach tmux session in current directory
    ccc -c                  Continue previous session
    ccc <message>           Send notification (if away mode is on)

COMMANDS:
    setup <token>           Complete setup (bot, hook, service - all in one!)
    doctor                  Check all dependencies and configuration
    config                  Show/set configuration values
    config projects-dir <path>  Set base directory for projects
    setgroup                Configure Telegram group for topics (if skipped during setup)
    listen                  Start the Telegram bot listener manually
    install                 Install Claude hook manually
    run                     Run Claude directly (used by tmux sessions)
    hook                    Handle Claude hook (internal)

HOST MANAGEMENT (for remote sessions):
    host add <name> <addr> [dir]  Add remote host
    host del <name>               Remove remote host
    host list                     List configured hosts

CLIENT MODE (for laptops):
    client                  Show client mode config
    client enable           Enable client mode (auto-installs hook)
    client disable          Disable client mode
    client set server <host>  Set server address (user@ip)
    client set name <name>    Set this machine's name

TELEGRAM COMMANDS:
    /help                   Show all commands
    /ping                   Check if bot is alive
    /away                   Toggle away mode (notifications)
    /new [host:]<name>      Create new session (remote or local)
    /new ~/path/name        Create session with custom path
    /new                    Restart session in current topic
    /continue [host:]<name> Create session with conversation history
    /continue               Restart with -c flag in current topic
    /kill <name>            Kill a session (keeps topic)
    /list                   List sessions with status (🟢/⚪)
    /setdir [host:]<path>   Set projects directory
    /c <cmd>                Execute local shell command
    /rc <host> <cmd>        Execute command on remote host
    /host add <name> <addr> [dir]  Add remote host
    /host del <name>        Remove remote host
    /host list              List configured hosts
    /host check <name>      Check host connectivity

FLAGS:
    -h, --help              Show this help
    -v, --version           Show version

For more info: https://github.com/kidandcat/ccc
`, version)
}

func main() {
	// Handle flags
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "-h", "--help", "help":
			printHelp()
			return
		case "-v", "--version", "version":
			fmt.Printf("ccc version %s\n", version)
			return
		}
	}

	if len(os.Args) < 2 {
		// No args: start/attach tmux session with topic
		config, _ := loadOrCreateConfig()
		if config.Mode == "client" && config.Server != "" && config.HostName != "" {
			if err := startClientSession(config, nil); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		} else {
			if err := startSession(false); err != nil {
				os.Exit(1)
			}
		}
		return
	}

	// Check for -c flag (continue) as first arg
	if os.Args[1] == "-c" {
		config, _ := loadOrCreateConfig()
		if config.Mode == "client" && config.Server != "" && config.HostName != "" {
			if err := startClientSession(config, []string{"-c"}); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		} else {
			if err := startSession(true); err != nil {
				os.Exit(1)
			}
		}
		return
	}

	switch os.Args[1] {
	case "run":
		// Run claude directly (used inside tmux sessions)
		continueSession := len(os.Args) > 2 && os.Args[2] == "-c"
		if err := runClaudeRaw(continueSession); err != nil {
			os.Exit(1)
		}
		return
	case "setup":
		if len(os.Args) < 3 {
			fmt.Println("Usage: ccc setup <bot_token>")
			os.Exit(1)
		}
		if err := setup(os.Args[2]); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}

	case "doctor":
		doctor()

	case "config":
		config, err := loadConfig()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		if len(os.Args) < 3 {
			// Show current config
			fmt.Printf("projects_dir: %s\n", getProjectsDir(config))
			fmt.Println("\nUsage: ccc config <key> <value>")
			fmt.Println("  ccc config projects-dir ~/Projects")
			os.Exit(0)
		}
		key := os.Args[2]
		if len(os.Args) < 4 {
			// Show specific key
			switch key {
			case "projects-dir":
				fmt.Println(getProjectsDir(config))
			default:
				fmt.Fprintf(os.Stderr, "Unknown config key: %s\n", key)
				os.Exit(1)
			}
			os.Exit(0)
		}
		value := os.Args[3]
		switch key {
		case "projects-dir":
			config.ProjectsDir = value
			if err := saveConfig(config); err != nil {
				fmt.Fprintf(os.Stderr, "Error saving config: %v\n", err)
				os.Exit(1)
			}
			fmt.Printf("✅ projects_dir set to: %s\n", getProjectsDir(config))
		default:
			fmt.Fprintf(os.Stderr, "Unknown config key: %s\n", key)
			os.Exit(1)
		}

	case "setgroup":
		config, err := loadConfig()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		if err := setGroup(config); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}

	case "listen":
		if err := listen(); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}

	case "hook":
		if err := handleHook(); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}

	case "hook-permission":
		if err := handlePermissionHook(); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}

	case "hook-prompt":
		if err := handlePromptHook(); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}

	case "hook-question":
		if err := handleQuestionHook(); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}

	case "register-session":
		// Internal command: register a session from a remote client
		// Usage: ccc register-session <host> <path>
		// Returns: topic_id on success, error on failure
		if len(os.Args) < 4 {
			fmt.Fprintf(os.Stderr, "Usage: ccc register-session <host> <path>\n")
			os.Exit(1)
		}
		host := os.Args[2]
		path := os.Args[3]

		config, err := loadConfig()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}

		// Generate session name: host:projectDir
		fullName := host + ":" + filepath.Base(path)

		topicID, err := getOrCreateTopic(config, fullName, path, host)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}

		// Output just the topic ID for parsing by client
		fmt.Println(topicID)

	case "hook-output":
		if err := handleOutputHook(); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}

	case "install":
		if err := installHook(); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}

	case "client":
		// Client mode configuration
		config, err := loadOrCreateConfig()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		if len(os.Args) < 3 {
			// Show current config
			fmt.Println("Client mode configuration:")
			fmt.Printf("  mode: %s\n", config.Mode)
			fmt.Printf("  server: %s\n", config.Server)
			fmt.Printf("  host_name: %s\n", config.HostName)
			fmt.Println("\nUsage:")
			fmt.Println("  ccc client set server <user@host>  - Set server address")
			fmt.Println("  ccc client set name <hostname>     - Set this machine's name")
			fmt.Println("  ccc client enable                  - Enable client mode")
			fmt.Println("  ccc client disable                 - Disable client mode")
			os.Exit(0)
		}
		subCmd := os.Args[2]
		switch subCmd {
		case "set":
			if len(os.Args) < 5 {
				fmt.Println("Usage: ccc client set <key> <value>")
				os.Exit(1)
			}
			key, value := os.Args[3], os.Args[4]
			switch key {
			case "server":
				config.Server = value
				saveConfig(config)
				fmt.Printf("✅ Server set to: %s\n", value)
			case "name":
				config.HostName = value
				saveConfig(config)
				fmt.Printf("✅ Host name set to: %s\n", value)
			default:
				fmt.Fprintf(os.Stderr, "Unknown key: %s\n", key)
				os.Exit(1)
			}
		case "enable":
			config.Mode = "client"
			saveConfig(config)
			fmt.Println("✅ Client mode enabled")
			// Install hook automatically
			if err := installHook(); err != nil {
				fmt.Fprintf(os.Stderr, "⚠️  Failed to install hook: %v\n", err)
				fmt.Println("   Run 'ccc install' manually after claude is set up")
			}
			if config.Server == "" || config.HostName == "" {
				fmt.Println("⚠️  Don't forget to set server and name:")
				fmt.Println("   ccc client set server user@server")
				fmt.Println("   ccc client set name laptop")
			}
		case "disable":
			config.Mode = ""
			saveConfig(config)
			fmt.Println("✅ Client mode disabled")
		default:
			fmt.Fprintf(os.Stderr, "Unknown subcommand: %s\n", subCmd)
			os.Exit(1)
		}

	case "host":
		// Host management CLI commands
		config, err := loadOrCreateConfig()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		if len(os.Args) < 3 {
			fmt.Println("Host management commands:")
			fmt.Println("  ccc host add <name> <address> [projects_dir]")
			fmt.Println("  ccc host del <name>")
			fmt.Println("  ccc host list")
			os.Exit(0)
		}
		subCmd := os.Args[2]
		switch subCmd {
		case "add":
			if len(os.Args) < 5 {
				fmt.Println("Usage: ccc host add <name> <address> [projects_dir]")
				fmt.Println("Example: ccc host add laptop wlad@192.168.1.50 ~/Projects")
				os.Exit(1)
			}
			name := os.Args[3]
			address := os.Args[4]
			projectsDir := "~"
			if len(os.Args) >= 6 {
				projectsDir = os.Args[5]
			}
			if config.Hosts == nil {
				config.Hosts = make(map[string]*HostInfo)
			}
			if _, exists := config.Hosts[name]; exists {
				fmt.Fprintf(os.Stderr, "❌ Host '%s' already exists. Use 'ccc host del %s' first.\n", name, name)
				os.Exit(1)
			}
			config.Hosts[name] = &HostInfo{
				Address:     address,
				ProjectsDir: projectsDir,
			}
			saveConfig(config)
			fmt.Printf("✅ Host '%s' added: %s (projects: %s)\n", name, address, projectsDir)
		case "del":
			if len(os.Args) < 4 {
				fmt.Println("Usage: ccc host del <name>")
				os.Exit(1)
			}
			name := os.Args[3]
			if config.Hosts == nil || config.Hosts[name] == nil {
				fmt.Fprintf(os.Stderr, "❌ Host '%s' not found\n", name)
				os.Exit(1)
			}
			delete(config.Hosts, name)
			saveConfig(config)
			fmt.Printf("✅ Host '%s' deleted\n", name)
		case "list":
			if config.Hosts == nil || len(config.Hosts) == 0 {
				fmt.Println("No hosts configured.")
				fmt.Println("Use: ccc host add <name> <address>")
				os.Exit(0)
			}
			fmt.Println("Configured hosts:")
			for name, info := range config.Hosts {
				fmt.Printf("  • %s → %s (%s)\n", name, info.Address, info.ProjectsDir)
			}
		default:
			fmt.Fprintf(os.Stderr, "Unknown subcommand: %s\n", subCmd)
			os.Exit(1)
		}

	default:
		// Check for --from, --cwd, and --project flags (used by client mode to forward messages)
		var fromHost string
		var remoteCwd string
		var remoteProject string
		args := os.Args[1:]
		filteredArgs := []string{}
		for i := 0; i < len(args); i++ {
			if strings.HasPrefix(args[i], "--from=") {
				fromHost = strings.TrimPrefix(args[i], "--from=")
			} else if args[i] == "--from" && i+1 < len(args) {
				fromHost = args[i+1]
				i++ // skip next arg
			} else if strings.HasPrefix(args[i], "--cwd=") {
				remoteCwd = strings.TrimPrefix(args[i], "--cwd=")
			} else if args[i] == "--cwd" && i+1 < len(args) {
				remoteCwd = args[i+1]
				i++ // skip next arg
			} else if strings.HasPrefix(args[i], "--project=") {
				remoteProject = strings.TrimPrefix(args[i], "--project=")
			} else if args[i] == "--project" && i+1 < len(args) {
				remoteProject = args[i+1]
				i++ // skip next arg
			} else {
				filteredArgs = append(filteredArgs, args[i])
			}
		}

		if fromHost != "" {
			// Message from remote client
			message := strings.Join(filteredArgs, " ")
			if err := handleRemoteMessage(fromHost, remoteCwd, remoteProject, message); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		} else {
			// Check if in client mode
			config, _ := loadOrCreateConfig()
			if config.Mode == "client" && config.Server != "" && config.HostName != "" {
				// Client mode: start session
				if err := startClientSession(config, filteredArgs); err != nil {
					fmt.Fprintf(os.Stderr, "Error: %v\n", err)
					os.Exit(1)
				}
			} else {
				// Server/standalone mode: send message
				if err := send(strings.Join(os.Args[1:], " ")); err != nil {
					fmt.Fprintf(os.Stderr, "Error: %v\n", err)
					os.Exit(1)
				}
			}
		}
	}
}
