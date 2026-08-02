package main

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/kidandcat/ccc/internal/config"
	"github.com/kidandcat/ccc/internal/mail"
)

const version = "1.42.3"

// Type aliases for backward compatibility during migration
type SessionInfo = config.SessionInfo
type GroupInfo = config.GroupInfo
type HostInfo = config.HostInfo
type Config = config.Config

// TelegramMessage represents a Telegram message
type TelegramMessage struct {
	MessageID       int   `json:"message_id"`
	MessageThreadID int64 `json:"message_thread_id,omitempty"` // Topic ID
	Chat            struct {
		ID   int64  `json:"id"`
		Type string `json:"type"` // "private", "group", "supergroup"
	} `json:"chat"`
	From struct {
		ID        int64  `json:"id"`
		Username  string `json:"username"`
		FirstName string `json:"first_name"`
	} `json:"from"`
	Text           string            `json:"text"`
	ReplyToMessage *TelegramMessage  `json:"reply_to_message,omitempty"`
	Voice          *TelegramVoice    `json:"voice,omitempty"`
	Photo          []TelegramPhoto   `json:"photo,omitempty"`
	Document       *TelegramDocument `json:"document,omitempty"`
	Caption        string            `json:"caption,omitempty"`
	ReplyMarkup    *struct {
		InlineKeyboard [][]InlineKeyboardButton `json:"inline_keyboard"`
	} `json:"reply_markup,omitempty"`
}

type TelegramVoice struct {
	FileID   string `json:"file_id"`
	Duration int    `json:"duration"`
}

type TelegramDocument struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name"`
	MimeType string `json:"mime_type"`
	FileSize int64  `json:"file_size"`
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
		Questions      []auqQuestion `json:"questions"`
		AllowedPrompts []struct {
			Tool   string `json:"tool"`
			Prompt string `json:"prompt"`
		} `json:"allowedPrompts,omitempty"`
	} `json:"tool_input"`
}

// auqOption / auqQuestion mirror the AskUserQuestion tool_input. Named (not
// anonymous) so the question set can be passed to postAskUserQuestion and
// relayed from a client to the server.
type auqOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}
type auqQuestion struct {
	Question    string      `json:"question"`
	Header      string      `json:"header"`
	MultiSelect bool        `json:"multiSelect"`
	Options     []auqOption `json:"options"`
}

// ============================================================================
// Local API Types and Functions (Unix Socket)
// ============================================================================

// APIRequest represents an incoming request on the Unix socket
type APIRequest struct {
	Cmd           string          `json:"cmd"`                      // ping, sessions, ask, send, history, screenshot, subscribe, questions, answer
	Session       string          `json:"session,omitempty"`        // session name
	Text          string          `json:"text,omitempty"`           // message text
	From          string          `json:"from,omitempty"`           // agent identifier
	After         int64           `json:"after,omitempty"`          // for history: after message_id
	Limit         int             `json:"limit,omitempty"`          // for history: max messages
	FromFilter    string          `json:"from_filter,omitempty"`    // for history: filter by sender (human, claude, api)
	Sessions      []string        `json:"sessions,omitempty"`       // for subscribe: session list
	QuestionIndex int             `json:"question_index,omitempty"` // for answer: which question (0-based)
	OptionIndex   int             `json:"option_index,omitempty"`   // for answer: which option (0-based)
	Cwd           string          `json:"cwd,omitempty"`            // caller working dir (mcp-secretary: trusted identity source)
	Host          string          `json:"host,omitempty"`           // caller machine id ("" = server-local); disambiguates same path on different hosts
	Payload       json.RawMessage `json:"payload,omitempty"`        // command-specific args (mail/agent commands)
	StreamAction  string          `json:"stream_action,omitempty"`  // for "stream": "delta" or "final"
}

// APIResponse represents a response on the Unix socket
type APIResponse struct {
	OK             bool                `json:"ok"`
	Error          string              `json:"error,omitempty"`
	Sessions       []APISessionInfo    `json:"sessions,omitempty"`
	Activity       []ActivityInfo      `json:"activity,omitempty"`
	Response       string              `json:"response,omitempty"`
	MessageID      int64               `json:"message_id,omitempty"`
	Messages       []HistoryMessage    `json:"messages,omitempty"`
	Duration       int64               `json:"duration_ms,omitempty"`
	Version        string              `json:"version,omitempty"`
	UptimeSeconds  int64               `json:"uptime_seconds,omitempty"`
	SessionsActive int                 `json:"sessions_active,omitempty"`
	Questions      *PendingQuestionSet `json:"questions,omitempty"`
	Result         json.RawMessage     `json:"result,omitempty"` // command-specific result (mail/agent commands)
}

// ActivityInfo represents last message summary for a session
type ActivityInfo struct {
	Name          string `json:"name"`
	LastMessageID int64  `json:"lastMessageId"`
	LastMessageTs int64  `json:"lastMessageTs"`
	LastFrom      string `json:"lastFrom,omitempty"`
	LastText      string `json:"lastText,omitempty"`
}

// APIEvent represents a streaming event for subscribe
type APIEvent struct {
	Event   string `json:"event"` // subscribed, message, status
	Session string `json:"session,omitempty"`
	From    string `json:"from,omitempty"` // human, claude, api
	Text    string `json:"text,omitempty"`
	Status  string `json:"status,omitempty"` // active, idle
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
	From          string `json:"from"` // human, claude, api
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
var lockFile *os.File       // kept open to hold flock
var activeCaptures sync.Map // key: session name, prevents concurrent response captures
var updateInProgress int32  // atomic flag to prevent concurrent /update

// processedMessages deduplicates Telegram updates in forum groups.
// Telegram can send two updates with different update_id but the same message_id
// for a single message in a forum topic. We track recently processed message IDs
// to avoid handling the same message twice (e.g. double transcription of voice).
var processedMessages struct {
	sync.Mutex
	ids [64]int // ring buffer of recent message IDs
	pos int
}

// isMessageProcessed returns true if this message_id was already processed.
// If not, it records the ID and returns false.
func isMessageProcessed(messageID int) bool {
	processedMessages.Lock()
	defer processedMessages.Unlock()
	for _, id := range processedMessages.ids {
		if id == messageID && id != 0 {
			return true
		}
	}
	processedMessages.ids[processedMessages.pos] = messageID
	processedMessages.pos = (processedMessages.pos + 1) % len(processedMessages.ids)
	return false
}

// Global message ID counter (in-memory, initialized from history on start)
var (
	messageIDCounter int64
	messageIDMutex   sync.Mutex
)

// Webhook configuration (set when config is loaded)
var (
	webhookMu  sync.RWMutex
	webhookCfg *Config
)
var webhookClient = &http.Client{Timeout: 5 * time.Second}

// telegramFallbackIPs are Telegram HTTPS frontend IPs that serve the Bot API
// (api.telegram.org) — verified to answer getMe end-to-end. We dial them by IP
// when normal DNS resolution yields an address that is blackholed/blocked from
// this host (observed: api.telegram.org resolved only to 149.154.166.110, which
// timed out, while these frontends stayed reachable). TLS/SNI still uses
// api.telegram.org, so the certificate validates regardless of which IP we
// connect to. NOTE: only true HTTPS frontends belong here — MTProto DC IPs
// (e.g. 149.154.175.x, 91.108.56.x) accept TCP:443 but do not serve the Bot
// API, so dialing them would hang the HTTP request.
var telegramFallbackIPs = []string{
	"149.154.167.220:443",
	"149.154.167.99:443",
	"149.154.167.132:443",
	"149.154.167.32:443",
}

// telegramDialContext dials api.telegram.org resiliently: it first tries normal
// DNS resolution, then falls back to known-good Telegram DC IPs if that fails
// (e.g. DNS returns a single blocked IP). For any other host it dials normally.
func telegramDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	d := &net.Dialer{Timeout: 8 * time.Second, KeepAlive: 30 * time.Second}
	conn, err := d.DialContext(ctx, network, addr)
	if err == nil || !strings.Contains(addr, "api.telegram.org") {
		return conn, err
	}
	// DNS-resolved address is unreachable; try the known DC frontends.
	lastErr := err
	for _, ip := range telegramFallbackIPs {
		c, e := d.DialContext(ctx, network, ip)
		if e == nil {
			return c, nil
		}
		lastErr = e
	}
	return nil, lastErr
}

// telegramHTTPClient is used for Telegram Bot API calls. It caps the dial time
// (so a brief network blip fails in ~8s instead of hanging ~30s) and the overall
// request, leaving room for telegramAPI to retry.
var telegramHTTPClient = &http.Client{
	Timeout: 30 * time.Second,
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           telegramDialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	},
}

const (
	telegramMaxAttempts = 3
	telegramRetryBase   = 1 * time.Second
)

func setWebhookConfig(cfg *Config) {
	webhookMu.Lock()
	webhookCfg = cfg
	webhookMu.Unlock()
}

func getWebhookConfig() *Config {
	webhookMu.RLock()
	defer webhookMu.RUnlock()
	return webhookCfg
}

// WebhookPayload is the JSON body sent to webhook endpoints
type WebhookPayload struct {
	Event     string `json:"event"`
	Session   string `json:"session"`
	From      string `json:"from"`
	Timestamp string `json:"timestamp"`
	MessageID int64  `json:"messageId"`
	Preview   string `json:"preview"`
}

// dispatchWebhooks sends a POST to each configured webhook that subscribes to "message" events
func dispatchWebhooks(webhooks []config.WebhookConfig, sessionName string, msg HistoryMessage) {
	preview := msg.Text
	if preview == "" {
		preview = msg.Transcription
	}
	if preview == "" {
		preview = msg.Caption
	}
	if preview == "" && msg.Type != "" {
		preview = "[" + msg.Type + "]"
	}
	if len(preview) > 200 {
		preview = preview[:200]
	}

	payload := WebhookPayload{
		Event:     "message",
		Session:   sessionName,
		From:      msg.From,
		Timestamp: time.Unix(msg.Timestamp, 0).UTC().Format(time.RFC3339),
		MessageID: msg.ID,
		Preview:   preview,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}

	for _, wh := range webhooks {
		hasMessageEvent := false
		for _, ev := range wh.Events {
			if ev == "message" {
				hasMessageEvent = true
				break
			}
		}
		if !hasMessageEvent {
			continue
		}

		req, err := http.NewRequest("POST", wh.URL, bytes.NewReader(body))
		if err != nil {
			fmt.Fprintf(os.Stderr, "webhook: bad request for %s: %v\n", wh.URL, err)
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		if wh.Token != "" {
			req.Header.Set("Authorization", "Bearer "+wh.Token)
		}

		resp, err := webhookClient.Do(req)
		if err != nil {
			fmt.Fprintf(os.Stderr, "webhook: POST %s failed: %v\n", wh.URL, err)
			continue
		}
		resp.Body.Close()
		if resp.StatusCode >= 400 {
			fmt.Fprintf(os.Stderr, "webhook: POST %s returned %d\n", wh.URL, resp.StatusCode)
		}
	}
}

// pendingQuestions stores AskUserQuestion questions awaiting answers.
// Key: session name, Value: *PendingQuestionSet
var pendingQuestions sync.Map

// PendingQuestionOption represents one option in a pending question
type PendingQuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// PendingQuestion represents a single question from AskUserQuestion
type PendingQuestion struct {
	Question    string                  `json:"question"`
	Header      string                  `json:"header"`
	Options     []PendingQuestionOption `json:"options"`
	MultiSelect bool                    `json:"multi_select,omitempty"`
	Answered    bool                    `json:"answered"`
	AnswerIndex int                     `json:"answer_index,omitempty"`
	Selected    []bool                  `json:"selected,omitempty"` // multiSelect: toggled options
}

// PendingQuestionSet represents all questions from one AskUserQuestion call
type PendingQuestionSet struct {
	Session   string            `json:"session"`
	Questions []PendingQuestion `json:"questions"`
	Timestamp int64             `json:"timestamp"`
	TopicID   int64             `json:"-"`
}

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

	if err := json.NewEncoder(f).Encode(msg); err != nil {
		return err
	}

	// Fire webhooks asynchronously
	if cfg := getWebhookConfig(); cfg != nil && len(cfg.Webhooks) > 0 {
		sessionName := getSessionByTopic(cfg, topicID)
		go dispatchWebhooks(cfg.Webhooks, sessionName, msg)
	}

	return nil
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

		// Reload config from disk to pick up sessions created by external
		// processes (hook CLIs, handleRemoteMessage, etc.)
		if freshCfg, err := loadConfig(); err == nil {
			cfg = freshCfg
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
		case "activity":
			handleActivityCmd(encoder, cfg)
		case "typing":
			handleTypingSocketCmd(encoder, cfg, req)
		case "stream":
			handleStreamSocketCmd(encoder, cfg, req)
		case "question":
			handleQuestionSocketCmd(encoder, cfg, req)
		case "sendfile":
			handleSendFileSocketCmd(encoder, cfg, req)
		case "screenshot":
			handleScreenshotCmd(encoder, cfg, req)
		case "questions":
			handleQuestionsCmd(encoder, cfg, req)
		case "answer":
			handleAnswerCmd(encoder, cfg, req)
		case "continue":
			handleContinueCmd(encoder, cfg, req)
		case "subscribe":
			handleSubscribeCmd(conn, encoder, cfg, req)
			return // Subscribe keeps connection open until done
		case "agent.list":
			handleAgentListCmd(encoder, cfg, req)
		case "agent.get":
			handleAgentGetCmd(encoder, cfg, req)
		case "agent.update_self":
			handleAgentUpdateSelfCmd(encoder, cfg, req)
		case "agent.status":
			handleAgentStatusCmd(encoder, cfg, req)
		case "subscribe.idle":
			handleSubscribeIdleCmd(encoder, cfg, req)
		case "mail.send":
			handleMailSendCmd(encoder, cfg, req)
		case "mail.deliver":
			handleMailDeliverCmd(encoder, cfg, req)
		case "mail.ack":
			handleMailAckCmd(encoder, cfg, req)
		case "reminder.add":
			handleReminderAddCmd(encoder, cfg, req)
		case "reminder.list":
			handleReminderListCmd(encoder, cfg, req)
		case "reminder.delete":
			handleReminderDeleteCmd(encoder, cfg, req)
		case "rl.recover":
			handleRateLimitRecover(encoder, cfg, req)
		case "briefing":
			handleBriefingCmd(encoder, cfg, req)
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
		if info == nil {
			continue
		}
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
		sendMessage(cfg, groupChatID(cfg, sessionGroup(info)), info.TopicID, telegramMsg)
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

	// Record timestamp before Claude starts processing.
	// The Stop hook will store Claude's response in history when it finishes.
	sentAt := time.Now()

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
					// Claude is idle. The Stop hook should have stored the response
					// in history already. Poll history to retrieve it.
					duration := time.Since(startTime).Milliseconds()
					response := waitForHistoryResponse(info.TopicID, sentAt, 10*time.Second)

					encoder.Encode(APIResponse{
						OK:       true,
						Response: response,
						Duration: duration,
					})
					return
				}
			} else {
				idleCount = 0
			}
		}
	}
}

// waitForHistoryResponse polls history for a Claude response that arrived after sentAt.
// Used by handleAskCmd to read the response stored by the Stop hook.
func waitForHistoryResponse(topicID int64, sentAt time.Time, maxWait time.Duration) string {
	deadline := time.Now().Add(maxWait)
	for time.Now().Before(deadline) {
		msgs, err := readHistory(topicID, 0, 3, "claude")
		if err == nil {
			for i := len(msgs) - 1; i >= 0; i-- {
				if msgs[i].Timestamp >= sentAt.Unix() {
					return msgs[i].Text
				}
			}
		}
		time.Sleep(1 * time.Second)
	}
	return ""
}

// captureResponseAsync polls a remote session in the background to capture
// Claude's response and store it in history. This is a fallback for cases
// where client-mode forwarding isn't active — if the Stop hook already
// forwarded the response (Fix 1), this is a no-op due to dedup.
func captureResponseAsync(cfg *Config, sessionName string, info *SessionInfo) {
	if info.Host == "" || info.TopicID == 0 {
		return
	}

	// Per-session guard: only one capture goroutine per session
	if _, loaded := activeCaptures.LoadOrStore(sessionName, true); loaded {
		return
	}

	go func() {
		defer activeCaptures.Delete(sessionName)

		_, projectName := parseSessionTarget(sessionName)
		tmuxName := tmuxSessionName(extractProjectName(projectName))
		sshAddr := getHostAddress(cfg, info.Host)
		sentAt := time.Now()

		// Wait for Claude to start processing
		time.Sleep(2 * time.Second)

		// Poll until Claude becomes idle
		timeout := time.After(5 * time.Minute)
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()

		idleCount := 0
		for {
			select {
			case <-timeout:
				fmt.Printf("[capture] timeout waiting for idle session=%s\n", sessionName)
				return
			case <-ticker.C:
				state := checkClaudeState(tmuxName, sshAddr)
				if state == "idle" {
					idleCount++
					if idleCount >= 2 {
						// Claude is idle — check if response already in history (from Stop hook + Fix 1)
						response := waitForHistoryResponse(info.TopicID, sentAt, 5*time.Second)
						if response != "" {
							fmt.Printf("[capture] response already in history session=%s\n", sessionName)
							return
						}

						// No response in history — capture from remote transcript
						response = getRemoteLastResponse(sshAddr, info.Path)
						if response != "" {
							appendHistoryDedup(info.TopicID, "claude", response)
							fmt.Printf("[capture] stored remote response session=%s len=%d\n", sessionName, len(response))
						} else {
							fmt.Printf("[capture] no response captured session=%s\n", sessionName)
						}
						return
					}
				} else {
					idleCount = 0
				}
			}
		}
	}()
}

// getRemoteLastResponse reads the last Claude response from a remote machine's
// transcript file via SSH. Used as a fallback when client-mode forwarding is not active.
func getRemoteLastResponse(sshAddr string, projectPath string) string {
	if sshAddr == "" || projectPath == "" {
		return ""
	}

	// Claude encodes project path by replacing / with -
	encodedPath := strings.ReplaceAll(projectPath, "/", "-")

	// Find the most recent transcript file for this project
	findCmd := fmt.Sprintf(
		"ls -t ~/.claude/projects/%s/*/transcript.jsonl 2>/dev/null | head -1",
		encodedPath,
	)
	result, err := runSSH(sshAddr, findCmd, 10*time.Second)
	if err != nil || strings.TrimSpace(result) == "" {
		return ""
	}
	transcriptPath := strings.TrimSpace(result)

	// Read the last 200KB of the transcript (enough for the last turn)
	readCmd := fmt.Sprintf("tail -c 204800 %s", shellQuote(transcriptPath))
	content, err := runSSH(sshAddr, readCmd, 15*time.Second)
	if err != nil || content == "" {
		return ""
	}

	// Write to temp file and parse with existing getLastAssistantMessage
	tmpFile, err := os.CreateTemp("", "ccc-transcript-*.jsonl")
	if err != nil {
		return ""
	}
	defer os.Remove(tmpFile.Name())
	tmpFile.WriteString(content)
	tmpFile.Close()

	return getLastAssistantMessage(tmpFile.Name())
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
		sendMessage(cfg, groupChatID(cfg, sessionGroup(info)), info.TopicID, telegramMsg)
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

	// Background capture for remote sessions (fallback if client-mode forwarding is inactive)
	captureResponseAsync(cfg, req.Session, info)
}

// handleContinueCmd handles the "continue" command - restarts Claude in a session with -c flag
func handleContinueCmd(encoder *json.Encoder, cfg *Config, req APIRequest) {
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
	workDir := info.Path
	if workDir == "" {
		workDir = resolveProjectPath(cfg, projectName)
	}

	if info.Host != "" {
		// Remote session
		address := getHostAddress(cfg, info.Host)
		if address == "" {
			encoder.Encode(APIResponse{OK: false, Error: "host not configured"})
			return
		}

		// Kill existing tmux session if running
		if sshTmuxHasSession(address, tmuxName) {
			sshTmuxKillSession(address, tmuxName)
			time.Sleep(300 * time.Millisecond)
		}

		// Create directory if needed
		if workDir != "" {
			sshMkdir(address, workDir)
		}

		// Create new tmux session with -c (continue) flag
		if err := sshTmuxNewSession(address, tmuxName, workDir, true); err != nil {
			encoder.Encode(APIResponse{OK: false, Error: fmt.Sprintf("failed to start: %v", err)})
			return
		}

		// Wait for Claude to initialize
		time.Sleep(5 * time.Second)

		if !isClaudeRunning(tmuxName, address) {
			encoder.Encode(APIResponse{OK: false, Error: "session started but Claude failed to initialize"})
			return
		}
	} else {
		// Local session
		// Kill existing tmux session if running
		if tmuxSessionExists(tmuxName) {
			killTmuxSession(tmuxName)
			time.Sleep(300 * time.Millisecond)
		}

		// Ensure work directory exists
		if workDir != "" {
			os.MkdirAll(workDir, 0755)
		}

		// Create new tmux session with -c (continue) flag
		if err := createTmuxSession(tmuxName, workDir, true); err != nil {
			encoder.Encode(APIResponse{OK: false, Error: fmt.Sprintf("failed to start: %v", err)})
			return
		}

		// Wait for Claude to initialize
		time.Sleep(5 * time.Second)

		if !isClaudeRunning(tmuxName, "") {
			encoder.Encode(APIResponse{OK: false, Error: "session started but Claude failed to initialize"})
			return
		}
	}

	// Notify Telegram
	if info.TopicID > 0 {
		agentLabel := req.From
		if agentLabel == "" {
			agentLabel = "api"
		}
		sendMessage(cfg, groupChatID(cfg, sessionGroup(info)), info.TopicID, fmt.Sprintf("🔄 [%s] Session continued", agentLabel))
	}

	encoder.Encode(APIResponse{OK: true})
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

// handleActivityCmd returns last message summary for all sessions
func handleActivityCmd(encoder *json.Encoder, cfg *Config) {
	var activity []ActivityInfo

	for name, info := range cfg.Sessions {
		if info.Deleted {
			continue
		}
		ai := ActivityInfo{Name: name}

		if info.TopicID > 0 {
			if msg := readLastHistoryMessage(info.TopicID); msg != nil {
				ai.LastMessageID = msg.ID
				ai.LastMessageTs = msg.Timestamp
				ai.LastFrom = msg.From
				text := msg.Text
				if text == "" && msg.Transcription != "" {
					text = msg.Transcription
				}
				if text == "" && msg.Caption != "" {
					text = msg.Caption
				}
				if len(text) > 100 {
					text = text[:100] + "..."
				}
				ai.LastText = text
			}
		}

		activity = append(activity, ai)
	}

	encoder.Encode(APIResponse{OK: true, Activity: activity})
}

// readLastHistoryMessage reads the last line from the newest history JSONL file
func readLastHistoryMessage(topicID int64) *HistoryMessage {
	dir := getHistoryDir(topicID)
	files, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil || len(files) == 0 {
		return nil
	}
	// Files are named YYYY-MM-DD-HH.jsonl, lexicographic sort = chronological
	sort.Strings(files)

	// Read last non-empty line from newest file, fall back to older files
	for i := len(files) - 1; i >= 0; i-- {
		if msg := readLastLine(files[i]); msg != nil {
			return msg
		}
	}
	return nil
}

// readLastLine reads the last non-empty JSONL line from a file using tail seek
func readLastLine(path string) *HistoryMessage {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil || stat.Size() == 0 {
		return nil
	}

	// Read up to last 8KB to find the last line
	bufSize := int64(8192)
	if stat.Size() < bufSize {
		bufSize = stat.Size()
	}
	buf := make([]byte, bufSize)
	f.ReadAt(buf, stat.Size()-bufSize)

	// Find last newline-terminated JSON line
	lines := bytes.Split(buf, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 {
			continue
		}
		var msg HistoryMessage
		if json.Unmarshal(line, &msg) == nil && msg.ID > 0 {
			return &msg
		}
	}
	return nil
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

// handleQuestionsCmd returns pending AskUserQuestion questions for a session.
func handleQuestionsCmd(encoder *json.Encoder, cfg *Config, req APIRequest) {
	if req.Session == "" {
		encoder.Encode(APIResponse{OK: false, Error: "session required"})
		return
	}

	// Clean expired questions (older than 5 minutes)
	cleanExpiredQuestions()

	val, exists := pendingQuestions.Load(req.Session)
	if !exists {
		encoder.Encode(APIResponse{OK: true}) // No pending questions
		return
	}

	qs := val.(*PendingQuestionSet)
	encoder.Encode(APIResponse{OK: true, Questions: qs})
}

// handleAnswerCmd answers a pending AskUserQuestion by sending keys to tmux.
func handleAnswerCmd(encoder *json.Encoder, cfg *Config, req APIRequest) {
	if req.Session == "" {
		encoder.Encode(APIResponse{OK: false, Error: "session required"})
		return
	}

	val, exists := pendingQuestions.Load(req.Session)
	if !exists {
		encoder.Encode(APIResponse{OK: false, Error: "no pending questions for this session"})
		return
	}

	qs := val.(*PendingQuestionSet)
	if req.QuestionIndex < 0 || req.QuestionIndex >= len(qs.Questions) {
		encoder.Encode(APIResponse{OK: false, Error: fmt.Sprintf("question_index out of range (0-%d)", len(qs.Questions)-1)})
		return
	}

	q := &qs.Questions[req.QuestionIndex]
	if q.Answered {
		encoder.Encode(APIResponse{OK: false, Error: "question already answered"})
		return
	}

	if req.OptionIndex < 0 || req.OptionIndex >= len(q.Options) {
		encoder.Encode(APIResponse{OK: false, Error: fmt.Sprintf("option_index out of range (0-%d)", len(q.Options)-1)})
		return
	}

	// Resolve session info for tmux
	info, sessionExists := cfg.Sessions[req.Session]
	if !sessionExists || info.Deleted {
		encoder.Encode(APIResponse{OK: false, Error: "session not found"})
		return
	}

	_, projectName := parseSessionTarget(req.Session)
	tmuxName := tmuxSessionName(extractProjectName(projectName))

	// Send tmux keys: Down × optionIndex, then Enter
	var sendErr error
	if info.Host != "" {
		address := getHostAddress(cfg, info.Host)
		for i := 0; i < req.OptionIndex; i++ {
			cmd := fmt.Sprintf("tmux send-keys -t %s Down", shellQuote(tmuxName))
			runSSH(address, cmd, 5*time.Second)
			time.Sleep(50 * time.Millisecond)
		}
		cmd := fmt.Sprintf("tmux send-keys -t %s Enter", shellQuote(tmuxName))
		_, sendErr = runSSH(address, cmd, 5*time.Second)
	} else {
		for i := 0; i < req.OptionIndex; i++ {
			tmuxCmd("send-keys", "-t", tmuxName, "Down").Run()
			time.Sleep(50 * time.Millisecond)
		}
		sendErr = tmuxCmd("send-keys", "-t", tmuxName, "Enter").Run()
	}

	if sendErr != nil {
		encoder.Encode(APIResponse{OK: false, Error: fmt.Sprintf("failed to send keys: %v", sendErr)})
		return
	}

	// Mark as answered
	q.Answered = true
	q.AnswerIndex = req.OptionIndex

	// Auto-submit if this was the last question
	allAnswered := true
	for _, qq := range qs.Questions {
		if !qq.Answered {
			allAnswered = false
			break
		}
	}
	if allAnswered {
		time.Sleep(300 * time.Millisecond)
		if info.Host != "" {
			address := getHostAddress(cfg, info.Host)
			cmd := fmt.Sprintf("tmux send-keys -t %s Enter", shellQuote(tmuxName))
			runSSH(address, cmd, 5*time.Second)
		} else {
			tmuxCmd("send-keys", "-t", tmuxName, "Enter").Run()
		}
		// Remove from pending
		pendingQuestions.Delete(req.Session)
		fmt.Printf("[answer] Auto-submitted all answers for %s\n", req.Session)
	}

	// Store answer in history
	optLabel := q.Options[req.OptionIndex].Label
	appendHistoryDedup(qs.TopicID, "human", fmt.Sprintf("Selected: %s", optLabel))

	encoder.Encode(APIResponse{OK: true, Response: fmt.Sprintf("answered question %d with option %d (%s)", req.QuestionIndex, req.OptionIndex, optLabel)})
}

// cleanExpiredQuestions removes pending questions older than 5 minutes.
func cleanExpiredQuestions() {
	cutoff := time.Now().Unix() - 300
	pendingQuestions.Range(func(key, value interface{}) bool {
		qs := value.(*PendingQuestionSet)
		if qs.Timestamp < cutoff {
			pendingQuestions.Delete(key)
		}
		return true
	})
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

// Config function wrappers - delegate to config package
func getConfigPath() string                              { return config.Path() }
func loadOrCreateConfig() (*Config, error)               { return config.LoadOrCreate() }
func loadConfig() (*Config, error)                       { return config.Load() }
func saveConfig(cfg *Config) error                       { return config.Save(cfg) }
func getProjectsDir(cfg *Config) string                  { return config.GetProjectsDir(cfg) }
func resolveProjectPath(cfg *Config, name string) string { return config.ResolveProjectPath(cfg, name) }

// Telegram API helpers

const maxResponseSize = 10 * 1024 * 1024 // 10MB limit for HTTP response bodies

// redactTokenError replaces the bot token in error messages with "***"
func redactTokenError(err error, token string) error {
	if err == nil || token == "" {
		return err
	}
	return fmt.Errorf("%s", strings.ReplaceAll(err.Error(), token, "***"))
}

// telegramGet performs an HTTP GET and redacts the bot token from any errors
func telegramGet(token string, url string) (*http.Response, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, redactTokenError(err, token)
	}
	return resp, nil
}

// telegramClientGet performs an HTTP GET with a custom client and redacts the bot token
func telegramClientGet(client *http.Client, token string, url string) (*http.Response, error) {
	resp, err := client.Get(url)
	if err != nil {
		return nil, redactTokenError(err, token)
	}
	return resp, nil
}

func telegramAPI(config *Config, method string, params url.Values) (*TelegramResponse, error) {
	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/%s", config.BotToken, method)
	var lastErr error
	for attempt := 0; attempt < telegramMaxAttempts; attempt++ {
		if attempt > 0 {
			// Linear backoff: 1s, 2s. Gives a brief network blip time to recover.
			time.Sleep(time.Duration(attempt) * telegramRetryBase)
		}
		resp, err := telegramHTTPClient.PostForm(apiURL, params)
		if err != nil {
			lastErr = redactTokenError(err, config.BotToken) // transient network error — retry
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
		status := resp.StatusCode
		resp.Body.Close()
		if status >= 500 || status == 429 {
			lastErr = fmt.Errorf("telegram %s: HTTP %d", method, status) // server-side transient — retry
			continue
		}
		var result TelegramResponse
		json.Unmarshal(body, &result)
		return &result, nil
	}
	return nil, lastErr
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

// editMessageKeyboard replaces only the inline keyboard of a message (used to
// reflect multiSelect toggles without touching the message text).
func editMessageKeyboard(config *Config, chatID int64, messageID int, buttons [][]InlineKeyboardButton) {
	keyboardJSON, _ := json.Marshal(map[string]interface{}{"inline_keyboard": buttons})
	params := url.Values{
		"chat_id":      {fmt.Sprintf("%d", chatID)},
		"message_id":   {fmt.Sprintf("%d", messageID)},
		"reply_markup": {string(keyboardJSON)},
	}
	telegramAPI(config, "editMessageReplyMarkup", params)
}

// truncButtonLabel caps an inline-button label to Telegram's practical limit.
func truncButtonLabel(s string) string {
	if len(s) > 120 {
		return s[:117] + "..."
	}
	return s
}

// buildMultiSelectKeyboard renders a multiSelect question's toggle buttons
// (checkbox-prefixed, reflecting q.Selected) plus a Submit row. callback_data:
// toggle = "session:qIdx:total:optIdx:m", submit = "session:qIdx:total:0:x".
func buildMultiSelectKeyboard(sessionName string, qIdx, total int, q PendingQuestion) [][]InlineKeyboardButton {
	var buttons [][]InlineKeyboardButton
	for i, opt := range q.Options {
		if opt.Label == "" {
			continue
		}
		mark := "▫️"
		if i < len(q.Selected) && q.Selected[i] {
			mark = "☑️"
		}
		label := opt.Label
		if opt.Description != "" {
			label += " — " + opt.Description
		}
		buttons = append(buttons, []InlineKeyboardButton{{
			Text:         truncButtonLabel(mark + " " + label),
			CallbackData: fmt.Sprintf("%s:%d:%d:%d:m", sessionName, qIdx, total, i),
		}})
	}
	buttons = append(buttons, []InlineKeyboardButton{{
		Text:         "✅ Submit",
		CallbackData: fmt.Sprintf("%s:%d:%d:0:x", sessionName, qIdx, total),
	}})
	return buttons
}

// multiSelectSubmitKeys returns the TUI key sequence that, from the initial
// AskUserQuestion multiSelect state (cursor on the first option, nothing
// checked), toggles exactly the selected options and submits: Enter toggles the
// current option, Down advances, then Right opens the Submit tab and Enter
// confirms "Submit answers".
func multiSelectSubmitKeys(selected []bool, n int) []string {
	var keys []string
	for i := 0; i < n; i++ {
		if i < len(selected) && selected[i] {
			keys = append(keys, "Enter")
		}
		if i < n-1 {
			keys = append(keys, "Down")
		}
	}
	return append(keys, "Right", "Enter")
}

// parseChoiceCallback splits single-select callback_data into its fields.
// Current format is "<session>:<qIdx>:<total>:<optIdx>"; the pre-total legacy
// format is "<session>:<qIdx>:<optIdx>".
//
// The session name itself may contain ':' (a remote agent is "host:project"),
// so the split has to be right-anchored — parts[0] would yield just the host.
// Both formats end in a fixed number of integers, so prefer the reading whose
// session half is a session we actually know, and fall back to the current
// format otherwise.
func parseChoiceCallback(config *Config, data string) (session string, qIdx, total, optIdx int, ok bool) {
	parts := strings.Split(data, ":")
	n := len(parts)

	// trailing returns the last k parts as ints, and the rest rejoined.
	trailing := func(k int) (string, []int, bool) {
		if n < k+1 {
			return "", nil, false
		}
		vals := make([]int, k)
		for i, s := range parts[n-k:] {
			v, err := strconv.Atoi(s)
			if err != nil {
				return "", nil, false
			}
			vals[i] = v
		}
		return strings.Join(parts[:n-k], ":"), vals, true
	}

	known := func(s string) bool {
		if config == nil {
			return false
		}
		_, exists := config.Sessions[s]
		return exists
	}

	// Preferred: current 3-int format, then legacy 2-int, each confirmed
	// against the session registry so the two can't be confused.
	if s, v, valid := trailing(3); valid && known(s) {
		return s, v[0], v[1], v[2], true
	}
	if s, v, valid := trailing(2); valid && known(s) {
		return s, v[0], 0, v[1], true
	}
	// Unknown session (renamed or removed): still parse, newest format first,
	// so the keystrokes at least reach a live tmux session of that name.
	if s, v, valid := trailing(3); valid {
		return s, v[0], v[1], v[2], true
	}
	if s, v, valid := trailing(2); valid {
		return s, v[0], 0, v[1], true
	}
	return "", 0, 0, 0, false
}

// injectTmuxKeys sends raw key names (e.g. "Down", "Enter", "Right") to a
// session's Claude TUI, local or remote, with a short gap so the TUI registers
// each one. Returns false if the tmux session isn't running.
func injectTmuxKeys(config *Config, sessionName string, keys ...string) bool {
	info, exists := config.Sessions[sessionName]
	// On a remote host the tmux session is named after the project only
	// (claude-<project>), not "claude-host:project".
	_, projectName := parseSessionTarget(sessionName)
	tmuxName := tmuxSessionName(extractProjectName(projectName))
	remote := exists && info.Host != ""
	var address string
	if remote {
		address = getHostAddress(config, info.Host)
		if address == "" || !sshTmuxHasSession(address, tmuxName) {
			return false
		}
	} else if !tmuxSessionExists(tmuxName) {
		return false
	}
	for _, k := range keys {
		if remote {
			runSSH(address, fmt.Sprintf("tmux send-keys -t %s %s", shellQuote(tmuxName), k), 5*time.Second)
		} else {
			tmuxCmd("send-keys", "-t", tmuxName, k).Run()
		}
		time.Sleep(60 * time.Millisecond)
	}
	return true
}

const (
	markOn  = "☑️"
	markOff = "▫️"
)

// toggleCheckmark flips a multiSelect button label's leading checkbox prefix.
func toggleCheckmark(text string) string {
	switch {
	case strings.HasPrefix(text, markOn+" "):
		return markOff + " " + strings.TrimPrefix(text, markOn+" ")
	case strings.HasPrefix(text, markOff+" "):
		return markOn + " " + strings.TrimPrefix(text, markOff+" ")
	}
	return text
}

// cleanOptionLabel strips the checkbox prefix and trailing description from a
// multiSelect button label, leaving just the option label for summaries.
func cleanOptionLabel(text string) string {
	text = strings.TrimPrefix(strings.TrimPrefix(text, markOn+" "), markOff+" ")
	if i := strings.Index(text, " — "); i >= 0 {
		text = text[:i]
	}
	return text
}

// handleMultiSelectCallback handles a button press for a multiSelect question.
// State lives in the Telegram message's own inline keyboard (the ☑️/▫️ prefixes),
// NOT in process memory — the question is registered by a separate hook process,
// so the bot has no shared state. "m" toggles the pressed option's checkbox and
// re-renders; "x" reads the checked set from the keyboard and submits by
// injecting the TUI key sequence (Enter toggles each selected, Down walks the
// list, Right -> Submit tab, Enter).
func handleMultiSelectCallback(config *Config, cb *CallbackQuery, sessionName, action string) {
	if cb.Message == nil || cb.Message.ReplyMarkup == nil {
		// The keyboard IS the state here, so without it there is nothing to
		// toggle or submit — say so instead of returning silently.
		fmt.Fprintf(os.Stderr, "[callback] multiselect %q for %s: no keyboard on the message\n", action, sessionName)
		return
	}
	kb := cb.Message.ReplyMarkup.InlineKeyboard

	switch action {
	case "m": // toggle the pressed option, re-render the keyboard
		for r := range kb {
			for c := range kb[r] {
				if kb[r][c].CallbackData == cb.Data {
					kb[r][c].Text = toggleCheckmark(kb[r][c].Text)
				}
			}
		}
		editMessageKeyboard(config, cb.Message.Chat.ID, cb.Message.MessageID, kb)

	case "x": // submit: derive the checked set from the keyboard, inject keys
		selByIdx := map[int]bool{}
		labelByIdx := map[int]string{}
		maxIdx := -1
		for r := range kb {
			for c := range kb[r] {
				b := kb[r][c]
				if !strings.HasSuffix(b.CallbackData, ":m") {
					continue // skip the Submit button
				}
				p := strings.Split(b.CallbackData, ":")
				idx, _ := strconv.Atoi(p[len(p)-2])
				selByIdx[idx] = strings.HasPrefix(b.Text, markOn)
				labelByIdx[idx] = cleanOptionLabel(b.Text)
				if idx > maxIdx {
					maxIdx = idx
				}
			}
		}
		n := maxIdx + 1
		selected := make([]bool, n)
		var chosen []string
		for i := 0; i < n; i++ {
			selected[i] = selByIdx[i]
			if selected[i] {
				chosen = append(chosen, labelByIdx[i])
			}
		}
		injectTmuxKeys(config, sessionName, multiSelectSubmitKeys(selected, n)...)

		summary := "(nothing)"
		if len(chosen) > 0 {
			summary = strings.Join(chosen, ", ")
		}
		editMessageRemoveKeyboard(config, cb.Message.Chat.ID, cb.Message.MessageID,
			cb.Message.Text+"\n\n☑️ Submitted: "+summary)
		appendHistoryDedup(cb.Message.MessageThreadID, "human", "Selected: "+summary)
	}
}

// ---- Live streaming responses (editMessageText) -------------------------

const (
	streamEditInterval = 3 * time.Second // throttle: ~1 edit / 3s
	telegramTextLimit  = 4096
)

type streamState struct {
	msgID    int
	chatID   int64
	threadID int64
	text     string
	lastEdit time.Time
}

var (
	streamStates = map[string]*streamState{} // session name -> live stream
	streamMu     sync.Mutex
)

// streamDisplayText caps the in-progress text to Telegram's single-message limit.
func streamDisplayText(s string) string {
	if s == "" {
		return "…"
	}
	if len(s) > telegramTextLimit-8 {
		return s[:telegramTextLimit-8] + "\n…"
	}
	return s
}

// sendStreamMessage posts the initial streaming message and returns its id (0 on failure).
func sendStreamMessage(cfg *Config, chatID, threadID int64, text string) int {
	params := url.Values{"chat_id": {fmt.Sprintf("%d", chatID)}, "text": {text}}
	if threadID > 0 {
		params.Set("message_thread_id", fmt.Sprintf("%d", threadID))
	}
	res, err := telegramAPI(cfg, "sendMessage", params)
	if err != nil || res == nil || !res.OK {
		return 0
	}
	var m struct {
		MessageID int `json:"message_id"`
	}
	json.Unmarshal(res.Result, &m)
	return m.MessageID
}

func editStreamMessage(cfg *Config, chatID int64, msgID int, text string) {
	telegramAPI(cfg, "editMessageText", url.Values{
		"chat_id":    {fmt.Sprintf("%d", chatID)},
		"message_id": {fmt.Sprintf("%d", msgID)},
		"text":       {text},
	})
}

// finalizeStream replaces the streaming message with the complete text (the
// first chunk by edit; any overflow chunks as new messages).
func finalizeStream(cfg *Config, st *streamState, final string) {
	if final == "" {
		final = st.text
	}
	chunks := splitMessage(final, telegramTextLimit)
	if len(chunks) == 0 {
		return
	}
	editStreamMessage(cfg, st.chatID, st.msgID, chunks[0])
	for _, c := range chunks[1:] {
		sendMessage(cfg, st.chatID, st.threadID, c)
	}
}

// handleStreamSocketCmd drives a live streaming response from hook processes.
// StreamAction "delta" appends text and throttle-edits; "final" replaces with
// the complete text and clears state. On "final" with no active stream it
// returns Response="nostream" so the Stop hook can send the message normally.
func handleStreamSocketCmd(encoder *json.Encoder, cfg *Config, req APIRequest) {
	session := req.Session
	if session == "" {
		session = resolveSessionByHostCwd(cfg, req.Host, req.Cwd)
	}
	info := cfg.Sessions[session]
	if info == nil {
		encoder.Encode(APIResponse{OK: false, Error: "session not found"})
		return
	}
	// Stream only in live mode with streaming enabled; otherwise tell the caller
	// "nostream" so its Stop hook sends the message normally.
	if !isSessionLive(cfg, info) || info.StreamOff {
		encoder.Encode(APIResponse{OK: true, Response: "nostream"})
		return
	}

	streamMu.Lock()
	defer streamMu.Unlock()
	st := streamStates[session]

	switch req.StreamAction {
	case "delta":
		if req.Text == "" {
			encoder.Encode(APIResponse{OK: true})
			return
		}
		if st == nil {
			st = &streamState{chatID: sessionGroupChatID(cfg, session), threadID: info.TopicID}
			streamStates[session] = st
		}
		st.text += req.Text
		if st.msgID == 0 {
			st.msgID = sendStreamMessage(cfg, st.chatID, st.threadID, streamDisplayText(st.text))
			st.lastEdit = time.Now()
		} else if time.Since(st.lastEdit) >= streamEditInterval {
			editStreamMessage(cfg, st.chatID, st.msgID, streamDisplayText(st.text))
			st.lastEdit = time.Now()
		}
		encoder.Encode(APIResponse{OK: true})
	case "final":
		if st == nil || st.msgID == 0 {
			delete(streamStates, session)
			encoder.Encode(APIResponse{OK: true, Response: "nostream"})
			return
		}
		finalizeStream(cfg, st, req.Text)
		delete(streamStates, session)
		encoder.Encode(APIResponse{OK: true})
	default:
		encoder.Encode(APIResponse{OK: false, Error: "stream_action must be delta|final"})
	}
}

// sendDocument uploads a file to a chat/topic via Telegram's sendDocument
// (multipart). Uses the resilient telegram HTTP client (DC fallback).
func sendDocument(cfg *Config, chatID, threadID int64, filePath, caption, nameOverride string) error {
	f, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer f.Close()

	name := filepath.Base(filePath)
	if nameOverride != "" {
		name = nameOverride
	}

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	w.WriteField("chat_id", fmt.Sprintf("%d", chatID))
	if threadID > 0 {
		w.WriteField("message_thread_id", fmt.Sprintf("%d", threadID))
	}
	if caption != "" {
		w.WriteField("caption", caption)
	}
	part, err := w.CreateFormFile("document", name)
	if err != nil {
		return err
	}
	if _, err := io.Copy(part, f); err != nil {
		return err
	}
	w.Close()

	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/sendDocument", cfg.BotToken)
	req, err := http.NewRequest("POST", apiURL, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := telegramHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	var r struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	json.Unmarshal(body, &r)
	if !r.OK {
		return fmt.Errorf("telegram: %s", r.Description)
	}
	return nil
}

// handleSendFileCmd implements `ccc send-file <path> [caption]`: an agent
// attaches a file to its own Telegram topic. Runs from the agent's cwd, which
// it uses to resolve both the (possibly relative) path and the owning session.
func handleSendFileCmd() error {
	args := os.Args[2:]
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: ccc send-file <path> [caption]")
		os.Exit(1)
	}
	path := args[0]
	caption := strings.Join(args[1:], " ")

	config, err := loadConfig()
	if err != nil {
		return fmt.Errorf("no config: %w", err)
	}

	cwd, _ := os.Getwd()
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	if fi, err := os.Stat(path); err != nil || fi.IsDir() {
		return fmt.Errorf("file not found: %s", path)
	}
	name := filepath.Base(path)

	// Remote agent: upload the file to the server, then have the bot send it.
	if config.Mode == "client" {
		if config.Server == "" || config.HostName == "" {
			return fmt.Errorf("client not configured for relay")
		}
		remote := fmt.Sprintf("/tmp/ccc-upload-%d-%s", os.Getpid(), name)
		if err := scpToHost(config.Server, path, remote, 120*time.Second); err != nil {
			return fmt.Errorf("upload to server: %w", err)
		}
		payload, _ := json.Marshal(map[string]string{"path": remote, "caption": caption, "name": name})
		resp, err := callSocket(config, APIRequest{Cmd: "sendfile", Cwd: cwd, Payload: payload})
		if err != nil {
			return err
		}
		if resp == nil || !resp.OK {
			msg := "unknown error"
			if resp != nil && resp.Error != "" {
				msg = resp.Error
			}
			return fmt.Errorf("server: %s", msg)
		}
		fmt.Printf("✅ Sent %s to your topic\n", name)
		return nil
	}

	var sessionName string
	var topicID int64
	for sname, info := range config.Sessions {
		if info == nil {
			continue
		}
		if cwd == info.Path || strings.HasPrefix(cwd, info.Path+"/") || strings.HasSuffix(cwd, "/"+sname) {
			sessionName, topicID = sname, info.TopicID
			break
		}
	}
	if sessionName == "" || topicID == 0 {
		return fmt.Errorf("no CCC session for cwd=%s", cwd)
	}

	if err := sendDocument(config, sessionGroupChatID(config, sessionName), topicID, path, caption, ""); err != nil {
		return err
	}
	note := "📎 " + name
	if caption != "" {
		note += " — " + caption
	}
	appendHistory(topicID, HistoryMessage{
		ID:        nextMessageID(),
		Timestamp: time.Now().Unix(),
		From:      "claude",
		Text:      note,
	})
	fmt.Printf("✅ Sent %s to topic %d\n", name, topicID)
	return nil
}

// handleSendFileSocketCmd sends a file a remote agent uploaded to the server's
// /tmp, then removes the temp copy. req.Payload = {path, caption, name}.
func handleSendFileSocketCmd(encoder *json.Encoder, cfg *Config, req APIRequest) {
	session := req.Session
	if session == "" {
		session = resolveSessionByHostCwd(cfg, req.Host, req.Cwd)
	}
	info := cfg.Sessions[session]
	if info == nil {
		encoder.Encode(APIResponse{OK: false, Error: "session not found"})
		return
	}
	var p struct{ Path, Caption, Name string }
	if json.Unmarshal(req.Payload, &p) != nil || p.Path == "" {
		encoder.Encode(APIResponse{OK: false, Error: "bad sendfile payload"})
		return
	}
	defer os.Remove(p.Path) // clean the uploaded temp file
	if err := sendDocument(cfg, sessionGroupChatID(cfg, session), info.TopicID, p.Path, p.Caption, p.Name); err != nil {
		encoder.Encode(APIResponse{OK: false, Error: err.Error()})
		return
	}
	note := "📎 " + p.Name
	if p.Caption != "" {
		note += " — " + p.Caption
	}
	appendHistory(info.TopicID, HistoryMessage{ID: nextMessageID(), Timestamp: time.Now().Unix(), From: "claude", Text: note})
	encoder.Encode(APIResponse{OK: true})
}

//go:embed briefing/agent_briefing.md
var agentBriefingTemplate string

// firstLine returns the first line of s, trimmed and capped to max runes.
func firstLine(s string, max int) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > max {
		return strings.TrimSpace(string(r[:max])) + "…"
	}
	return s
}

// handleAgentBriefing prints the CCC environment + tools briefing for the session
// that owns the current working directory. It is the SINGLE source of truth for
// "what environment am I in and what can I do": wired as a SessionStart hook (its
// stdout is injected into the agent's context) and reused on demand by the
// ccc-agent skill. The static body lives in briefing/agent_briefing.md; identity
// and the live agent directory are filled in here.
func handleAgentBriefing() error {
	config, err := loadConfig()
	if err != nil {
		return nil // never block a session start
	}
	cwd, _ := os.Getwd()

	// On a CLIENT the sessions/registry live on the server, so ask the server to
	// build the briefing for this host+cwd and print what it returns. Otherwise
	// the briefing would be empty for every remote agent (the local config has no
	// sessions), and they never learn about send-file / mail / reminders.
	if config.Mode == "client" {
		if resp, err := callSocket(config, APIRequest{Cmd: "briefing", Cwd: cwd}); err == nil && resp != nil && resp.OK {
			fmt.Print(resp.Response)
		}
		return nil
	}

	var sessionName string
	for name, si := range config.Sessions {
		if si == nil {
			continue
		}
		if cwd == si.Path || strings.HasPrefix(cwd, si.Path+"/") || strings.HasSuffix(cwd, "/"+name) {
			sessionName = name
			break
		}
	}
	if sessionName == "" {
		return nil // not a CCC session — inject nothing
	}
	fmt.Print(buildAgentBriefing(config, sessionName))
	return nil
}

// buildAgentBriefing renders the CCC environment+tools briefing for a session.
// Shared by the server-local SessionStart path and the client relay handler.
func buildAgentBriefing(config *Config, sessionName string) string {
	info := config.Sessions[sessionName]
	if info == nil {
		return ""
	}
	// Secretaries follow their own operating manual; skip the ordinary brief.
	if mail.IsSecretary(sessionName) {
		return "# CCC\n\nYou are the CCC secretary for this group — follow your own operating manual (CLAUDE.md). The ordinary-agent briefing below does not apply to you.\n"
	}

	group := sessionGroup(info)
	var b strings.Builder
	b.WriteString("# Your CCC environment\n\n")
	fmt.Fprintf(&b, "You are agent `%s` — Telegram topic %d, group `%s`.\n\n", sessionName, info.TopicID, group)
	b.WriteString(strings.TrimSpace(agentBriefingTemplate))
	b.WriteString("\n")

	// Live agent directory — only agents that published a card (real mail
	// participants), excluding self and the secretary. Kept short: one line each,
	// capped, with a pointer to list_agents. The "default" group is a catch-all
	// of many cardless sessions, so dumping all of them every start would be huge.
	const maxPeers = 15
	var peers []AgentInfo
	for _, a := range buildDirectory(config, group) {
		if a.Name == sessionName || mail.IsSecretary(a.Name) || a.Description == "" {
			continue
		}
		peers = append(peers, a)
	}
	if len(peers) > 0 {
		b.WriteString("\n## Agents you can reach (via `secretary` mail)\n")
		shown := peers
		if len(shown) > maxPeers {
			shown = shown[:maxPeers]
		}
		for _, a := range shown {
			b.WriteString("- `" + a.Name + "` — " + firstLine(a.Description, 100) + "\n")
		}
		if len(peers) > maxPeers {
			fmt.Fprintf(&b, "- …and %d more — use `list_agents` for the full directory.\n", len(peers)-maxPeers)
		}
		b.WriteString("Use `get_agent(name)` for a full card before writing.\n")
	}
	return b.String()
}

// handleBriefingCmd (server socket) returns the briefing for the caller's
// host+cwd, so a client agent gets the same briefing a server-local one does.
func handleBriefingCmd(encoder *json.Encoder, cfg *Config, req APIRequest) {
	name := resolveSessionByHostCwd(cfg, req.Host, req.Cwd)
	if name == "" {
		encoder.Encode(APIResponse{OK: true, Response: ""}) // not a CCC session
		return
	}
	encoder.Encode(APIResponse{OK: true, Response: buildAgentBriefing(cfg, name)})
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
		"❯",                   // Input prompt
		"bypass permissions",  // Status bar
		"shift+tab to cycle",  // Status bar variant
		"ctrl+c to interrupt", // Activity indicator
		"●",                   // Tool marker
		"✽",                   // Spinner
		"✻",                   // Spinner variant
		"⎿",                   // Tool output
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

	// In live integration mode, the turn boundary is known deterministically
	// from the Stop hook (which signals stop via the socket), so we skip the
	// fragile capture-pane "idle" heuristic and just refresh until stopped.
	live := isSessionLive(cfg, cfg.Sessions[sessionName])

	go func() {
		typingTicker := time.NewTicker(4 * time.Second)
		defer typingTicker.Stop()

		// Send initial typing
		sendTypingAction(cfg, chatID, threadID)

		if live {
			for {
				select {
				case <-ctx.Done():
					return
				case <-typingTicker.C:
					sendTypingAction(cfg, chatID, threadID)
				}
			}
		}

		stateTicker := time.NewTicker(2 * time.Second)
		defer stateTicker.Stop()

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

// handleTypingSocketCmd lets a hook process drive the typing indicator inside
// the bot process (live integration mode). req.Text is "start" or "stop".
func handleTypingSocketCmd(encoder *json.Encoder, cfg *Config, req APIRequest) {
	// Hooks may identify the session directly (server-local) or by host+cwd
	// (relayed from a client). The live-mode gate lives here so clients — which
	// don't have the session in their config — don't need to know the mode.
	session := req.Session
	if session == "" {
		session = resolveSessionByHostCwd(cfg, req.Host, req.Cwd)
	}
	info := cfg.Sessions[session]
	if info == nil {
		encoder.Encode(APIResponse{OK: false, Error: "session not found"})
		return
	}
	// Record the agent's work-state edge (start/stop) for status queries and idle
	// subscriptions. Independent of live-mode: the edge signal is meaningful even
	// when the typing indicator is not shown.
	recordAgentWorkEdge(cfg, session, req.Text)
	live := isSessionLive(cfg, info)
	switch req.Text {
	case "start":
		if live {
			startContinuousTyping(cfg, sessionGroupChatID(cfg, session), info.TopicID, session)
		}
	case "stop":
		stopContinuousTyping(session)
	default:
		encoder.Encode(APIResponse{OK: false, Error: "state must be start|stop"})
		return
	}
	resp := APIResponse{OK: true}
	if live {
		resp.Response = "live" // lets a client cache the mode for the turn
	}
	encoder.Encode(resp)
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
	resp, err := telegramGet(config.BotToken, fmt.Sprintf("https://api.telegram.org/bot%s/getFile?file_id=%s", config.BotToken, fileID))
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
	fileResp, err := telegramGet(config.BotToken, fileURL)
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

var topicLocks sync.Map // threadID -> *sync.Mutex

// topicLock returns a per-topic mutex so concurrent handlers for the same topic
// (e.g. two voice messages in a row) stay ordered, while different topics run in
// parallel and the main update loop never blocks.
func topicLock(threadID int64) *sync.Mutex {
	v, _ := topicLocks.LoadOrStore(threadID, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// handleVoiceMessage downloads, transcribes and injects a voice message into its
// agent. Runs in a goroutine (see the listen loop) so its multi-second
// transcription never blocks the serial update loop / command handling.
func handleVoiceMessage(chatID, threadID int64, senderTag, voiceFileID, username string) {
	defer func() { recover() }()
	config, err := loadConfig()
	if err != nil || config == nil {
		return
	}
	sessionName := getSessionByGroupTopic(config, chatID, threadID)
	if sessionName == "" {
		return
	}
	sessionInfo := config.Sessions[sessionName]
	hostName := ""
	if sessionInfo != nil {
		hostName = sessionInfo.Host
	}
	_, projectName := parseSessionTarget(sessionName)
	tmuxName := tmuxSessionName(extractProjectName(projectName))

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
	if !sessionRunning {
		return
	}

	sshAddr := ""
	if hostName != "" {
		sshAddr = address
	}
	if !isClaudeRunning(tmuxName, sshAddr) {
		sendMessage(config, chatID, threadID, "🔄 Session interrupted, restarting...")
		if !restartClaudeInSession(tmuxName, sshAddr) {
			sendMessage(config, chatID, threadID, "❌ Failed to restart Claude. Use /continue to restart manually.")
			return
		}
		sendMessage(config, chatID, threadID, "✅ Session restarted")
	}

	sendMessage(config, chatID, threadID, "🎤 Transcribing...")
	audioPath := filepath.Join(os.TempDir(), fmt.Sprintf("voice_%d.ogg", time.Now().UnixNano()))
	if err := downloadTelegramFile(config, voiceFileID, audioPath); err != nil {
		sendMessage(config, chatID, threadID, fmt.Sprintf("❌ Download failed: %v", err))
		return
	}
	transcription, err := transcribeAudio(config, audioPath)
	os.Remove(audioPath)
	if err != nil {
		sendMessage(config, chatID, threadID, fmt.Sprintf("❌ Transcription failed: %v", err))
		return
	}
	if transcription == "" {
		return
	}
	fmt.Printf("[voice] @%s: %s\n", username, transcription)
	sendMessage(config, chatID, threadID, fmt.Sprintf("📝 %s", transcription))
	appendHistory(threadID, HistoryMessage{
		ID:            nextMessageID(),
		Timestamp:     time.Now().Unix(),
		From:          "human",
		Type:          "voice",
		Transcription: transcription,
		Username:      username,
	})
	markTelegramSent(threadID)
	startContinuousTyping(config, chatID, threadID, sessionName)
	if hostName != "" {
		sshTmuxSendKeys(address, tmuxName, senderTag+transcription)
	} else {
		sendToTmux(tmuxName, senderTag+transcription)
	}
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
		"-o", "ControlMaster=auto",
		"-o", "ControlPath=/tmp/ccc-ssh-%C",
		"-o", "ControlPersist=30s",
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

// runSSHWithInput is runSSH but feeds `input` to the remote command's stdin.
// Used to pass large payloads (e.g. a letter body) without embedding them in the
// command line, which would hit the shell/tmux "too long" limits.
func runSSHWithInput(address string, command string, input string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	wrappedCmd := fmt.Sprintf("bash -i -l -c %s", shellQuote(command))
	cmd := exec.CommandContext(ctx, "ssh",
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "ControlMaster=auto",
		"-o", "ControlPath=/tmp/ccc-ssh-%C",
		"-o", "ControlPersist=30s",
		"-o", fmt.Sprintf("ConnectTimeout=%d", sshConnectTimeout),
		address,
		wrappedCmd,
	)
	cmd.Stdin = strings.NewReader(input)

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

	if len(text) <= tmuxLiteralLimit {
		// Small text: decode inline and type it literally (proven path).
		cmd := fmt.Sprintf(
			"echo %s | base64 -d | xargs -0 tmux send-keys -t %s -l",
			encoded, shellQuote(sessionName),
		)
		if _, err := runSSH(address, cmd, time.Duration(sshCommandTimeout)*time.Second); err != nil {
			return err
		}
	} else {
		// Large text: pipe base64 via stdin (no command-length limit), decode
		// into a tmux buffer (no send-keys "command too long" limit), and paste.
		buf := mailBufName()
		cmd := fmt.Sprintf(
			"base64 -d | tmux load-buffer -b %s - && tmux paste-buffer -b %s -t %s -d",
			shellQuote(buf), shellQuote(buf), shellQuote(sessionName),
		)
		if _, err := runSSHWithInput(address, cmd, encoded, time.Duration(sshCommandTimeout)*time.Second); err != nil {
			return err
		}
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
func getHostProjectsDir(cfg *Config, hostName string) string {
	return config.GetHostProjectsDir(cfg, hostName)
}

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

func createForumTopic(config *Config, chatID int64, name string) (int64, error) {
	if chatID == 0 {
		return 0, fmt.Errorf("no group configured. Add bot to a group with topics enabled and run: ccc setgroup")
	}

	params := url.Values{
		"chat_id": {fmt.Sprintf("%d", chatID)},
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
func editForumTopic(config *Config, chatID int64, topicID int64, name string) error {
	if chatID == 0 {
		return fmt.Errorf("no group configured")
	}

	params := url.Values{
		"chat_id":           {fmt.Sprintf("%d", chatID)},
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
func deleteForumTopic(config *Config, chatID int64, topicID int64) error {
	if chatID == 0 {
		return fmt.Errorf("no group configured")
	}

	params := url.Values{
		"chat_id":           {fmt.Sprintf("%d", chatID)},
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

// closeForumTopic closes (archives) a topic without deleting its messages.
func closeForumTopic(config *Config, chatID int64, topicID int64) error {
	if chatID == 0 {
		return fmt.Errorf("no group configured")
	}
	params := url.Values{
		"chat_id":           {fmt.Sprintf("%d", chatID)},
		"message_thread_id": {fmt.Sprintf("%d", topicID)},
	}
	result, err := telegramAPI(config, "closeForumTopic", params)
	if err != nil {
		return err
	}
	if !result.OK {
		return fmt.Errorf("failed to close topic: %s", result.Description)
	}
	return nil
}

// moveHistory moves a session's stored history from one topic id to another so
// the project's history survives a /changegroup move (ccc-dcp.9).
func moveHistory(oldTopic, newTopic int64) error {
	oldDir := filepath.Dir(getHistoryDir(oldTopic)) // ~/.ccc/history/<oldTopic>
	newDir := filepath.Dir(getHistoryDir(newTopic))
	if _, err := os.Stat(oldDir); err != nil {
		return nil // nothing to move
	}
	if _, err := os.Stat(newDir); err == nil {
		return nil // target already exists, don't clobber
	}
	return os.Rename(oldDir, newDir)
}

// handleChangeGroup implements `/changegroup <alias>`: move the project owning
// the current topic to another group. Creates a fresh topic in the target
// group's chat, migrates history, updates the session, and closes the old topic.
// Admin-only (slash commands are gated to the admin in listen()).
func handleChangeGroup(config *Config, chatID, threadID int64, alias string) {
	if alias == "" {
		sendMessage(config, chatID, threadID, "Usage: /changegroup <group-alias>")
		return
	}
	sessionName := getSessionByGroupTopic(config, chatID, threadID)
	if sessionName == "" {
		sendMessage(config, chatID, threadID, "❌ No project session is mapped to this topic")
		return
	}
	if err := changeGroupCore(config, sessionName, alias); err != nil {
		sendMessage(config, chatID, threadID, "❌ "+err.Error())
	}
}

// changeGroupCore moves a project session to another group: it creates a fresh
// topic in the target group's chat, preserves history, updates config, and
// closes the old topic (posting notices in both). Shared by the /changegroup
// Telegram command and the `ccc changegroup` CLI. The old chat is derived from
// the session's current group, so it works regardless of caller.
func changeGroupCore(config *Config, sessionName, alias string) error {
	info := config.Sessions[sessionName]
	if info == nil {
		return fmt.Errorf("session %q not found", sessionName)
	}
	targetChat := groupChatID(config, alias)
	if targetChat == 0 {
		return fmt.Errorf("unknown group %q (add it to ~/.ccc.json groups first)", alias)
	}
	if sessionGroup(info) == alias {
		return fmt.Errorf("'%s' is already in group %q", sessionName, alias)
	}
	oldChat := groupChatID(config, sessionGroup(info))
	oldTopic := info.TopicID

	newTopic, err := createForumTopic(config, targetChat, sessionName)
	if err != nil {
		return fmt.Errorf("create topic in target group: %w", err)
	}
	if err := moveHistory(oldTopic, newTopic); err != nil {
		fmt.Fprintf(os.Stderr, "changegroup: history move %d->%d: %v\n", oldTopic, newTopic, err)
	}
	groupField := alias
	if alias == "default" {
		groupField = ""
	}
	info.Group, info.TopicID = groupField, newTopic
	saveConfig(config)

	sendMessage(config, targetChat, newTopic,
		fmt.Sprintf("📦 Project '%s' moved here. History preserved. Interact with the agent in this topic now.", sessionName))
	if oldChat != 0 && oldTopic != 0 {
		sendMessage(config, oldChat, oldTopic,
			fmt.Sprintf("📦 Project '%s' moved to group %q. This topic is closed — use the new one.", sessionName, alias))
		if err := closeForumTopic(config, oldChat, oldTopic); err != nil {
			fmt.Fprintf(os.Stderr, "changegroup: close old topic: %v\n", err)
		}
	}
	return nil
}

// getOrCreateTopic finds existing topic or creates new one
// Also syncs topic name and updates path if changed
func getOrCreateTopic(config *Config, fullName string, path string, host string) (int64, error) {
	// Check if session exists in config (including deleted)
	if info, exists := config.Sessions[fullName]; exists {
		// Operate within the session's own group (default group falls back to GroupID).
		chatID := groupChatID(config, sessionGroup(info))
		// Try to rename topic to verify it exists and sync name
		err := editForumTopic(config, chatID, info.TopicID, fullName)
		if err != nil {
			errStr := err.Error()
			// Check if error indicates topic doesn't exist vs just "not modified"
			if strings.Contains(errStr, "not found") || strings.Contains(errStr, "TOPIC_CLOSED") ||
				strings.Contains(errStr, "TOPIC_DELETED") || strings.Contains(errStr, "invalid") {
				// Topic was deleted by user, create new one
				fmt.Fprintf(os.Stderr, "Topic %d gone, creating new: %v\n", info.TopicID, err)
				topicID, err := createForumTopic(config, chatID, fullName)
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

	// Create new topic in the default group.
	topicID, err := createForumTopic(config, config.GroupID, fullName)
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
	tmuxCmd("send-keys", "-t", name, cccCmd, "C-m").Run()

	return nil
}

// runClaudeRaw runs claude directly (used inside tmux sessions)
func runClaudeRaw(continueSession bool) error {
	if claudePath == "" {
		return fmt.Errorf("claude binary not found")
	}

	// Resolve per-session launch tweaks from config: account config dir and the
	// secretary flag. Both key off the session that owns the current cwd.
	var accountDir, secretaryName string
	cfg, cfgErr := loadConfig()
	cwd, cwdErr := os.Getwd()
	if cfgErr == nil && cwdErr == nil {
		accountDir = claudeConfigDirForCwd(cfg, cwd)
		if name := resolveCaller(cfg, "", cwd); name != "" && mail.IsSecretary(name) {
			secretaryName = name
		}
	}

	// Effective config dir this launch uses (account override, else default ~/.claude).
	effDir := accountDir
	if effDir == "" {
		home, _ := os.UserHomeDir()
		effDir = filepath.Join(home, ".claude")
	}

	// Suppress the ~20 claude.ai team-scope connectors a corporate login auto-pulls
	// — noise for headless CCC agents. Config-dir level (all projects at once), so it
	// also covers switching account via /login WITHOUT changing folder (the default
	// ~/.claude case, not just CLAUDE_CONFIG_DIR-routed agents). Server-local only: a
	// client machine's ~/.claude is the operator's personal Claude Code config, which
	// CCC never touches.
	if cfgErr == nil && cfg != nil && cfg.Mode != "client" {
		if err := ensureConnectorsDisabledInConfigDir(filepath.Join(effDir, "settings.json")); err != nil {
			fmt.Fprintf(os.Stderr, "ccc run: disable connectors in %s: %v\n", effDir, err)
		}
	}

	// If asked to continue but effDir has no prior conversation for this project
	// (e.g. an agent freshly moved onto another account), start fresh instead of
	// failing with "No conversation found to continue".
	if continueSession && cwdErr == nil {
		if !dirHasJSONL(filepath.Join(effDir, "projects", encodeProjectPath(cwd))) {
			fmt.Fprintf(os.Stderr, "ccc run: no prior conversation in %s for %s — starting fresh (dropping -c)\n", effDir, cwd)
			continueSession = false
		}
	}

	args := []string{"--dangerously-skip-permissions"}
	if continueSession {
		args = append(args, "-c")
	}
	if secretaryName != "" {
		// Secretaries run on a lighter model: mail routing doesn't need Opus, it's
		// cheaper, and a smaller model degenerates less (the "court" loop). Ordinary
		// agents keep the default model.
		args = append(args, "--model", secretaryModel)
		fmt.Fprintf(os.Stderr, "ccc run: --model %s (secretary %s)\n", secretaryModel, secretaryName)
	}

	cmd := exec.Command(claudePath, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// If this agent's group is pinned to a subscription account, launch claude with
	// that account's CLAUDE_CONFIG_DIR. Strict no-op otherwise: cmd.Env stays nil and
	// claude inherits the default environment (~/.claude) exactly as before.
	if accountDir != "" {
		// Pre-trust this folder in the account's config so the TUI folder-trust
		// prompt (which --dangerously-skip-permissions does NOT suppress) never
		// blocks a headless agent launched under a fresh account.
		if err := ensureTrustedInConfigDir(filepath.Join(accountDir, ".claude.json"), cwd); err != nil {
			fmt.Fprintf(os.Stderr, "ccc run: pre-trust %s in %s: %v\n", cwd, accountDir, err)
		}
		// Register the secretary MCP in this account's config dir so mail works.
		// A fresh account config has no mcpServers, so without this the agent
		// boots without the mail tool and silently can't send/deliver.
		if err := ensureSecretaryMcpInConfigDir(filepath.Join(accountDir, ".claude.json")); err != nil {
			fmt.Fprintf(os.Stderr, "ccc run: ensure secretary mcp in %s: %v\n", accountDir, err)
		}
		cmd.Env = append(os.Environ(), "CLAUDE_CONFIG_DIR="+accountDir)
		fmt.Fprintf(os.Stderr, "ccc run: CLAUDE_CONFIG_DIR=%s (group account)\n", accountDir)
	}

	return cmd.Run()
}

// accountDirForGroup returns the expanded CLAUDE_CONFIG_DIR for a group, or ""
// when the group has no account pinned (→ default ~/.claude).
func accountDirForGroup(cfg *Config, group string) string {
	if cfg == nil {
		return ""
	}
	gi := cfg.Groups[group]
	if gi == nil || gi.Account == "" {
		return ""
	}
	dir := cfg.Accounts[gi.Account]
	if dir == "" {
		return ""
	}
	return expandPath(dir)
}

// claudeConfigDirForCwd resolves the account config dir for the LOCAL session
// owning cwd (matched by path). Returns "" for unknown/default → no override.
func claudeConfigDirForCwd(cfg *Config, cwd string) string {
	if cfg == nil {
		return ""
	}
	want := filepath.Clean(cwd)
	for _, si := range cfg.Sessions {
		if si == nil || si.Deleted || si.Host != "" {
			continue
		}
		p := filepath.Clean(si.Path)
		if p == want || strings.HasPrefix(want, p+"/") {
			return accountDirForSession(cfg, si)
		}
	}
	return ""
}

// accountDirForSession resolves the CLAUDE_CONFIG_DIR for one session: its own
// Account override wins, else the session's group account, else "" (default
// ~/.claude). Lets individual agents run on a separate account while staying in
// their group's mail domain.
func accountDirForSession(cfg *Config, si *SessionInfo) string {
	if cfg == nil || si == nil {
		return ""
	}
	if si.Account != "" {
		if dir := cfg.Accounts[si.Account]; dir != "" {
			return expandPath(dir)
		}
	}
	return accountDirForGroup(cfg, sessionGroup(si))
}

// setSessionAccount pins (or clears) a single session's subscription account.
// alias must be a key in cfg.Accounts, or "default"/"none"/"" to clear (→ inherit
// the group's account).
func setSessionAccount(cfg *Config, session, alias string) (string, error) {
	if alias == "none" || alias == "default" {
		alias = ""
	}
	if alias != "" {
		if _, ok := cfg.Accounts[alias]; !ok {
			return "", fmt.Errorf("unknown account %q (register it first: ccc account add %s <config-dir>)", alias, alias)
		}
	}
	si := cfg.Sessions[session]
	if si == nil {
		return "", fmt.Errorf("no session named %q", session)
	}
	si.Account = alias
	saveConfig(cfg)
	if alias == "" {
		return fmt.Sprintf("Session '%s' → inherits its group's account. Restart it (/continue) to apply.", session), nil
	}
	return fmt.Sprintf("Session '%s' → account '%s' (%s). Restart it (/continue) to apply.", session, alias, cfg.Accounts[alias]), nil
}

// setGroupAccount pins (or clears) a group's subscription account. alias must be
// a key in cfg.Accounts, or "default"/"none"/"" to clear (→ ~/.claude).
func setGroupAccount(cfg *Config, group, alias string) (string, error) {
	if group == "" {
		return "", fmt.Errorf("no group for this topic")
	}
	if alias == "none" || alias == "default" {
		alias = ""
	}
	if alias != "" {
		if _, ok := cfg.Accounts[alias]; !ok {
			return "", fmt.Errorf("unknown account %q (register it first: ccc account add %s <config-dir>)", alias, alias)
		}
	}
	gi := cfg.Groups[group]
	if gi == nil {
		gi = &GroupInfo{ChatID: groupChatID(cfg, group)}
		if cfg.Groups == nil {
			cfg.Groups = map[string]*GroupInfo{}
		}
		cfg.Groups[group] = gi
	}
	gi.Account = alias
	saveConfig(cfg)
	if alias == "" {
		return fmt.Sprintf("Group '%s' → default account (~/.claude). Restart its agents to apply.", group), nil
	}
	return fmt.Sprintf("Group '%s' → account '%s' (%s). Restart its agents to apply.", group, alias, cfg.Accounts[alias]), nil
}

// accountConfigDirOrDefault turns an account dir ("" = default) into a concrete path.
func accountConfigDirOrDefault(dir string) string {
	if dir != "" {
		return dir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude")
}

// dirHasJSONL reports whether dir contains at least one *.jsonl (a Claude session).
func dirHasJSONL(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
			return true
		}
	}
	return false
}

// migrateGroupContext copies each local agent session's Claude conversation history
// from oldDir to newDir (two CLAUDE_CONFIG_DIR roots) and relaunches the sessions so
// they resume WITH context under the group's new account. Without this, switching a
// group's account starts every agent from a blank conversation. The group's secretary
// is restarted fresh (it rebuilds from its mail files, not from chat history). This is
// the automation behind `ccc account set <group> <alias> --migrate`.
func migrateGroupContext(cfg *Config, group, oldDir, newDir string) error {
	if oldDir == newDir {
		fmt.Printf("migrate: config dir unchanged (%s) — nothing to migrate\n", oldDir)
		return nil
	}
	fmt.Printf("migrate: %s  ->  %s\n", oldDir, newDir)
	for name, info := range cfg.Sessions {
		if info == nil || info.Deleted || info.Host != "" {
			continue // skip deleted and remote (SSH) sessions
		}
		if sessionGroup(info) != group || strings.HasPrefix(name, "secretary") {
			continue // secretary handled separately, below
		}
		enc := encodeProjectPath(info.Path)
		src := filepath.Join(oldDir, "projects", enc)
		dst := filepath.Join(newDir, "projects", enc)
		if _, err := os.Stat(src); err == nil {
			os.MkdirAll(filepath.Join(newDir, "projects"), 0755)
			os.RemoveAll(dst)
			if out, err := exec.Command("cp", "-rp", src, dst).CombinedOutput(); err != nil {
				fmt.Fprintf(os.Stderr, "migrate %s: copy history failed: %v: %s\n", name, err, out)
			} else {
				fmt.Printf("migrate %s: history copied\n", name)
			}
		} else {
			fmt.Printf("migrate %s: no history at %s — relaunching fresh\n", name, src)
		}
		// -c only if the destination now holds a session; otherwise `claude -c` would
		// fail with "No conversation found to continue".
		continueSession := dirHasJSONL(dst)
		tmux := tmuxSessionName(name)
		if tmuxSessionExists(tmux) {
			tmuxCmd("kill-session", "-t", tmux).Run()
		}
		if err := createTmuxSession(tmux, info.Path, continueSession); err != nil {
			fmt.Fprintf(os.Stderr, "migrate %s: relaunch failed: %v\n", name, err)
		} else {
			mode := "fresh"
			if continueSession {
				mode = "with context (-c)"
			}
			fmt.Printf("migrate %s: relaunched %s\n", name, mode)
		}
	}
	// Bring the group's secretary onto the new account too (fresh; Feature-1 auto-reg
	// gives it the secretary MCP there). No-op error if the group has no secretary.
	if err := restartSecretarySession(cfg, group); err != nil {
		fmt.Fprintf(os.Stderr, "migrate: secretary restart: %v\n", err)
	} else {
		fmt.Printf("migrate: secretary for group %s restarted on new account\n", group)
	}
	return nil
}

// handleAccountCommand implements /account [alias] in a group topic (admin-only):
// no arg shows the group's current account; an alias pins it.
func handleAccountCommand(config *Config, chatID, threadID int64, arg string) {
	sessionName := getSessionByGroupTopic(config, chatID, threadID)
	if sessionName == "" {
		sendMessage(config, chatID, threadID, "❌ No session mapped to this topic")
		return
	}
	group := sessionGroup(config.Sessions[sessionName])
	if arg == "" {
		cur := "default (~/.claude)"
		if gi := config.Groups[group]; gi != nil && gi.Account != "" {
			cur = fmt.Sprintf("%s (%s)", gi.Account, config.Accounts[gi.Account])
		}
		avail := "none registered"
		if len(config.Accounts) > 0 {
			var names []string
			for a := range config.Accounts {
				names = append(names, a)
			}
			sort.Strings(names)
			avail = strings.Join(names, ", ")
		}
		sendMessage(config, chatID, threadID, fmt.Sprintf("💳 Account for group *%s*: %s\nAvailable: %s\nUsage: /account <alias|default>", group, cur, avail))
		return
	}
	msg, err := setGroupAccount(config, group, arg)
	if err != nil {
		sendMessage(config, chatID, threadID, "❌ "+err.Error())
		return
	}
	sendMessage(config, chatID, threadID, "✅ "+msg+"\n(On the server: restart the group's sessions so they relaunch under the new account.)")
}

// startSession creates/attaches to a tmux session with Telegram topic
func startSession(continueSession bool, group, account string) error {
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

	// Validate optional --group / --account flags up front.
	if account != "" {
		if _, ok := config.Accounts[account]; !ok {
			return fmt.Errorf("unknown account %q (register it first: ccc account add %s <config-dir>)", account, account)
		}
	}
	groupField := ""
	targetChat := config.GroupID // default group's chat
	if group != "" && group != "default" {
		if c := groupChatID(config, group); c != 0 {
			targetChat, groupField = c, group
		} else {
			return fmt.Errorf("unknown group %q (add it to ~/.ccc.json groups first)", group)
		}
	}

	if si, exists := config.Sessions[name]; !exists || si == nil {
		// New session: create its topic directly in the target group's chat, and
		// pin the account from the start — so the very first launch is in the right
		// group on the right account (no /changegroup + set-session + /continue).
		if targetChat != 0 {
			topicID, err := createForumTopic(config, targetChat, name)
			if err == nil {
				config.Sessions[name] = &SessionInfo{
					TopicID: topicID, Path: cwd, Group: groupField, Account: account,
				}
				saveConfig(config)
				fmt.Printf("📱 Created session %q (topic %d, group=%q, account=%q)\n", name, topicID, groupField, account)
			}
		}
	} else {
		// Existing session: apply the flags if given (account applies on this
		// (re)launch; a group change recreates the topic in the target chat).
		if account != "" && si.Account != account {
			si.Account = account
			saveConfig(config)
			fmt.Printf("💳 Session %q → account %q\n", name, account)
		}
		if group != "" && sessionGroup(si) != groupField {
			if err := changeGroupCore(config, name, group); err != nil {
				fmt.Fprintf(os.Stderr, "changegroup: %v\n", err)
			}
		}
	}

	// Check if tmux session exists
	if tmuxSessionExists(tmuxName) {
		// Check if we're already inside tmux
		if os.Getenv("TMUX") != "" {
			// Inside tmux: switch to the session
			cmd := tmuxCmd("switch-client", "-t", tmuxName)
			cmd.Stdin = os.Stdin
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			return cmd.Run()
		}
		// Outside tmux: attach to existing session
		cmd := tmuxCmd("attach-session", "-t", tmuxName)
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
		cmd := tmuxCmd("switch-client", "-t", tmuxName)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}
	cmd := tmuxCmd("attach-session", "-t", tmuxName)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// tmuxLiteralLimit is the largest text we pass to `tmux send-keys -l <arg>`.
// Above it tmux returns "command too long", so we inject via a tmux buffer
// (load-buffer reads stdin — no length limit) and paste it into the pane.
const tmuxLiteralLimit = 16000

var mailBufSeq int64

func mailBufName() string {
	return fmt.Sprintf("ccc-%d-%d", time.Now().UnixNano(), atomic.AddInt64(&mailBufSeq, 1))
}

// tmuxInjectLocal injects text into a local tmux pane. Small text uses
// send-keys -l (the proven path); large text is pasted via a one-shot buffer so
// it does not hit tmux's per-argument "command too long" limit.
func tmuxInjectLocal(session, text string) error {
	if len(text) <= tmuxLiteralLimit {
		return tmuxCmd("send-keys", "-t", session, "-l", text).Run()
	}
	buf := mailBufName()
	lc := tmuxCmd("load-buffer", "-b", buf, "-")
	lc.Stdin = strings.NewReader(text)
	if err := lc.Run(); err != nil {
		return fmt.Errorf("tmux load-buffer: %w", err)
	}
	return tmuxCmd("paste-buffer", "-b", buf, "-t", session, "-d").Run()
}

func sendToTmux(session string, text string) error {
	// Always use 2 second delay before Enter to ensure text is fully processed
	// Without this delay, Enter may be interpreted as newline instead of submit
	return sendToTmuxWithDelay(session, text, 2*time.Second)
}

func sendToTmuxWithDelay(session string, text string, delay time.Duration) error {
	// Inject text (buffer-paste for very large text send-keys -l cannot handle)
	if err := tmuxInjectLocal(session, text); err != nil {
		return err
	}

	// Wait for content to load (e.g., images, long pasted text)
	time.Sleep(delay)

	// Send Enter twice (Claude Code needs double Enter)
	cmd := tmuxCmd("send-keys", "-t", session, "C-m")
	if err := cmd.Run(); err != nil {
		return err
	}
	time.Sleep(50 * time.Millisecond)
	cmd = tmuxCmd("send-keys", "-t", session, "C-m")
	return cmd.Run()
}

func killTmuxSession(name string) error {
	cmd := tmuxCmd("kill-session", "-t", name)
	return cmd.Run()
}

func listTmuxSessions() ([]string, error) {
	cmd := tmuxCmd("list-sessions", "-F", "#{session_name}")
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
	cmd := tmuxCmd("list-sessions", "-F",
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
	topicID, err := createForumTopic(config, config.GroupID, name)
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

func getSessionByTopic(cfg *Config, topicID int64) string {
	return config.GetSessionByTopic(cfg, topicID)
}

// Project-group helpers (phase 1: data model). default group = original GroupID.
func sessionGroup(info *SessionInfo) string       { return config.SessionGroup(info) }
func groupChatID(cfg *Config, group string) int64 { return config.GroupChatID(cfg, group) }
func sessionGroupChatID(cfg *Config, name string) int64 {
	return config.SessionGroupChatID(cfg, name)
}
func getSessionByGroupTopic(cfg *Config, chatID, topicID int64) string {
	return config.GetSessionByGroupTopic(cfg, chatID, topicID)
}
func sessionIntegrationMode(cfg *Config, info *SessionInfo) string {
	return config.SessionIntegrationMode(cfg, info)
}
func validIntegrationMode(s string) bool { return config.ValidIntegrationMode(s) }
func isSessionLive(cfg *Config, info *SessionInfo) bool {
	return config.SessionIntegrationMode(cfg, info) == config.IntegrationLive
}

// callSocket delivers one APIRequest to the CCC bot and returns the response.
// Short-lived hook processes use it to signal the long-running bot, which holds
// in-memory state (typing indicators, live stream messages) a separate process
// cannot touch. On the server it dials the local Unix socket; on a CLIENT
// machine (no local bot) it relays over SSH to the server's `ccc mcp-relay`
// (stamping the caller host), so live signals from remote agents reach the bot.
// Best-effort: callers should ignore errors gracefully.
func callSocket(cfg *Config, req APIRequest) (*APIResponse, error) {
	if cfg != nil && cfg.Mode == "client" && cfg.Server != "" && cfg.HostName != "" {
		req.Host = cfg.HostName
		data, err := json.Marshal(req)
		if err != nil {
			return nil, err
		}
		out, err := runSSHWithInput(cfg.Server, "base64 -d | ccc mcp-relay",
			base64.StdEncoding.EncodeToString(data), 15*time.Second)
		if err != nil {
			return nil, err
		}
		var resp APIResponse
		if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &resp); err != nil {
			return nil, err
		}
		return &resp, nil
	}

	conn, err := net.DialTimeout("unix", socketPath(), 2*time.Second)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	data, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Write(append(data, '\n')); err != nil {
		return nil, err
	}
	var resp APIResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// liveCachePath is a per-cwd temp marker letting client hooks remember, for the
// turn, whether the session is live (learned from the typing-start response).
// This avoids relaying every stream delta over SSH for legacy remote agents.
func liveCachePath(cwd string) string {
	return filepath.Join(os.TempDir(), "ccc-live"+strings.ReplaceAll(cwd, "/", "_"))
}
func writeLiveCache(cwd string, live bool) {
	v := "legacy"
	if live {
		v = "live"
	}
	os.WriteFile(liveCachePath(cwd), []byte(v), 0600)
}
func liveCacheSaysLive(cwd string) bool {
	b, _ := os.ReadFile(liveCachePath(cwd))
	return string(b) == "live"
}

// resolveSessionByHostCwd finds the session on `host` ("" = server-local) whose
// path matches `cwd`. Used server-side to map a relayed live signal (which knows
// only the caller's host + cwd, not the server's "host:project" session name)
// back to a session.
func resolveSessionByHostCwd(cfg *Config, host, cwd string) string {
	if cwd == "" {
		return ""
	}
	for name, info := range cfg.Sessions {
		if info == nil || info.Deleted || info.Host != host {
			continue
		}
		_, proj := parseSessionTarget(name)
		if cwd == info.Path || strings.HasPrefix(cwd, info.Path+"/") || strings.HasSuffix(cwd, "/"+proj) {
			return name
		}
	}
	return ""
}

// humanTag returns a sender prefix to prepend to a human message injected into
// an agent, so the agent can always tell which person is speaking — including
// the owner and in multi-human groups. Every human message is tagged.
func humanTag(fromID int64, firstName, username string) string {
	name := firstName
	if name == "" {
		name = username
	}
	if name == "" {
		name = fmt.Sprintf("user%d", fromID)
	}
	if username != "" && username != name {
		return fmt.Sprintf("[from %s (@%s)] ", name, username)
	}
	return fmt.Sprintf("[from %s] ", name)
}

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
			switchCmd := tmuxCmd("switch-client", "-t", tmuxName)
			switchCmd.Stdin = os.Stdin
			switchCmd.Stdout = os.Stdout
			switchCmd.Stderr = os.Stderr
			return switchCmd.Run()
		}
		// Outside tmux: attach to existing session
		attachCmd := tmuxCmd("attach-session", "-t", tmuxName)
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
		attachCmd := tmuxCmd("switch-client", "-t", tmuxName)
		attachCmd.Stdin = os.Stdin
		attachCmd.Stdout = os.Stdout
		attachCmd.Stderr = os.Stderr
		return attachCmd.Run()
	}
	attachCmd := tmuxCmd("attach-session", "-t", tmuxName)
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

// encodeProjectPath encodes a filesystem path the same way Claude Code does for its
// <config-dir>/projects/ dirs: replaces "/", "_" and "." with "-". Verified against real
// dirs (…/GTaara_group/PM → -home-wlad-Projects-GTaara-group-PM; ~/.ccc/secretary-gtara
// → -home-wlad--ccc-secretary-gtara).
func encodeProjectPath(path string) string {
	s := strings.ReplaceAll(path, "/", "-")
	s = strings.ReplaceAll(s, "_", "-")
	s = strings.ReplaceAll(s, ".", "-")
	return s
}

// resolveProjectPathFromTranscript finds the actual project path by matching
// encoded transcript dir against progressively longer prefixes of cwd.
// Claude Code encodes project paths by replacing "/" and "_" with "-".
// Returns the project path if found, empty string otherwise.
func resolveProjectPathFromTranscript(encodedProjectDir string, cwd string) string {
	if encodedProjectDir == "" || cwd == "" {
		return ""
	}

	// Normalize cwd (expand ~)
	if strings.HasPrefix(cwd, "~") {
		home, _ := os.UserHomeDir()
		cwd = home + cwd[1:]
	}

	// Split into segments and build path incrementally
	segments := strings.Split(cwd, "/")
	currentPath := ""
	for _, segment := range segments {
		if segment == "" {
			continue
		}
		currentPath += "/" + segment

		if encodeProjectPath(currentPath) == encodedProjectDir {
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

// hookTracePath is the chronological JSONL file collecting EVERY hook event
// while trace mode is enabled for a test agent. One JSON object per line.
func hookTracePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".ccc", "hook-trace.jsonl")
}

// handleHookTrace is a passive, non-blocking logger wired onto ALL Claude hook
// events for a single test agent. It records the complete raw hook payload (so
// we can later discover which fields each event carries) and exits 0 with no
// stdout, so it never alters Claude's behavior on any event. This is the
// discovery tool for deciding which hooks are worth integrating.
func handleHookTrace() error {
	raw, _ := io.ReadAll(os.Stdin)

	// Pull a few fields for quick scanning without losing the full payload.
	var meta struct {
		HookEventName string `json:"hook_event_name"`
		ToolName      string `json:"tool_name"`
		SessionID     string `json:"session_id"`
		Cwd           string `json:"cwd"`
	}
	_ = json.Unmarshal(raw, &meta)

	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Join(home, ".ccc"), 0755); err != nil {
		return nil
	}

	// payload is the verbatim hook JSON; if it is not valid JSON, store it as a
	// quoted string so the trace line itself stays valid JSON.
	payload := json.RawMessage(raw)
	if !json.Valid(raw) {
		if b, e := json.Marshal(string(raw)); e == nil {
			payload = b
		} else {
			payload = json.RawMessage(`null`)
		}
	}

	entry := map[string]interface{}{
		"ts":      time.Now().Format(time.RFC3339Nano),
		"event":   meta.HookEventName,
		"tool":    meta.ToolName,
		"session": meta.SessionID,
		"cwd":     meta.Cwd,
		"payload": payload,
	}
	line, err := json.Marshal(entry)
	if err != nil {
		return nil
	}

	f, err := os.OpenFile(hookTracePath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil
	}
	defer f.Close()
	f.Write(append(line, '\n'))
	return nil
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

	// Stop the live typing indicator — the turn ended. Works for local and
	// remote agents (relayed to the server in client mode, which resolves the
	// session by host+cwd).
	callSocket(config, APIRequest{Cmd: "typing", Cwd: hookData.Cwd, Text: "stop"})

	// Remote live agent: if a stream was active, finalize it on the server with
	// the complete text instead of forwarding a duplicate message. Gated by the
	// per-turn live cache so legacy remote agents skip this entirely.
	if config.Mode == "client" && liveCacheSaysLive(hookData.Cwd) {
		resp, err := callSocket(config, APIRequest{Cmd: "stream", Cwd: hookData.Cwd, StreamAction: "final", Text: lastMessage})
		if err == nil && resp != nil && resp.OK && resp.Response != "nostream" {
			os.Remove(liveCachePath(hookData.Cwd))
			return nil
		}
	}

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

	// (typing already stopped above via the host-agnostic signal)

	// Store Claude's response in history
	appendHistory(topicID, HistoryMessage{
		ID:        nextMessageID(),
		Timestamp: time.Now().Unix(),
		From:      "claude",
		Text:      lastMessage,
	})

	final := fmt.Sprintf("✅ %s\n\n%s", sessionName, lastMessage)

	// In live mode, an active stream message is already showing this response —
	// finalize it (one edit) instead of posting a duplicate. If there is no
	// stream (e.g. no deltas arrived), fall through and send normally.
	if isSessionLive(config, config.Sessions[sessionName]) {
		if resp, err := callSocket(config, APIRequest{Cmd: "stream", Session: sessionName, StreamAction: "final", Text: final}); err == nil && resp != nil && resp.OK && resp.Response != "nostream" {
			return nil
		}
	}

	return sendMessage(config, sessionGroupChatID(config, sessionName), topicID, final)
}

// handleDisplayHook handles the MessageDisplay hook: in live mode it forwards
// each incremental assistant-text delta to the bot, which streams it into one
// Telegram message via editMessageText. No-op in legacy mode. Works for remote
// agents too: in client mode it relays over SSH, gated by the per-turn live
// cache so legacy remote agents don't relay anything.
func handleDisplayHook() error {
	defer func() { recover() }()
	raw, _ := io.ReadAll(os.Stdin)
	var hd struct {
		Cwd   string `json:"cwd"`
		Delta string `json:"delta"`
	}
	if json.Unmarshal(raw, &hd) != nil || hd.Delta == "" {
		return nil
	}
	config, err := loadConfig()
	if err != nil || config == nil {
		return nil
	}

	if config.Mode == "client" {
		// Remote agent: only relay if this turn was marked live (set by the
		// typing-start hook), so legacy remote agents incur zero relay cost.
		if !liveCacheSaysLive(hd.Cwd) {
			return nil
		}
		callSocket(config, APIRequest{Cmd: "stream", Cwd: hd.Cwd, StreamAction: "delta", Text: hd.Delta})
		return nil
	}

	// Server-local: resolve the session and gate on live + streaming.
	var sessionName string
	for name, info := range config.Sessions {
		if info == nil {
			continue
		}
		if hd.Cwd == info.Path || strings.HasPrefix(hd.Cwd, info.Path+"/") || strings.HasSuffix(hd.Cwd, "/"+name) {
			sessionName = name
			break
		}
	}
	info := config.Sessions[sessionName]
	if sessionName == "" || !isSessionLive(config, info) || (info != nil && info.StreamOff) {
		return nil
	}
	callSocket(config, APIRequest{Cmd: "stream", Session: sessionName, StreamAction: "delta", Text: hd.Delta})
	return nil
}

// postAskUserQuestion stores the pending question set and posts inline-keyboard
// messages (single-select buttons or multiSelect toggle+Submit) to the session's
// Telegram topic. Used for local agents (PreToolUse hook) and remote agents
// (relayed via the "question" socket command). Synchronous: the caller is a
// short-lived hook process, so a goroutine would die before the send completes.
func postAskUserQuestion(cfg *Config, sessionName string, topicID int64, questions []auqQuestion) {
	totalQuestions := len(questions)
	pqs := &PendingQuestionSet{Session: sessionName, Timestamp: time.Now().Unix(), TopicID: topicID}
	for _, q := range questions {
		pq := PendingQuestion{Question: q.Question, Header: q.Header, MultiSelect: q.MultiSelect}
		for _, opt := range q.Options {
			pq.Options = append(pq.Options, PendingQuestionOption{Label: opt.Label, Description: opt.Description})
		}
		if pq.MultiSelect {
			pq.Selected = make([]bool, len(pq.Options))
		}
		pqs.Questions = append(pqs.Questions, pq)
	}
	pendingQuestions.Store(sessionName, pqs)

	chatID := sessionGroupChatID(cfg, sessionName)
	for qIdx, q := range questions {
		if q.Question == "" {
			continue
		}
		msg := fmt.Sprintf("❓ %s\n\n%s", q.Header, q.Question)
		var buttons [][]InlineKeyboardButton
		if q.MultiSelect {
			buttons = buildMultiSelectKeyboard(sessionName, qIdx, totalQuestions, pqs.Questions[qIdx])
		} else {
			for i, opt := range q.Options {
				if opt.Label == "" {
					continue
				}
				cd := fmt.Sprintf("%s:%d:%d:%d", sessionName, qIdx, totalQuestions, i)
				if len(cd) > 64 {
					cd = cd[:64]
				}
				label := opt.Label
				if opt.Description != "" {
					label += " — " + opt.Description
				}
				buttons = append(buttons, []InlineKeyboardButton{{Text: truncButtonLabel(label), CallbackData: cd}})
			}
		}
		if len(buttons) > 0 {
			sendMessageWithKeyboard(cfg, chatID, topicID, msg, buttons)
		}
		appendHistory(topicID, HistoryMessage{ID: nextMessageID(), Timestamp: time.Now().Unix(), From: "claude", Text: msg})
	}
}

// handleQuestionSocketCmd posts AskUserQuestion buttons for a remote agent: the
// client relays the questions (req.Payload) with its host+cwd; the server
// resolves the session and posts. callback_data uses the server-side session
// name, so the existing callback handler injects keystrokes into the remote
// agent's tmux over SSH.
func handleQuestionSocketCmd(encoder *json.Encoder, cfg *Config, req APIRequest) {
	session := req.Session
	if session == "" {
		session = resolveSessionByHostCwd(cfg, req.Host, req.Cwd)
	}
	info := cfg.Sessions[session]
	if info == nil {
		encoder.Encode(APIResponse{OK: false, Error: "session not found"})
		return
	}
	var questions []auqQuestion
	if err := json.Unmarshal(req.Payload, &questions); err != nil || len(questions) == 0 {
		encoder.Encode(APIResponse{OK: false, Error: "no questions"})
		return
	}
	postAskUserQuestion(cfg, session, info.TopicID, questions)
	encoder.Encode(APIResponse{OK: true})
}

// logNotify appends every Notification payload to ~/.ccc/notify.log so we can
// reverse-engineer undocumented prompt types (rating / training-consent).
func logNotify(notifType, message string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(home, ".ccc", "notify.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "[%s] type=%q msg=%q\n", time.Now().Format("2006-01-02 15:04:05"), notifType, message)
}

// handleNotifyHook auto-answers Claude Code's periodic interactive prompts that
// would otherwise hang a headless agent (no human at its terminal). It detects
// the session-rating prompt and the training-consent prompt by message text
// (their notification_type is undocumented) and injects the answer into the
// agent's OWN tmux: rating -> always "3" (Good); training -> allow. Every
// notification is logged so unknown prompts can be wired precisely.
func handleNotifyHook() error {
	defer func() { recover() }()
	raw, _ := io.ReadAll(os.Stdin)
	var nd struct {
		Cwd              string `json:"cwd"`
		NotificationType string `json:"notification_type"`
		Message          string `json:"message"`
	}
	if json.Unmarshal(raw, &nd) != nil {
		return nil
	}
	logNotify(nd.NotificationType, nd.Message)

	msg := strings.ToLower(nd.Message)
	var keys []string
	switch {
	case strings.Contains(msg, "how is claude doing"):
		// Session-rating prompt: 1:Bad 2:Fine 3:Good 0:Dismiss → always Good.
		keys = []string{"3"}
	case strings.Contains(msg, "training") || strings.Contains(msg, "improve") ||
		strings.Contains(msg, "обуч"):
		// Training/model-improvement consent → allow. The exact option keys are
		// undocumented; "1" is typically the first (allow) option.
		keys = []string{"1"}
	default:
		return nil
	}

	// Inject into the agent's own tmux. The hook runs on the agent's machine
	// (server for local agents, the remote host for remote ones), so the tmux
	// session is local — no relay needed. Derive the name from cwd.
	tmuxName := tmuxSessionName(filepath.Base(nd.Cwd))
	if !tmuxSessionExists(tmuxName) {
		logNotify("(no-tmux)", tmuxName)
		return nil
	}
	time.Sleep(400 * time.Millisecond) // let the prompt render/focus
	for _, k := range keys {
		tmuxCmd("send-keys", "-t", tmuxName, "-l", k).Run()
		time.Sleep(120 * time.Millisecond)
	}
	return nil
}

// handleStopFailureHook fires when a turn ends due to an API error. If it looks
// like a transient rate-limit/overload, it asks the server to schedule a delayed
// "please continue" recovery (random 30–90s). Every payload is logged to
// ~/.ccc/stopfailure.log so we can confirm the hook fires and refine matching.
func handleStopFailureHook() error {
	defer func() { recover() }()
	raw, _ := io.ReadAll(os.Stdin)
	if home, err := os.UserHomeDir(); err == nil {
		if f, e := os.OpenFile(filepath.Join(home, ".ccc", "stopfailure.log"),
			os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644); e == nil {
			fmt.Fprintf(f, "[%s] %s\n", time.Now().Format("2006-01-02 15:04:05"), string(raw))
			f.Close()
		}
	}
	var hd struct {
		Cwd string `json:"cwd"`
	}
	if json.Unmarshal(raw, &hd) != nil || hd.Cwd == "" {
		return nil
	}
	// Only auto-retry transient API errors; "please continue" won't help a hard
	// failure (auth/billing) and could loop. The payload usually carries the text.
	low := strings.ToLower(string(raw))
	if !strings.Contains(low, "rate") && !strings.Contains(low, "limit") &&
		!strings.Contains(low, "overload") && !strings.Contains(low, "temporarily") {
		return nil
	}
	config, err := loadConfig()
	if err != nil || config == nil {
		return nil
	}
	callSocket(config, APIRequest{Cmd: "rl.recover", Cwd: hd.Cwd})
	return nil
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

	// In client mode, forward to server
	if config.Mode == "client" && config.Server != "" && config.HostName != "" {
		if hookData.ToolName == "AskUserQuestion" && len(hookData.ToolInput.Questions) > 0 {
			// Relay the question structure so the server posts real inline
			// buttons (callback_data uses the server-side session name, so the
			// callback handler injects keystrokes back into this agent's tmux
			// over SSH). Falls back to a plain text question if the relay fails.
			payload, _ := json.Marshal(hookData.ToolInput.Questions)
			resp, err := callSocket(config, APIRequest{Cmd: "question", Cwd: hookData.Cwd, Payload: payload})
			if err != nil || resp == nil || !resp.OK {
				for _, q := range hookData.ToolInput.Questions {
					if q.Question == "" {
						continue
					}
					msg := fmt.Sprintf("❓ %s\n\n%s", q.Header, q.Question)
					for i, opt := range q.Options {
						msg += fmt.Sprintf("\n%d. %s", i+1, opt.Label)
						if opt.Description != "" {
							msg += fmt.Sprintf(" — %s", opt.Description)
						}
					}
					forwardToServer(config, hookData.Cwd, hookData.TranscriptPath, msg)
				}
			}
			return nil
		}
		if hookData.ToolName == "ExitPlanMode" {
			planText := readLatestPlanFile(hookData.Cwd)
			if planText == "" {
				planText = "(plan file not found)"
			}
			if len(planText) > 3900 {
				planText = planText[:3900] + "\n\n... (truncated)"
			}
			msg := fmt.Sprintf("📋 Plan ready:\n\n%s", planText)
			forwardToServer(config, hookData.Cwd, hookData.TranscriptPath, msg)
			return nil
		}
	}

	// Find session by matching cwd
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

	// Handle AskUserQuestion — send inline keyboard buttons to Telegram
	logHook("Permission", "tool=%s session=%s questions=%d", hookData.ToolName, sessionName, len(hookData.ToolInput.Questions))
	if hookData.ToolName == "AskUserQuestion" && len(hookData.ToolInput.Questions) > 0 {
		postAskUserQuestion(config, sessionName, topicID, hookData.ToolInput.Questions)
		return nil
	}

	// Handle ExitPlanMode — read plan file and forward to Telegram (synchronous,
	// same reason as above).
	if hookData.ToolName == "ExitPlanMode" {
		planText := readLatestPlanFile(hookData.Cwd)
		if planText == "" {
			sendMessage(config, sessionGroupChatID(config, sessionName), topicID, "📋 Plan mode completed (plan file not found)")
			return nil
		}
		// Truncate to Telegram's 4096 char limit (leave room for header)
		if len(planText) > 3900 {
			planText = planText[:3900] + "\n\n... (truncated)"
		}
		msg := fmt.Sprintf("📋 Plan ready:\n\n%s", planText)
		sendMessage(config, sessionGroupChatID(config, sessionName), topicID, msg)
		// Store in history
		appendHistory(topicID, HistoryMessage{
			ID:        nextMessageID(),
			Timestamp: time.Now().Unix(),
			From:      "claude",
			Text:      msg,
		})
		return nil
	}

	return nil
}

// getLastAssistantMessage reads a Claude Code transcript JSONL file and extracts
// text blocks from the last assistant turn (after the last real user message).
// Handles both nested (message.content) and flat (root-level content) JSONL formats.
// Deduplicates streaming entries by requestId (last entry per requestId wins).
func getLastAssistantMessage(transcriptPath string) string {
	file, err := os.Open(transcriptPath)
	if err != nil {
		logHook("Parse", "failed to open transcript: %v", err)
		return ""
	}
	defer file.Close()

	// Typed structs for parsing JSONL entries
	type contentBlock struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}

	type transcriptLine struct {
		Type      string          `json:"type"`
		RequestID string          `json:"requestId,omitempty"`
		Role      string          `json:"role,omitempty"`
		Content   json.RawMessage `json:"content,omitempty"`
		Message   struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}

	type parsedEntry struct {
		entryType string
		requestID string
		role      string
		content   json.RawMessage
	}

	var entries []parsedEntry
	var linesProcessed int
	scanner := bufio.NewScanner(file)
	// 16MB buffer for large lines (transcripts with images/PDFs)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	for scanner.Scan() {
		linesProcessed++
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var tl transcriptLine
		if json.Unmarshal(line, &tl) != nil {
			continue
		}
		// Use nested message fields if present, otherwise fall back to root-level
		role := tl.Message.Role
		content := tl.Message.Content
		if role == "" {
			role = tl.Role
		}
		if len(content) == 0 {
			content = tl.Content
		}
		entries = append(entries, parsedEntry{
			entryType: tl.Type,
			requestID: tl.RequestID,
			role:      role,
			content:   content,
		})
	}
	if err := scanner.Err(); err != nil {
		logHook("Parse", "scanner error after %d lines: %v", linesProcessed, err)
	}
	if len(entries) == 0 {
		return ""
	}

	// Find the last real user message (not a tool_result)
	lastUserIdx := -1
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if e.entryType != "user" && e.role != "user" {
			continue
		}
		// Check if content is a tool_result
		if isToolResultContent(e.content) {
			continue
		}
		lastUserIdx = i
		break
	}

	// Collect assistant text blocks after the last user message.
	// Streaming dedup: same requestId may have multiple entries with progressively
	// updated text; for each requestId, the last entry's text blocks win.
	startIdx := lastUserIdx + 1
	if lastUserIdx < 0 {
		startIdx = 0
	}

	reqTexts := make(map[string][]string) // requestId -> text blocks from last entry
	var orderedKeys []string              // preserve order of first appearance
	var noIDTexts []string                // texts from entries without requestId

	for i := startIdx; i < len(entries); i++ {
		e := entries[i]
		if e.entryType != "assistant" && e.role != "assistant" {
			continue
		}

		var blocks []contentBlock
		if json.Unmarshal(e.content, &blocks) != nil {
			continue
		}

		var entryTexts []string
		for _, b := range blocks {
			if b.Type != "text" {
				continue
			}
			text := strings.TrimSpace(b.Text)
			if text != "" && text != "(no content)" {
				entryTexts = append(entryTexts, text)
			}
		}
		if len(entryTexts) == 0 {
			continue
		}

		if e.requestID == "" {
			noIDTexts = append(noIDTexts, entryTexts...)
		} else {
			if _, seen := reqTexts[e.requestID]; !seen {
				orderedKeys = append(orderedKeys, e.requestID)
			}
			reqTexts[e.requestID] = entryTexts // last entry with same requestId wins
		}
	}

	var allTexts []string
	for _, key := range orderedKeys {
		allTexts = append(allTexts, reqTexts[key]...)
	}
	allTexts = append(allTexts, noIDTexts...)

	logHook("Parse", "processed %d lines, %d entries, %d text blocks since last user msg", linesProcessed, len(entries), len(allTexts))
	return strings.Join(allTexts, "\n\n")
}

// readLatestPlanFile finds and reads the most recently modified plan file.
// Claude Code stores plans in ~/.claude/plans/<slug>.md
func readLatestPlanFile(cwd string) string {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	planDir := filepath.Join(homeDir, ".claude", "plans")
	files, err := filepath.Glob(filepath.Join(planDir, "*.md"))
	if err != nil || len(files) == 0 {
		return ""
	}

	// Find the most recently modified file
	var newest string
	var newestTime time.Time
	for _, f := range files {
		info, err := os.Stat(f)
		if err != nil {
			continue
		}
		if info.ModTime().After(newestTime) {
			newestTime = info.ModTime()
			newest = f
		}
	}

	if newest == "" || time.Since(newestTime) > 5*time.Minute {
		return "" // Too old, probably not from the current session
	}

	data, err := os.ReadFile(newest)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// isToolResultContent checks if a JSONL content field contains tool_result entries
func isToolResultContent(content json.RawMessage) bool {
	if len(content) == 0 {
		return false
	}
	var blocks []struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(content, &blocks) == nil {
		for _, b := range blocks {
			if b.Type == "tool_result" {
				return true
			}
		}
	}
	return false
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
		return nil
	}

	// Prepare prompt message
	prompt := hookData.Prompt
	if len(prompt) > 500 {
		prompt = prompt[:500] + "..."
	}

	// Start the live typing indicator as soon as a turn begins (the Stop hook
	// ends it). Works for local AND remote agents: callSocket relays to the
	// server in client mode; the server resolves the session by host+cwd and
	// gates on live mode. The response tells us whether the session is live —
	// cache it for the turn so the stream-delta hook can skip relaying on legacy
	// remote agents (zero overhead). Idempotent (cancel+restart).
	if resp, err := callSocket(config, APIRequest{Cmd: "typing", Cwd: hookData.Cwd, Text: "start"}); err == nil {
		writeLiveCache(hookData.Cwd, resp != nil && resp.Response == "live")
	}

	// In client mode, forward to server
	if forwardToServer(config, hookData.Cwd, hookData.TranscriptPath, fmt.Sprintf("💬 %s", prompt)) {
		return nil
	}

	// Find session by matching cwd suffix
	var topicID int64
	var matchedGroup int64
	var sessionName string
	for name, info := range config.Sessions {
		if info == nil {
			continue
		}
		if hookData.Cwd == info.Path || strings.HasPrefix(hookData.Cwd, info.Path+"/") || strings.HasSuffix(hookData.Cwd, "/"+name) {
			topicID = info.TopicID
			matchedGroup = groupChatID(config, sessionGroup(info))
			sessionName = name
			break
		}
	}

	if topicID == 0 || matchedGroup == 0 {
		return nil
	}

	_ = sessionName // typing-start now happens earlier (host-agnostic)

	// Check if this prompt was just sent from Telegram or injected by CCC
	// (mail/reminder/wake all markTelegramSent). Cooldown 10s. Skip the echo so
	// CCC-delivered prompts aren't mirrored back to the topic a second time.
	if wasTelegramSent(topicID) {
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
	sendTypingAction(config, matchedGroup, topicID)

	// Genuine terminal-typed prompt (not from Telegram, not CCC-injected): mirror
	// it to the topic so the human sees what was typed. Swallow any send error —
	// a hook must exit 0 on success, or Claude Code surfaces a scary
	// "UserPromptSubmit hook error" on every turn.
	sendMessage(config, matchedGroup, topicID, fmt.Sprintf("💬 %s", prompt))
	return nil
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

	// Cap at 20000 chars (~5 Telegram messages); sendMessage handles splitting
	if len(msg) > 20000 {
		msg = msg[:20000] + "\n\n... (truncated)"
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

	sendMessage(config, sessionGroupChatID(config, sessionName), topicID, msg)
	return nil
}

// handleQuestionHook is a legacy alias for handlePermissionHook.
// Kept for backward compatibility with old hook registrations.
func handleQuestionHook() error {
	return handlePermissionHook()
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
	// Honor CLAUDE_CONFIG_DIR so `CLAUDE_CONFIG_DIR=~/.claude-work ccc install`
	// installs the CCC hooks into a secondary subscription-account config dir.
	claudeDir := filepath.Join(home, ".claude")
	if cd := os.Getenv("CLAUDE_CONFIG_DIR"); cd != "" {
		claudeDir = expandPath(cd)
	}
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

	// Add UserPromptSubmit hook: the precise "turn begins" edge. Feeds the live
	// typing indicator AND agent work-state tracking (status=working, cancels the
	// idle-notify timer). Pairs with the Stop hook which marks status=idle.
	promptAdded := addHookToEvent(hooks, "UserPromptSubmit", cccPath+" hook-prompt")

	// Add PreToolUse hook for AskUserQuestion forwarding
	// Use timeout 300000ms (5min) to allow time for user to answer via Telegram
	preToolAdded := addHookToEvent(hooks, "PreToolUse", cccPath+" hook-permission")

	// Add MessageDisplay hook for live-mode response streaming (self-gates: it
	// no-ops unless the session is in "live" integration mode).
	displayAdded := addHookToEvent(hooks, "MessageDisplay", cccPath+" hook-display")

	// Add SessionStart hook that injects the CCC environment+tools briefing.
	briefingAdded := addHookToEvent(hooks, "SessionStart", cccPath+" agent-briefing")

	// Add Notification hook to auto-answer rating/training prompts (so headless
	// agents don't hang on them).
	notifyAdded := addHookToEvent(hooks, "Notification", cccPath+" hook-notify")

	// Add StopFailure hook to auto-recover from transient API rate-limit errors.
	stopFailAdded := addHookToEvent(hooks, "StopFailure", cccPath+" hook-stopfailure")

	settings["hooks"] = hooks

	// Disable Claude Code's session-quality survey for headless agents: the
	// "How is Claude doing this session?" rating prompt and its transcript-share
	// follow-up are interactive TUI modals that block an unattended agent (they
	// do NOT fire a Notification hook, so they can't be auto-answered). Setting
	// CLAUDE_CODE_DISABLE_FEEDBACK_SURVEY=1 in the settings env block suppresses
	// the whole survey flow while leaving telemetry intact. Takes effect for
	// sessions started after this runs.
	env, ok := settings["env"].(map[string]interface{})
	if !ok {
		env = map[string]interface{}{}
	}
	envAdded := false
	if env["CLAUDE_CODE_DISABLE_FEEDBACK_SURVEY"] != "1" {
		env["CLAUDE_CODE_DISABLE_FEEDBACK_SURVEY"] = "1"
		envAdded = true
	}
	settings["env"] = env

	// Pre-accept Bypass Permissions mode so a fresh account config dir doesn't
	// block headless agents on the one-time "Yes, I accept" prompt (which
	// --dangerously-skip-permissions itself does not suppress). The default
	// ~/.claude already has this; a secondary account dir needs it too.
	if settings["skipDangerousModePermissionPrompt"] != true {
		settings["skipDangerousModePermissionPrompt"] = true
		envAdded = true
	}

	newData, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal settings: %w", err)
	}

	if err := os.WriteFile(settingsPath, newData, 0600); err != nil {
		return fmt.Errorf("failed to write settings.json: %w", err)
	}

	if stopAdded || promptAdded || preToolAdded || displayAdded || briefingAdded || notifyAdded || stopFailAdded || envAdded {
		fmt.Println("✅ Claude hooks installed!")
		if promptAdded {
			fmt.Println("  + UserPromptSubmit hook (turn-start / work-state)")
		}
		if stopAdded {
			fmt.Println("  + Stop hook (response capture)")
		}
		if preToolAdded {
			fmt.Println("  + PreToolUse hook (AskUserQuestion forwarding)")
		}
		if displayAdded {
			fmt.Println("  + MessageDisplay hook (live streaming)")
		}
		if briefingAdded {
			fmt.Println("  + SessionStart hook (agent briefing)")
		}
		if notifyAdded {
			fmt.Println("  + Notification hook (auto-answer rating/training prompts)")
		}
		if stopFailAdded {
			fmt.Println("  + StopFailure hook (auto-recover from rate-limit errors)")
		}
		if envAdded {
			fmt.Println("  + env CLAUDE_CODE_DISABLE_FEEDBACK_SURVEY=1 (suppress rating/training prompts)")
		}
	} else {
		fmt.Println("✅ Claude hooks already installed")
	}
	return nil
}

// allHookEvents lists the Claude Code hook events trace mode registers the
// passive logger on for one test agent. It covers the classic stable set plus
// the newer high-value events for our goals: interactive prompts
// (PermissionRequest/Elicitation/Notification), progress streaming
// (PostToolUse*/MessageDisplay) and reliability (StopFailure). Unknown event
// names on older Claude Code versions are simply never fired, not an error.
var allHookEvents = []string{
	"SessionStart", "SessionEnd",
	"UserPromptSubmit",
	"PreToolUse", "PostToolUse", "PostToolUseFailure", "PostToolBatch",
	"PermissionRequest", "PermissionDenied",
	"Elicitation", "ElicitationResult",
	"Notification", "MessageDisplay",
	"SubagentStart", "SubagentStop",
	"Stop", "StopFailure",
	"PreCompact", "PostCompact",
}

// removeHookFromEvent removes a hook command from an event. Returns true if it
// removed anything. Empties are pruned so the settings stay clean.
func removeHookFromEvent(hooks map[string]interface{}, eventName, command string) bool {
	entries, ok := hooks[eventName].([]interface{})
	if !ok {
		return false
	}
	changed := false
	var keptEntries []interface{}
	for _, entry := range entries {
		entryMap, ok := entry.(map[string]interface{})
		if !ok {
			keptEntries = append(keptEntries, entry)
			continue
		}
		hooksList, _ := entryMap["hooks"].([]interface{})
		var kept []interface{}
		for _, h := range hooksList {
			if hm, ok := h.(map[string]interface{}); ok && hm["command"] == command {
				changed = true
				continue
			}
			kept = append(kept, h)
		}
		if len(kept) == 0 {
			continue // drop now-empty matcher entry
		}
		entryMap["hooks"] = kept
		keptEntries = append(keptEntries, entryMap)
	}
	if len(keptEntries) == 0 {
		delete(hooks, eventName)
	} else {
		hooks[eventName] = keptEntries
	}
	return changed
}

// installHookTrace adds (enable) or removes (disable) the passive `ccc
// hook-trace` logger on ALL hook events in a project's .claude/settings.json,
// scoping trace mode to that single agent. The user-global settings.json (the
// real Stop/PreToolUse hooks) is left untouched.
func installHookTrace(projectDir string, enable bool) error {
	abs, err := filepath.Abs(projectDir)
	if err != nil {
		return err
	}
	if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
		return fmt.Errorf("not a directory: %s", abs)
	}
	home, _ := os.UserHomeDir()
	cccPath := filepath.Join(home, "bin", "ccc")
	command := cccPath + " hook-trace"

	claudeDir := filepath.Join(abs, ".claude")
	if err := os.MkdirAll(claudeDir, 0755); err != nil {
		return err
	}
	settingsPath := filepath.Join(claudeDir, "settings.json")

	settings := map[string]interface{}{}
	if data, err := os.ReadFile(settingsPath); err == nil {
		if err := json.Unmarshal(data, &settings); err != nil {
			return fmt.Errorf("parse %s: %w", settingsPath, err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	hooks, ok := settings["hooks"].(map[string]interface{})
	if !ok {
		hooks = map[string]interface{}{}
	}

	changed := false
	for _, ev := range allHookEvents {
		if enable {
			if addHookToEvent(hooks, ev, command) {
				changed = true
			}
		} else {
			if removeHookFromEvent(hooks, ev, command) {
				changed = true
			}
		}
	}
	settings["hooks"] = hooks

	newData, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(settingsPath, newData, 0644); err != nil {
		return err
	}

	state := "enabled"
	if !enable {
		state = "disabled"
	}
	if !changed {
		fmt.Printf("✅ Hook trace already %s for %s\n", state, abs)
		return nil
	}
	fmt.Printf("✅ Hook trace %s for %s\n", state, abs)
	fmt.Printf("   settings: %s\n", settingsPath)
	if enable {
		fmt.Printf("   trace log: %s\n", hookTracePath())
		fmt.Printf("   events:    %s\n", strings.Join(allHookEvents, ", "))
		fmt.Printf("   NOTE: restart the agent's Claude session so it reloads settings.\n")
	}
	return nil
}

// findCCCSourceDir locates the CCC source directory by looking for go.mod + .git
// in known locations. Returns empty string if not found.
func findCCCSourceDir() string {
	home, _ := os.UserHomeDir()
	candidates := []string{
		filepath.Join(home, "Projects", "teleclaude", "ccc"), // wagok-server
		filepath.Join(home, "Projects", "ccc"),               // msi, XPS
		filepath.Join(home, "Public", "Tools", "ccc"),        // dell17
	}
	// Also check directory of the running binary
	if exe, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			candidates = append([]string{filepath.Dir(resolved)}, candidates...)
		}
	}
	for _, dir := range candidates {
		goMod := filepath.Join(dir, "go.mod")
		gitDir := filepath.Join(dir, ".git")
		if _, err := os.Stat(goMod); err == nil {
			if _, err := os.Stat(gitDir); err == nil {
				return dir
			}
		}
	}
	return ""
}

// handleUpdateCmd performs git pull + go build + backup + replace + restart.
// Runs in a goroutine to avoid blocking the Telegram polling loop.
func handleUpdateCmd(cfg *Config, chatID int64, threadID int64) {
	defer atomic.StoreInt32(&updateInProgress, 0)

	sourceDir := findCCCSourceDir()
	if sourceDir == "" {
		sendMessage(cfg, chatID, threadID, "❌ Source directory not found (need go.mod + .git)")
		return
	}

	// Step 1: git pull
	sendMessage(cfg, chatID, threadID, fmt.Sprintf("📦 Pulling from git...\n`%s`", sourceDir))
	pullCmd := exec.Command("git", "-C", sourceDir, "pull")
	pullOut, err := pullCmd.CombinedOutput()
	pullText := strings.TrimSpace(string(pullOut))
	if err != nil {
		sendMessage(cfg, chatID, threadID, fmt.Sprintf("❌ git pull failed:\n```\n%s\n```", pullText))
		return
	}

	if pullText == "Already up to date." {
		sendMessage(cfg, chatID, threadID, "✅ Already up to date, no rebuild needed.")
		return
	}

	// Step 2: go build
	sendMessage(cfg, chatID, threadID, "🔨 Building...")
	newBinary := filepath.Join(sourceDir, "ccc-new")
	buildCmd := exec.Command("go", "build", "-o", newBinary, ".")
	buildCmd.Dir = sourceDir
	buildOut, err := buildCmd.CombinedOutput()
	if err != nil {
		buildText := strings.TrimSpace(string(buildOut))
		if len(buildText) > 3000 {
			buildText = buildText[:3000] + "\n..."
		}
		sendMessage(cfg, chatID, threadID, fmt.Sprintf("❌ Build failed:\n```\n%s\n```", buildText))
		os.Remove(newBinary)
		return
	}

	// Step 3: Validate new binary (must be > 1MB)
	if stat, err := os.Stat(newBinary); err != nil || stat.Size() < 1*1024*1024 {
		sendMessage(cfg, chatID, threadID, "❌ Built binary too small or missing, aborting")
		os.Remove(newBinary)
		return
	}

	// Step 4: Find current binary and backup
	exe, err := os.Executable()
	if err != nil {
		sendMessage(cfg, chatID, threadID, fmt.Sprintf("❌ Cannot find current binary: %v", err))
		os.Remove(newBinary)
		return
	}
	exePath, _ := filepath.EvalSymlinks(exe)
	backupPath := exePath + ".bak"

	// Copy current binary as backup (ignore error if first run)
	exec.Command("cp", exePath, backupPath).Run()

	// Step 5: Replace binary (rm + cp to handle "text file busy")
	os.Remove(exePath)
	cpCmd := exec.Command("cp", newBinary, exePath)
	if err := cpCmd.Run(); err != nil {
		// Restore from backup
		sendMessage(cfg, chatID, threadID, fmt.Sprintf("❌ Failed to replace binary: %v\nRestoring backup...", err))
		exec.Command("cp", backupPath, exePath).Run()
		os.Remove(newBinary)
		return
	}
	os.Chmod(exePath, 0755)
	os.Remove(newBinary)

	sendMessage(cfg, chatID, threadID, fmt.Sprintf("✅ Updated!\n```\n%s\n```\n🔄 Restarting...", pullText))

	// Step 6: Exit and let systemd restart the service with the new binary
	os.Exit(0)
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
			{"command": "mode", "description": "Integration mode: /mode legacy|live"},
			{"command": "stream", "description": "Toggle live streaming: /stream on|off"},
			{"command": "host", "description": "Manage hosts: /host add|del|list|check"},
			{"command": "rc", "description": "Remote command: /rc <host> <cmd>"},
			{"command": "setdir", "description": "Set projects dir: /setdir [host:]<path>"},
			{"command": "away", "description": "Toggle notifications"},
			{"command": "c", "description": "Local command: /c <cmd>"},
			{"command": "screenshot", "description": "Take screenshot of display"},
			{"command": "ping", "description": "Check bot status"},
			{"command": "update", "description": "Pull, build and restart CCC"},
			{"command": "restart", "description": "Restart CCC process"}
		]
	}`

	resp, err := http.Post(
		fmt.Sprintf("https://api.telegram.org/bot%s/setMyCommands", botToken),
		"application/json",
		strings.NewReader(commands),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to set bot commands: %v\n", redactTokenError(err, botToken))
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

	if err := os.WriteFile(servicePath, []byte(service), 0600); err != nil {
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
		resp, err := telegramGet(botToken, fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates?offset=%d&timeout=30", botToken, offset))
		if err != nil {
			return fmt.Errorf("failed to get updates: %w", err)
		}

		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
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
		resp, err := telegramClientGet(client, config.BotToken, reqURL)
		if err != nil {
			continue
		}

		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
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
		resp, err := telegramClientGet(client, config.BotToken, reqURL)
		if err != nil {
			return redactTokenError(err, config.BotToken)
		}

		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
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
				return sendMessage(config, groupChatID(config, sessionGroup(info)), info.TopicID, message)
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

	// Initialize message ID counter from history (CLI subprocess doesn't run listen())
	initMessageIDCounter()

	config, err := loadConfig()
	if err != nil {
		logHook("Remote", "ERROR: not configured: %v", err)
		return fmt.Errorf("not configured: %v", err)
	}
	setWebhookConfig(config)

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

	// Find session matching fromHost and path (exact match first, then subdirectory)
	var subdirMatch string
	var subdirInfo *SessionInfo
	for name, info := range config.Sessions {
		if info == nil || info.Host != fromHost {
			continue
		}
		// Exact match
		if info.Path == projectPath {
			// Skip prompt messages that were just sent from Telegram (cooldown 10s)
			if strings.HasPrefix(message, "💬") && wasTelegramSent(info.TopicID) {
				logHook("Remote", "skipping prompt (telegram cooldown) session=%s topic=%d", name, info.TopicID)
				return nil
			}
			logHook("Remote", "matched session=%s topic=%d, sending", name, info.TopicID)
			fmt.Printf("[remote] from=%s session=%s\n", fromHost, name)
			histFrom, histText := parseRemoteMessagePrefix(message)
			appendHistoryDedup(info.TopicID, histFrom, histText)
			return sendMessage(config, groupChatID(config, sessionGroup(info)), info.TopicID, message)
		}
		// Subdirectory match: projectPath is under this session's path
		if strings.HasPrefix(projectPath, info.Path+"/") {
			// Pick the longest (most specific) parent path
			if subdirInfo == nil || len(info.Path) > len(subdirInfo.Path) {
				subdirMatch = name
				subdirInfo = info
			}
		}
	}

	// Use subdirectory match if found (cwd is inside an existing session's project)
	if subdirInfo != nil {
		if strings.HasPrefix(message, "💬") && wasTelegramSent(subdirInfo.TopicID) {
			logHook("Remote", "skipping prompt (telegram cooldown) session=%s topic=%d", subdirMatch, subdirInfo.TopicID)
			return nil
		}
		logHook("Remote", "subdir match session=%s topic=%d (cwd=%s)", subdirMatch, subdirInfo.TopicID, projectPath)
		fmt.Printf("[remote] from=%s session=%s (subdir match)\n", fromHost, subdirMatch)
		histFrom, histText := parseRemoteMessagePrefix(message)
		appendHistoryDedup(subdirInfo.TopicID, histFrom, histText)
		return sendMessage(config, groupChatID(config, sessionGroup(subdirInfo)), subdirInfo.TopicID, message)
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
	return sendMessage(config, sessionGroupChatID(config, fullName), topicID, message)
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
	// Acquire exclusive lock to prevent multiple instances
	lockPath := filepath.Join(os.Getenv("HOME"), ".ccc.lock")
	var lockErr error
	lockFile, lockErr = os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if lockErr != nil {
		return fmt.Errorf("failed to open lock file: %w", lockErr)
	}
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lockFile.Close()
		lockFile = nil
		return fmt.Errorf("another ccc listen instance is already running (lock: %s)", lockPath)
	}
	// lockFile is kept open at package level — lock released when process exits

	config, err := loadConfig()
	if err != nil {
		return fmt.Errorf("not configured. Run: ccc setup <bot_token>")
	}
	setWebhookConfig(config)

	fmt.Printf("Bot listening... (chat: %d, group: %d)\n", config.ChatID, config.GroupID)
	fmt.Printf("Active sessions: %d\n", len(config.Sessions))
	fmt.Println("Press Ctrl+C to stop")

	// Initialize message ID counter from history
	initMessageIDCounter()

	// Start Unix socket API server
	if err := startSocketServer(config); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to start API socket: %v\n", err)
	}

	// Start the inter-agent mail delivery-deadline engine (smart secretary).
	if err := initMailScheduler(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to start mail scheduler: %v\n", err)
	}

	// Prepare the smart secretary (working dir + template + topic). Idempotent;
	// does not launch the session (that is `ccc secretary start` / on-demand).
	if err := bootstrapSecretary(config); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: secretary bootstrap: %v\n", err)
	}
	// Supervise the secretary once Vlad has enabled it (thin crash-restart net).
	go watchSecretary()

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
		// Reload config from disk to pick up sessions created by external
		// processes (hook CLIs, handleRemoteMessage, etc.)
		if freshCfg, err := loadConfig(); err == nil {
			config = freshCfg
			setWebhookConfig(config)
		}

		reqURL := fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates?offset=%d&timeout=30", config.BotToken, offset)
		resp, err := telegramClientGet(client, config.BotToken, reqURL)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Network error: %v (retrying...)\n", err)
			time.Sleep(5 * time.Second)
			continue
		}

		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
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

			// Handle callback queries (button presses from inline keyboards)
			if update.CallbackQuery != nil {
				cb := update.CallbackQuery
				// Every button press is logged on arrival: a press that goes
				// nowhere used to leave no trace at all, which made the whole
				// path undiagnosable from the outside.
				hasMarkup := cb.Message != nil && cb.Message.ReplyMarkup != nil
				fmt.Fprintf(os.Stderr, "[callback] recv data=%q from=%d msg=%v markup=%v\n",
					cb.Data, cb.From.ID, cb.Message != nil, hasMarkup)

				// Only accept from authorized user
				if cb.From.ID != config.ChatID {
					fmt.Fprintf(os.Stderr, "[callback] ignored: from=%d is not the admin (chat_id=%d)\n", cb.From.ID, config.ChatID)
					continue
				}

				answerCallbackQuery(config, cb.ID)

				// Parse callback data: session:questionIndex:totalQuestions:optionIndex
				// Legacy format (3 parts): session:questionIndex:optionIndex
				parts := strings.Split(cb.Data, ":")

				// multiSelect callbacks: <session...>:<qIdx>:<total>:<optIdx>:<m|x>.
				// Parse right-anchored so session names containing ':' (host:project)
				// still resolve correctly.
				if n := len(parts); n >= 5 && (parts[n-1] == "m" || parts[n-1] == "x") {
					fmt.Fprintf(os.Stderr, "[callback] multiselect action=%q session=%q\n", parts[n-1], strings.Join(parts[:n-4], ":"))
					handleMultiSelectCallback(config, cb, strings.Join(parts[:n-4], ":"), parts[n-1])
					continue
				}

				sessionName, questionIndex, totalQuestions, optionIndex, parsed := parseChoiceCallback(config, cb.Data)
				if !parsed {
					fmt.Fprintf(os.Stderr, "[callback] unparseable data=%q\n", cb.Data)
				}
				if parsed {

					// Edit message to show selection and remove buttons
					if cb.Message != nil {
						originalText := cb.Message.Text
						newText := fmt.Sprintf("%s\n\n✓ Selected option %d", originalText, optionIndex+1)
						editMessageRemoveKeyboard(config, cb.Message.Chat.ID, cb.Message.MessageID, newText)
					}

					// Store answer in history
					if cb.Message != nil {
						appendHistoryDedup(cb.Message.MessageThreadID, "human", fmt.Sprintf("Selected option %d", optionIndex+1))
					}

					// Resolve tmux session name and check local/remote. On a
					// remote host the tmux session is named after the project
					// only (claude-<project>), not "claude-host:project".
					info, exists := config.Sessions[sessionName]
					_, projectName := parseSessionTarget(sessionName)
					tmuxName := tmuxSessionName(extractProjectName(projectName))

					sendTmuxKeys := func(keys ...string) {
						if exists && info.Host != "" {
							// Remote session — send via SSH
							address := getHostAddress(config, info.Host)
							for _, key := range keys {
								cmd := fmt.Sprintf("tmux send-keys -t %s %s", shellQuote(tmuxName), key)
								runSSH(address, cmd, 5*time.Second)
							}
						} else {
							// Local session
							for _, key := range keys {
								tmuxCmd("send-keys", "-t", tmuxName, key).Run()
							}
						}
					}

					// Check session exists
					sessionExists := false
					if exists && info.Host != "" {
						address := getHostAddress(config, info.Host)
						sessionExists = sshTmuxHasSession(address, tmuxName)
					} else {
						sessionExists = tmuxSessionExists(tmuxName)
					}

					if sessionExists {
						// Send arrow down keys to select option, then Enter
						for i := 0; i < optionIndex; i++ {
							sendTmuxKeys("Down")
							time.Sleep(50 * time.Millisecond)
						}
						sendTmuxKeys("Enter")
						fmt.Fprintf(os.Stderr, "[callback] Selected option %d for %s (question %d/%d)\n", optionIndex, sessionName, questionIndex+1, totalQuestions)

						// Mark answered in pending questions (for API sync)
						if val, ok := pendingQuestions.Load(sessionName); ok {
							pqs := val.(*PendingQuestionSet)
							if questionIndex < len(pqs.Questions) {
								pqs.Questions[questionIndex].Answered = true
								pqs.Questions[questionIndex].AnswerIndex = optionIndex
							}
						}

						// After the last question, send Enter to confirm "Submit answers"
						if totalQuestions > 0 && questionIndex == totalQuestions-1 {
							time.Sleep(300 * time.Millisecond)
							sendTmuxKeys("Enter")
							pendingQuestions.Delete(sessionName)
							fmt.Fprintf(os.Stderr, "[callback] Auto-submitted answers for %s\n", sessionName)
						}
					} else {
						// Silence here used to hide a parsing bug: log the miss so a
						// button press that goes nowhere leaves a trace.
						fmt.Fprintf(os.Stderr, "[callback] no live tmux session %q for %s (data=%q)\n", tmuxName, sessionName, cb.Data)
					}
				}
				continue
			}

			msg := update.Message

			// Authorization: the admin (config.ChatID) may do anything, incl.
			// slash commands. Other people in the (private) group may send plain
			// messages to agents, but never commands and never outside a topic.
			isAdmin := msg.From.ID == config.ChatID
			if !isAdmin {
				if strings.HasPrefix(strings.TrimSpace(msg.Text), "/") {
					continue // slash commands are admin-only
				}
				if msg.Chat.Type != "supergroup" || msg.MessageThreadID == 0 {
					continue // non-admins are only allowed inside group topics
				}
			}

			// Deduplicate: Telegram forum groups can send two updates with
			// different update_id but the same message_id for a single message.
			if isMessageProcessed(msg.MessageID) {
				continue
			}

			chatID := msg.Chat.ID
			threadID := msg.MessageThreadID
			isGroup := msg.Chat.Type == "supergroup"
			// Tag injected human messages with the sender so multi-human groups
			// stay attributable (empty for the primary admin — unchanged UX).
			senderTag := humanTag(msg.From.ID, msg.From.FirstName, msg.From.Username)

			// Handle voice messages OFF the main loop: transcription takes
			// seconds, and doing it inline blocks all other updates (commands,
			// other agents) until it finishes. Run it in a goroutine, serialized
			// per topic so two voices to the same agent keep their order.
			if msg.Voice != nil && isGroup && threadID > 0 {
				fileID, username := msg.Voice.FileID, msg.From.Username
				go func() {
					l := topicLock(threadID)
					l.Lock()
					defer l.Unlock()
					handleVoiceMessage(chatID, threadID, senderTag, fileID, username)
				}()
				continue
			}

			// Handle photo messages
			if len(msg.Photo) > 0 && isGroup && threadID > 0 {
				config, _ = loadConfig()
				sessionName := getSessionByGroupTopic(config, chatID, threadID)
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
						sshTmuxSendKeys(hostInfo.Address, tmuxName, senderTag+prompt)
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
						sendToTmuxWithDelay(tmuxName, senderTag+prompt, 2*time.Second)
					}
				}
				continue
			}

			// Document/file messages (a file with an optional caption)
			if msg.Document != nil && isGroup && threadID > 0 {
				config, _ = loadConfig()
				sessionName := getSessionByGroupTopic(config, chatID, threadID)
				if sessionName != "" {
					sessionInfo := config.Sessions[sessionName]
					hostName := ""
					if sessionInfo != nil {
						hostName = sessionInfo.Host
					}
					_, projectName := parseSessionTarget(sessionName)
					tmuxName := tmuxSessionName(extractProjectName(projectName))

					fileName := msg.Document.FileName
					if fileName == "" {
						fileName = "file"
					}
					docPath := filepath.Join(os.TempDir(), fmt.Sprintf("telegram_%d_%s", time.Now().UnixNano(), filepath.Base(fileName)))
					if err := downloadTelegramFile(config, msg.Document.FileID, docPath); err != nil {
						sendMessage(config, chatID, threadID, fmt.Sprintf("❌ Download failed: %v", err))
						continue
					}

					caption := msg.Caption
					if caption == "" {
						caption = "Here is a file:"
					}

					if hostName != "" {
						hostInfo := config.Hosts[hostName]
						if hostInfo == nil {
							sendMessage(config, chatID, threadID, fmt.Sprintf("❌ Host %s not found in config", hostName))
							continue
						}
						if !isClaudeRunning(tmuxName, hostInfo.Address) {
							sendMessage(config, chatID, threadID, "🔄 Session interrupted, restarting...")
							if !restartClaudeInSession(tmuxName, hostInfo.Address) {
								sendMessage(config, chatID, threadID, "❌ Failed to restart Claude. Use /continue to restart manually.")
								continue
							}
							sendMessage(config, chatID, threadID, "✅ Session restarted")
						}
						sendMessage(config, chatID, threadID, "📎 Transferring file to remote host...")
						if err := scpToHost(hostInfo.Address, docPath, docPath, 60*time.Second); err != nil {
							sendMessage(config, chatID, threadID, fmt.Sprintf("❌ SCP failed: %v", err))
							continue
						}
						appendHistory(threadID, HistoryMessage{
							ID: nextMessageID(), Timestamp: time.Now().Unix(),
							From: "human", Type: "document", Path: docPath, Caption: caption, Username: msg.From.Username,
						})
						startContinuousTyping(config, chatID, threadID, sessionName)
						sshTmuxSendKeys(hostInfo.Address, tmuxName, fmt.Sprintf("%s%s %s", senderTag, caption, docPath))
						os.Remove(docPath)
						continue
					}

					// Local session
					if tmuxSessionExists(tmuxName) {
						if !isClaudeRunning(tmuxName, "") {
							sendMessage(config, chatID, threadID, "🔄 Session interrupted, restarting...")
							if !restartClaudeInSession(tmuxName, "") {
								sendMessage(config, chatID, threadID, "❌ Failed to restart Claude. Use /continue to restart manually.")
								continue
							}
							sendMessage(config, chatID, threadID, "✅ Session restarted")
						}
						appendHistory(threadID, HistoryMessage{
							ID: nextMessageID(), Timestamp: time.Now().Unix(),
							From: "human", Type: "document", Path: docPath, Caption: caption, Username: msg.From.Username,
						})
						sendMessage(config, chatID, threadID, "📎 File saved, sending to Claude...")
						startContinuousTyping(config, chatID, threadID, sessionName)
						sendToTmuxWithDelay(tmuxName, fmt.Sprintf("%s%s %s", senderTag, caption, docPath), 2*time.Second)
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

*Integration (per topic):*
• /mode \[legacy|live\] — Show/set integration mode
• /stream \[on|off\] — Toggle live response streaming (live mode)

*Settings:*
• /setdir \[host:\]<path> — Set projects directory
• /away — Toggle notifications
• /c <cmd> — Run local command
• /ping — Check bot status
• /update — Pull, build and restart CCC
• /restart — Restart CCC process`
				sendMessage(config, chatID, threadID, helpText)
				continue
			}

			if text == "/ping" {
				sendMessage(config, chatID, threadID, "pong!")
				continue
			}

			// /groupid - report this chat's id (to register a new project group)
			if text == "/groupid" {
				configured := chatID == config.GroupID
				for _, g := range config.Groups {
					if g != nil && g.ChatID == chatID {
						configured = true
					}
				}
				note := "not yet in CCC — add it to ~/.ccc.json under \"groups\""
				if configured {
					note = "already configured"
				}
				sendMessage(config, chatID, threadID, fmt.Sprintf("chat_id: %d\ntopic_id: %d\n(%s)", chatID, threadID, note))
				continue
			}

			// /addthisgroup <alias> - register THIS Telegram group as a project
			// group. Admin only; additive (only adds, never removes/moves).
			if strings.HasPrefix(text, "/addthisgroup") {
				if msg.From.ID != config.ChatID {
					continue // admin only
				}
				alias := strings.TrimSpace(strings.TrimPrefix(text, "/addthisgroup"))
				switch {
				case alias == "":
					sendMessage(config, chatID, threadID, "Usage: /addthisgroup <alias>")
				case !isGroup:
					sendMessage(config, chatID, threadID, "❌ Run this inside the Telegram group you want to register")
				case alias == "default":
					sendMessage(config, chatID, threadID, "❌ 'default' is reserved")
				case chatID == config.GroupID:
					sendMessage(config, chatID, threadID, "❌ This is the default group, already configured")
				default:
					already := ""
					for a, g := range config.Groups {
						if g != nil && g.ChatID == chatID {
							already = a
						}
					}
					if _, used := config.Groups[alias]; used {
						sendMessage(config, chatID, threadID, fmt.Sprintf("❌ Alias %q is already used", alias))
					} else if already != "" {
						sendMessage(config, chatID, threadID, fmt.Sprintf("❌ This group is already registered as %q", already))
					} else {
						if config.Groups == nil {
							config.Groups = map[string]*GroupInfo{}
						}
						config.Groups[alias] = &GroupInfo{ChatID: chatID}
						if err := saveConfig(config); err != nil {
							sendMessage(config, chatID, threadID, "❌ save failed: "+err.Error())
						} else {
							sendMessage(config, chatID, threadID, fmt.Sprintf("✅ Group %q registered (chat_id %d).\nNext on the server: `ccc secretary start %s` to launch its secretary, then `/changegroup %s` in a project's topic to move it here.", alias, chatID, alias, alias))
						}
					}
				}
				config, _ = loadConfig() // reload so routing picks up the new group
				continue
			}

			if text == "/restart" {
				sendMessage(config, chatID, threadID, "🔄 Restarting...")
				// Commit the offset to Telegram so the /restart message is not re-delivered
				// to the next process instance (prevents restart loop)
				commitURL := fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates?offset=%d&timeout=1", config.BotToken, offset)
				if commitResp, err := client.Get(commitURL); err == nil {
					commitResp.Body.Close()
				}
				// Just exit — systemd will restart the service
				os.Exit(0)
			}

			if text == "/update" {
				if !atomic.CompareAndSwapInt32(&updateInProgress, 0, 1) {
					sendMessage(config, chatID, threadID, "⏳ Update already in progress...")
					continue
				}
				go handleUpdateCmd(config, chatID, threadID)
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

			// /changegroup <alias> - move this topic's project to another group
			if strings.HasPrefix(text, "/changegroup") && isGroup {
				alias := strings.TrimSpace(strings.TrimPrefix(text, "/changegroup"))
				handleChangeGroup(config, chatID, threadID, alias)
				config, _ = loadConfig() // reload after the move
				continue
			}

			// /account [alias] - show or pin this group's subscription account (admin)
			if strings.HasPrefix(text, "/account") && isGroup {
				if msg.From.ID != config.ChatID {
					continue // admin only
				}
				arg := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(text, "/account")))
				handleAccountCommand(config, chatID, threadID, arg)
				config, _ = loadConfig() // reload after the change
				continue
			}

			// /mode [legacy|live] - show or set this topic's Claude-Code
			// integration mode (live = streaming/buttons/typing; legacy = robust
			// Stop-only). Admin-only; takes effect on the fly (hooks read config).
			if strings.HasPrefix(text, "/mode") && isGroup {
				if msg.From.ID != config.ChatID {
					continue // admin only
				}
				arg := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(text, "/mode")))
				sessionName := getSessionByGroupTopic(config, chatID, threadID)
				if sessionName == "" {
					sendMessage(config, chatID, threadID, "❌ No session mapped to this topic")
					continue
				}
				info := config.Sessions[sessionName]
				if info == nil {
					sendMessage(config, chatID, threadID, "❌ Session info not found")
					continue
				}
				if arg == "" {
					src := "global default"
					if info.IntegrationMode != "" {
						src = "session override"
					}
					sendMessage(config, chatID, threadID, fmt.Sprintf("⚙️ Integration mode for *%s*: `%s` (%s)\nUsage: /mode legacy|live", sessionName, sessionIntegrationMode(config, info), src))
					continue
				}
				if !validIntegrationMode(arg) {
					sendMessage(config, chatID, threadID, "❌ Unknown mode. Use: /mode legacy|live")
					continue
				}
				info.IntegrationMode = arg
				if err := saveConfig(config); err != nil {
					sendMessage(config, chatID, threadID, "❌ save failed: "+err.Error())
					continue
				}
				sendMessage(config, chatID, threadID, fmt.Sprintf("✅ Integration mode for *%s* set to `%s`.", sessionName, arg))
				continue
			}

			// /stream [on|off] - toggle live response streaming for this topic's
			// session (only meaningful in live mode). Admin-only; on the fly.
			if strings.HasPrefix(text, "/stream") && isGroup {
				if msg.From.ID != config.ChatID {
					continue // admin only
				}
				arg := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(text, "/stream")))
				sessionName := getSessionByGroupTopic(config, chatID, threadID)
				if sessionName == "" {
					sendMessage(config, chatID, threadID, "❌ No session mapped to this topic")
					continue
				}
				info := config.Sessions[sessionName]
				if info == nil {
					sendMessage(config, chatID, threadID, "❌ Session info not found")
					continue
				}
				if arg == "" {
					state := "on"
					if info.StreamOff {
						state = "off"
					}
					note := ""
					if !isSessionLive(config, info) {
						note = " (note: only active in `live` mode — see /mode)"
					}
					sendMessage(config, chatID, threadID, fmt.Sprintf("📡 Streaming for *%s*: `%s`%s\nUsage: /stream on|off", sessionName, state, note))
					continue
				}
				switch arg {
				case "on":
					info.StreamOff = false
				case "off":
					info.StreamOff = true
				default:
					sendMessage(config, chatID, threadID, "❌ Usage: /stream on|off")
					continue
				}
				if err := saveConfig(config); err != nil {
					sendMessage(config, chatID, threadID, "❌ save failed: "+err.Error())
					continue
				}
				note := ""
				if arg == "on" && !isSessionLive(config, info) {
					note = "\n⚠️ Streaming is only active in `live` mode — run /mode live."
				}
				sendMessage(config, chatID, threadID, fmt.Sprintf("✅ Streaming for *%s* set to `%s`.%s", sessionName, arg, note))
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
				sessionName := getSessionByGroupTopic(config, chatID, threadID)
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
				sessionName := getSessionByGroupTopic(config, chatID, threadID)
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
				if err := editForumTopic(config, chatID, threadID, name); err != nil {
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
				deleteErr := deleteForumTopic(config, chatID, oldTopicID)
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
						topicID, err = createForumTopic(config, chatID, fullName)
						if err != nil {
							sendMessage(config, chatID, threadID, fmt.Sprintf("❌ Failed to create topic: %v", err))
							continue
						}

						// Resolve work directory path
						workDir, err = resolveSessionPath(config, hostName, projectName)
						if err != nil {
							sendMessage(config, chatID, topicID, fmt.Sprintf("❌ Failed to resolve path: %v", err))
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
							sendMessage(config, chatID, topicID, fmt.Sprintf("❌ Failed to create directory: %v", err))
							continue
						}

						// Create tmux session on remote host
						if err := sshTmuxNewSession(address, tmuxName, workDir, continueSession); err != nil {
							sendMessage(config, chatID, topicID, fmt.Sprintf("❌ Failed to start tmux: %v", err))
						} else {
							time.Sleep(500 * time.Millisecond)
							if sshTmuxHasSession(address, tmuxName) {
								sendMessage(config, chatID, topicID, fmt.Sprintf("🚀 Session '%s' started on %s!\n\nSend messages here to interact with Claude.", fullName, hostName))
							} else {
								sendMessage(config, chatID, topicID, fmt.Sprintf("⚠️ Session '%s' created but died immediately. Check if claude works on %s.", fullName, hostName))
							}
						}
					} else {
						// Local session
						if _, err := os.Stat(workDir); os.IsNotExist(err) {
							os.MkdirAll(workDir, 0755)
						}

						if err := createTmuxSession(tmuxName, workDir, continueSession); err != nil {
							sendMessage(config, chatID, topicID, fmt.Sprintf("❌ Failed to start tmux: %v", err))
						} else {
							time.Sleep(500 * time.Millisecond)
							if tmuxSessionExists(tmuxName) {
								sendMessage(config, chatID, topicID, fmt.Sprintf("🚀 Session '%s' started!\n\nSend messages here to interact with Claude.", fullName))
							} else {
								sendMessage(config, chatID, topicID, fmt.Sprintf("⚠️ Session '%s' created but died immediately. Check if ~/bin/ccc works.", fullName))
							}
						}
					}
					continue
				}

				// Without args - restart session in current topic
				if threadID > 0 {
					sessionName := getSessionByGroupTopic(config, chatID, threadID)
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
				sessionName := getSessionByGroupTopic(config, chatID, threadID)
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

					// Ensure session is running (auto-start if stopped, auto-restart if crashed)
					if errMsg := ensureSessionRunning(config, sessionName, sessionInfo); errMsg != "" {
						sendMessage(config, chatID, threadID, fmt.Sprintf("❌ %s", errMsg))
						continue
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

					// Send to tmux (remote or local)
					var sendErr error
					if hostName != "" {
						address := getHostAddress(config, hostName)
						sendErr = sshTmuxSendKeys(address, tmuxName, senderTag+text)
					} else {
						sendErr = sendToTmux(tmuxName, senderTag+text)
					}
					if sendErr != nil {
						stopContinuousTyping(sessionName)
						sendMessage(config, chatID, threadID, fmt.Sprintf("❌ Failed to send: %v", sendErr))
					}
					// Background capture for remote sessions (fallback if client-mode forwarding is inactive)
					captureResponseAsync(config, sessionName, sessionInfo)
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
    ccc --group <g> [--account <a>]   Start this dir's agent in a Telegram group
                            and/or on a subscription account (see ACCOUNTS & GROUPS)
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

ACCOUNTS & GROUPS (server-local agents):
    ccc --group <alias> --account <alias>   Create/launch this dir's agent in a
                            Telegram group AND on a subscription account, one command.
                            Both flags optional; unknown group/account = error, no-op.
                            (Old manual way: ccc -> /changegroup <g> -> account
                             set-session <name> <a> -> relaunch.)
    account list                       List accounts + group and session assignments
    account add <alias> <config-dir>   Register an account (a CLAUDE_CONFIG_DIR)
    account set <group> <alias> [--migrate]   Pin a whole group to an account
    account set-session <name> <alias>        Pin one session to an account
    (New account first: CLAUDE_CONFIG_DIR=~/.claude-X claude  # then /login;
     then CLAUDE_CONFIG_DIR=~/.claude-X ccc install; then account add <alias> ~/.claude-X)

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
    /update                 Pull, build and restart CCC
    /restart                Restart CCC process
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
			if err := startSession(false, "", ""); err != nil {
				os.Exit(1)
			}
		}
		return
	}

	// Flag-style start: `ccc [--group <alias>] [--account <alias>] [-c]` — create
	// (or reuse) this dir's session directly in a group / on an account, one shot.
	if os.Args[1] == "--group" || os.Args[1] == "--account" {
		var group, account string
		cont := false
		a := os.Args[1:]
		for i := 0; i < len(a); i++ {
			switch a[i] {
			case "--group":
				if i+1 < len(a) {
					group = a[i+1]
					i++
				}
			case "--account":
				if i+1 < len(a) {
					account = a[i+1]
					i++
				}
			case "-c":
				cont = true
			}
		}
		config, _ := loadOrCreateConfig()
		if config.Mode == "client" && config.Server != "" && config.HostName != "" {
			fmt.Fprintln(os.Stderr, "note: --group/--account apply to server-local agents; ignored in client mode")
			if err := startClientSession(config, nil); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		} else if err := startSession(cont, group, account); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
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
			if err := startSession(true, "", ""); err != nil {
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

	case "mcp-secretary":
		// Stateless MCP (JSON-RPC over stdio) server registered in each agent's
		// .mcp.json; translates tool calls into Unix-socket requests to the
		// running CCC server. See mcp_secretary.go.
		if err := mcpSecretary(); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		return

	case "mcp-relay":
		// Server-side relay for client-mode shims: read one APIRequest from
		// stdin, run it against the local socket, write the APIResponse to
		// stdout. Invoked over SSH by a remote agent's mcp-secretary shim.
		if err := mcpRelay(); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		return

	case "secretary":
		// Manage the smart secretary agent: `ccc secretary [start|status]`.
		if err := secretaryCommand(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		return

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

	case "send-file":
		// Agent attaches a file to its Telegram topic: ccc send-file <path> [caption]
		if err := handleSendFileCmd(); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}

	case "agent-briefing":
		// SessionStart: inject the CCC environment+tools briefing into context.
		if err := handleAgentBriefing(); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}

	case "changegroup":
		// Admin: move a session to another group. ccc changegroup <session> <alias>
		if len(os.Args) < 4 {
			fmt.Fprintln(os.Stderr, "Usage: ccc changegroup <session-name> <group-alias>")
			os.Exit(1)
		}
		cfg, err := loadConfig()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		if err := changeGroupCore(cfg, os.Args[2], os.Args[3]); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("✅ Moved %s to group %s\n", os.Args[2], os.Args[3])

	case "account":
		// ccc account list | add <alias> <config-dir> | set <group> <alias|default>
		cfg, err := loadConfig()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		sub := ""
		if len(os.Args) > 2 {
			sub = os.Args[2]
		}
		switch sub {
		case "list":
			fmt.Println("Accounts (alias -> config dir):")
			if len(cfg.Accounts) == 0 {
				fmt.Println("  (none)")
			}
			for a, d := range cfg.Accounts {
				fmt.Printf("  %s -> %s\n", a, d)
			}
			fmt.Println("Group assignments:")
			any := false
			for g, gi := range cfg.Groups {
				if gi != nil && gi.Account != "" {
					fmt.Printf("  group %s -> %s\n", g, gi.Account)
					any = true
				}
			}
			if !any {
				fmt.Println("  (all groups use the default account)")
			}
			fmt.Println("Session overrides:")
			anyS := false
			for s, si := range cfg.Sessions {
				if si != nil && !si.Deleted && si.Account != "" {
					fmt.Printf("  session %s -> %s (group %s)\n", s, si.Account, sessionGroup(si))
					anyS = true
				}
			}
			if !anyS {
				fmt.Println("  (none)")
			}
		case "add":
			if len(os.Args) < 5 {
				fmt.Fprintln(os.Stderr, "Usage: ccc account add <alias> <config-dir>")
				os.Exit(1)
			}
			if cfg.Accounts == nil {
				cfg.Accounts = map[string]string{}
			}
			cfg.Accounts[os.Args[3]] = os.Args[4]
			saveConfig(cfg)
			fmt.Printf("✅ Account '%s' -> %s\n", os.Args[3], os.Args[4])
		case "set":
			if len(os.Args) < 5 {
				fmt.Fprintln(os.Stderr, "Usage: ccc account set <group> <alias|default> [--migrate]")
				os.Exit(1)
			}
			group, alias := os.Args[3], os.Args[4]
			migrate := false
			for _, a := range os.Args[5:] {
				if a == "--migrate" {
					migrate = true
				}
			}
			// Capture the group's current config dir BEFORE the switch so we know
			// where to copy history from when --migrate is set.
			oldDir := accountConfigDirOrDefault(accountDirForGroup(cfg, group))
			msg, err := setGroupAccount(cfg, group, alias)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
			fmt.Println("✅ " + msg)
			if migrate {
				newDir := accountConfigDirOrDefault(accountDirForGroup(cfg, group))
				if err := migrateGroupContext(cfg, group, oldDir, newDir); err != nil {
					fmt.Fprintf(os.Stderr, "migrate: %v\n", err)
				}
			}
		case "set-session":
			if len(os.Args) < 5 {
				fmt.Fprintln(os.Stderr, "Usage: ccc account set-session <session> <alias|default>")
				os.Exit(1)
			}
			msg, err := setSessionAccount(cfg, os.Args[3], os.Args[4])
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
			fmt.Println("✅ " + msg)
		default:
			fmt.Fprintln(os.Stderr, "Usage: ccc account list | add <alias> <config-dir> | set <group> <alias|default> [--migrate] | set-session <session> <alias|default>")
			os.Exit(1)
		}

	case "hook-display":
		// MessageDisplay: stream assistant deltas to Telegram (live mode).
		if err := handleDisplayHook(); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}

	case "hook-notify":
		// Notification: auto-answer rating/training prompts for headless agents.
		if err := handleNotifyHook(); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}

	case "hook-stopfailure":
		// StopFailure: auto-recover an agent whose turn died on a rate-limit error.
		if err := handleStopFailureHook(); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}

	case "hook-trace":
		// Passive logger wired onto every hook event for a test agent.
		if err := handleHookTrace(); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}

	case "hook-trace-on", "hook-trace-off":
		if len(os.Args) < 3 {
			fmt.Fprintf(os.Stderr, "Usage: ccc %s <project-dir>\n", os.Args[1])
			os.Exit(1)
		}
		if err := installHookTrace(os.Args[2], os.Args[1] == "hook-trace-on"); err != nil {
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
