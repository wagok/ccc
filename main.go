package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const version = "1.0.0"

// SessionInfo stores information about a session
type SessionInfo struct {
	TopicID int64  `json:"topic_id"`
	Path    string `json:"path"`
	Host    string `json:"host,omitempty"` // Remote host name or "" for local
}

// HostInfo stores information about a remote host
type HostInfo struct {
	Address     string `json:"address"`               // SSH target (user@host)
	ProjectsDir string `json:"projects_dir,omitempty"` // Base directory for projects on this host
}

// Config stores bot configuration and session mappings
type Config struct {
	BotToken         string                  `json:"bot_token"`
	ChatID           int64                   `json:"chat_id"`                     // Private chat for simple commands
	GroupID          int64                   `json:"group_id,omitempty"`          // Group with topics for sessions
	Sessions         map[string]*SessionInfo `json:"sessions,omitempty"`          // session name -> session info
	ProjectsDir      string                  `json:"projects_dir,omitempty"`      // Base directory for new projects (default: ~)
	TranscriptionCmd string                  `json:"transcription_cmd,omitempty"` // Command for audio transcription
	Away             bool                    `json:"away"`

	// Remote hosts configuration (server mode)
	Hosts map[string]*HostInfo `json:"hosts,omitempty"` // host name -> host info

	// Client mode configuration
	Mode     string `json:"mode,omitempty"`      // "client" or "" (server/standalone)
	Server   string `json:"server,omitempty"`    // SSH target for server (client mode)
	HostName string `json:"host_name,omitempty"` // This machine's identifier
}

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

func getConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".ccc.json")
}

// loadOrCreateConfig loads config or returns empty config if file doesn't exist
func loadOrCreateConfig() (*Config, error) {
	config, err := loadConfig()
	if err != nil {
		if os.IsNotExist(err) {
			return &Config{
				Sessions: make(map[string]*SessionInfo),
				Hosts:    make(map[string]*HostInfo),
			}, nil
		}
		return nil, err
	}
	return config, nil
}

func loadConfig() (*Config, error) {
	data, err := os.ReadFile(getConfigPath())
	if err != nil {
		return nil, err
	}

	// First check if this is old format (sessions as map[string]int64)
	var rawConfig map[string]json.RawMessage
	if err := json.Unmarshal(data, &rawConfig); err != nil {
		return nil, err
	}

	// Try to detect old sessions format
	var needsMigration bool
	var oldSessions map[string]int64
	if sessionsRaw, ok := rawConfig["sessions"]; ok {
		// Try to parse as old format (map of topic IDs)
		if json.Unmarshal(sessionsRaw, &oldSessions) == nil && len(oldSessions) > 0 {
			// Check if values are positive numbers (old format)
			for _, v := range oldSessions {
				if v > 0 {
					needsMigration = true
					break
				}
			}
		}
	}

	var config Config
	if needsMigration {
		// Parse everything except sessions first
		type ConfigWithoutSessions struct {
			BotToken    string `json:"bot_token"`
			ChatID      int64  `json:"chat_id"`
			GroupID     int64  `json:"group_id"`
			ProjectsDir string `json:"projects_dir"`
			Away        bool   `json:"away"`
		}
		var partial ConfigWithoutSessions
		json.Unmarshal(data, &partial)

		config.BotToken = partial.BotToken
		config.ChatID = partial.ChatID
		config.GroupID = partial.GroupID
		config.ProjectsDir = partial.ProjectsDir
		config.Away = partial.Away

		// Migrate sessions
		home, _ := os.UserHomeDir()
		config.Sessions = make(map[string]*SessionInfo)
		for name, topicID := range oldSessions {
			// For old sessions, try to figure out the path
			var sessionPath string
			if strings.HasPrefix(name, "/") {
				// Absolute path
				sessionPath = name
			} else if strings.HasPrefix(name, "~/") {
				// Home-relative path
				sessionPath = filepath.Join(home, name[2:])
			} else if config.ProjectsDir != "" {
				// Use projects_dir if set
				projectsDir := config.ProjectsDir
				if strings.HasPrefix(projectsDir, "~/") {
					projectsDir = filepath.Join(home, projectsDir[2:])
				}
				sessionPath = filepath.Join(projectsDir, name)
			} else {
				sessionPath = filepath.Join(home, name)
			}
			config.Sessions[name] = &SessionInfo{
				TopicID: topicID,
				Path:    sessionPath,
			}
		}
		// Save migrated config
		saveConfig(&config)
	} else {
		// Parse with new format
		if err := json.Unmarshal(data, &config); err != nil {
			return nil, err
		}
	}

	if config.Sessions == nil {
		config.Sessions = make(map[string]*SessionInfo)
	}

	return &config, nil
}

func saveConfig(config *Config) error {
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(getConfigPath(), data, 0600)
}

// getProjectsDir returns the base directory for projects
func getProjectsDir(config *Config) string {
	if config.ProjectsDir != "" {
		// Expand ~ to home directory
		if strings.HasPrefix(config.ProjectsDir, "~/") {
			home, _ := os.UserHomeDir()
			return filepath.Join(home, config.ProjectsDir[2:])
		}
		return config.ProjectsDir
	}
	home, _ := os.UserHomeDir()
	return home
}

// resolveProjectPath resolves the full path for a project
// If name starts with / or ~/, it's treated as absolute/home-relative path
// Otherwise, it's relative to projects_dir
func resolveProjectPath(config *Config, name string) string {
	// Absolute path
	if strings.HasPrefix(name, "/") {
		return name
	}
	// Home-relative path (~/something or just ~)
	if strings.HasPrefix(name, "~/") || name == "~" {
		home, _ := os.UserHomeDir()
		if name == "~" {
			return home
		}
		return filepath.Join(home, name[2:])
	}
	// Relative to projects_dir
	return filepath.Join(getProjectsDir(config), name)
}

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
func expandPath(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, path[2:])
	}
	return path
}

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
	_, err := runSSH(address, "mkdir -p "+shellQuote(path), time.Duration(sshCommandTimeout)*time.Second)
	return err
}

// sshDirExists checks if a directory exists on remote host
func sshDirExists(address string, path string) bool {
	_, err := runSSH(address, "test -d "+shellQuote(path), time.Duration(sshCommandTimeout)*time.Second)
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
	_, err := runSSH(address, sendCmd, time.Duration(sshCommandTimeout)*time.Second)
	return err
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
func getHostAddress(config *Config, hostName string) string {
	if hostName == "" {
		return ""
	}
	if config.Hosts == nil {
		return ""
	}
	if host, ok := config.Hosts[hostName]; ok {
		return host.Address
	}
	return ""
}

// getHostProjectsDir returns projects dir for a host
func getHostProjectsDir(config *Config, hostName string) string {
	if hostName == "" {
		return getProjectsDir(config)
	}
	if config.Hosts != nil {
		if host, ok := config.Hosts[hostName]; ok && host.ProjectsDir != "" {
			return host.ProjectsDir
		}
	}
	return "~"
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

func tmuxSessionExists(name string) bool {
	cmd := exec.Command(tmuxPath, "-S", tmuxSocket, "has-session", "-t", name)
	return cmd.Run() == nil
}

func createTmuxSession(name string, workDir string, continueSession bool) error {
	// Build the command to run inside tmux
	cccCmd := cccPath + " run"
	if continueSession {
		cccCmd += " -c"
	}

	// Create tmux session with a login shell (don't run command directly - it kills session on exit)
	args := []string{"-S", tmuxSocket, "new-session", "-d", "-s", name, "-c", workDir}
	cmd := exec.Command(tmuxPath, args...)
	if err := cmd.Run(); err != nil {
		return err
	}

	// Enable mouse mode for this session (allows scrolling)
	exec.Command(tmuxPath, "-S", tmuxSocket, "set-option", "-t", name, "mouse", "on").Run()

	// Send the command to the session via send-keys (preserves TTY properly)
	time.Sleep(200 * time.Millisecond)
	exec.Command(tmuxPath, "-S", tmuxSocket, "send-keys", "-t", name, cccCmd, "C-m").Run()

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
	tmuxName := "claude-" + name

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
			cmd := exec.Command(tmuxPath, "-S", tmuxSocket, "switch-client", "-t", tmuxName)
			cmd.Stdin = os.Stdin
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			return cmd.Run()
		}
		// Outside tmux: attach to existing session
		cmd := exec.Command(tmuxPath, "-S", tmuxSocket, "attach-session", "-t", tmuxName)
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
		cmd := exec.Command(tmuxPath, "-S", tmuxSocket, "switch-client", "-t", tmuxName)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}
	cmd := exec.Command(tmuxPath, "-S", tmuxSocket, "attach-session", "-t", tmuxName)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func sendToTmux(session string, text string) error {
	return sendToTmuxWithDelay(session, text, 50*time.Millisecond)
}

func sendToTmuxWithDelay(session string, text string, delay time.Duration) error {
	// Send text literally
	cmd := exec.Command(tmuxPath, "-S", tmuxSocket, "send-keys", "-t", session, "-l", text)
	if err := cmd.Run(); err != nil {
		return err
	}

	// Wait for content to load (e.g., images)
	time.Sleep(delay)

	// Send Enter twice (Claude Code needs double Enter)
	cmd = exec.Command(tmuxPath, "-S", tmuxSocket, "send-keys", "-t", session, "C-m")
	if err := cmd.Run(); err != nil {
		return err
	}
	time.Sleep(50 * time.Millisecond)
	cmd = exec.Command(tmuxPath, "-S", tmuxSocket, "send-keys", "-t", session, "C-m")
	return cmd.Run()
}

func killTmuxSession(name string) error {
	cmd := exec.Command(tmuxPath, "-S", tmuxSocket, "kill-session", "-t", name)
	return cmd.Run()
}

func listTmuxSessions() ([]string, error) {
	cmd := exec.Command(tmuxPath, "-S", tmuxSocket, "list-sessions", "-F", "#{session_name}")
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

// Session management

func sessionName(name string) string {
	return "claude-" + name
}

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

	if err := createTmuxSession(sessionName(name), workDir, false); err != nil {
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
	tmuxName := "claude-" + extractProjectName(projectName)

	// Kill tmux session (remote or local)
	if sessionInfo != nil && sessionInfo.Host != "" {
		address := getHostAddress(config, sessionInfo.Host)
		if address != "" {
			sshTmuxKillSession(address, tmuxName)
		}
	} else {
		killTmuxSession(tmuxName)
	}

	// Remove from config
	delete(config.Sessions, name)
	saveConfig(config)

	return nil
}

func getSessionByTopic(config *Config, topicID int64) string {
	for name, info := range config.Sessions {
		if info != nil && info.TopicID == topicID {
			return name
		}
	}
	return ""
}

// Hook handling

// forwardToServer forwards a message to the server in client mode
// Returns true if forwarded (client mode), false otherwise
func forwardToServer(config *Config, cwd string, message string) bool {
	if config.Mode != "client" || config.Server == "" || config.HostName == "" {
		return false
	}

	// Forward to server via SSH
	// Use base64 to safely encode the message
	encoded := base64.StdEncoding.EncodeToString([]byte(message))
	cmd := fmt.Sprintf("cd %s && ccc --from=%s \"$(echo %s | base64 -d)\"",
		shellQuote(cwd), shellQuote(config.HostName), encoded)

	fmt.Fprintf(os.Stderr, "hook: forwarding to server %s\n", config.Server)
	_, err := runSSH(config.Server, cmd, 10*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hook: forward error: %v\n", err)
	}
	return true
}

func handleHook() error {
	config, err := loadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "hook: no config\n")
		return nil
	}

	// Read hook data from stdin
	var hookData HookData
	decoder := json.NewDecoder(os.Stdin)
	if err := decoder.Decode(&hookData); err != nil {
		fmt.Fprintf(os.Stderr, "hook: decode error: %v\n", err)
		return nil
	}

	fmt.Fprintf(os.Stderr, "hook: cwd=%s transcript=%s\n", hookData.Cwd, hookData.TranscriptPath)

	// Read last message from transcript
	lastMessage := "Session ended"
	if hookData.TranscriptPath != "" {
		if msg := getLastAssistantMessage(hookData.TranscriptPath); msg != "" {
			lastMessage = msg
		}
	}

	// In client mode, forward to server
	if forwardToServer(config, hookData.Cwd, lastMessage) {
		return nil
	}

	// Find session by matching cwd with saved path
	var sessionName string
	var topicID int64
	for name, info := range config.Sessions {
		if info == nil {
			continue
		}
		// Match against saved path or suffix
		if hookData.Cwd == info.Path || strings.HasSuffix(hookData.Cwd, "/"+name) {
			sessionName = name
			topicID = info.TopicID
			break
		}
	}
	if sessionName == "" || config.GroupID == 0 {
		fmt.Fprintf(os.Stderr, "hook: no session found for cwd=%s\n", hookData.Cwd)
		return nil
	}

	fmt.Fprintf(os.Stderr, "hook: session=%s topic=%d\n", sessionName, topicID)
	fmt.Fprintf(os.Stderr, "hook: sending message to telegram\n")
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
		if hookData.Cwd == info.Path || strings.HasSuffix(hookData.Cwd, "/"+name) {
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
		return ""
	}
	defer file.Close()

	var lastMessage string
	scanner := bufio.NewScanner(file)
	// Increase buffer size for large lines
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	for scanner.Scan() {
		var entry map[string]interface{}
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}
		if entry["type"] == "assistant" {
			if msg, ok := entry["message"].(map[string]interface{}); ok {
				if content, ok := msg["content"].([]interface{}); ok {
					for _, c := range content {
						if block, ok := c.(map[string]interface{}); ok {
							if block["type"] == "text" {
								if text, ok := block["text"].(string); ok {
									lastMessage = text
								}
							}
						}
					}
				}
			}
		}
	}
	return lastMessage
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
	if forwardToServer(config, hookData.Cwd, fmt.Sprintf("💬 %s", prompt)) {
		return nil
	}

	// Find session by matching cwd suffix
	var topicID int64
	for name, info := range config.Sessions {
		if info == nil {
			continue
		}
		if hookData.Cwd == info.Path || strings.HasSuffix(hookData.Cwd, "/"+name) {
			topicID = info.TopicID
			break
		}
	}

	if topicID == 0 || config.GroupID == 0 {
		fmt.Fprintf(os.Stderr, "hook-prompt: no topic found for cwd=%s\n", hookData.Cwd)
		return nil
	}

	// Send typing action
	sendTypingAction(config, config.GroupID, topicID)

	fmt.Fprintf(os.Stderr, "hook-prompt: sending to topic %d\n", topicID)
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
	if forwardToServer(config, hookData.Cwd, msg) {
		return nil
	}

	// Find session
	var sessionName string
	var topicID int64
	for name, info := range config.Sessions {
		if info == nil {
			continue
		}
		if hookData.Cwd == info.Path || strings.HasSuffix(hookData.Cwd, "/"+name) {
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
		if hookData.Cwd == info.Path || strings.HasSuffix(hookData.Cwd, "/"+name) {
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

func installHook() error {
	home, _ := os.UserHomeDir()
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	cccPath := filepath.Join(home, "bin", "ccc")

	data, err := os.ReadFile(settingsPath)
	if err != nil {
		return fmt.Errorf("failed to read settings.json: %w", err)
	}

	var settings map[string]interface{}
	if err := json.Unmarshal(data, &settings); err != nil {
		return fmt.Errorf("failed to parse settings.json: %w", err)
	}

	// Create the Stop hook
	stopHook := map[string]interface{}{
		"type":    "command",
		"command": cccPath + " hook",
	}

	hooks, ok := settings["hooks"].(map[string]interface{})
	if !ok {
		hooks = make(map[string]interface{})
	}
	hooks["Stop"] = []interface{}{stopHook}
	settings["hooks"] = hooks

	newData, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal settings: %w", err)
	}

	if err := os.WriteFile(settingsPath, newData, 0600); err != nil {
		return fmt.Errorf("failed to write settings.json: %w", err)
	}

	fmt.Println("✅ Claude hook installed!")
	return nil
}

// Bot commands

func setBotCommands(botToken string) {
	commands := `{
		"commands": [
			{"command": "ping", "description": "Check if bot is alive"},
			{"command": "away", "description": "Toggle away mode"},
			{"command": "new", "description": "Create/restart session: /new [host:]<name>"},
			{"command": "continue", "description": "Continue session: /continue [host:]<name>"},
			{"command": "kill", "description": "Kill session: /kill <name>"},
			{"command": "list", "description": "List active sessions"},
			{"command": "setdir", "description": "Set projects dir: /setdir [host:]~/path"},
			{"command": "c", "description": "Execute local command: /c <cmd>"},
			{"command": "rc", "description": "Execute remote command: /rc <host> <cmd>"},
			{"command": "host", "description": "Manage remote hosts: /host add|del|list"}
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
			if cwd == info.Path || strings.HasSuffix(cwd, "/"+name) {
				return sendMessage(config, config.GroupID, info.TopicID, message)
			}
		}
	}

	// Fallback to private chat
	return sendMessage(config, config.ChatID, 0, message)
}

// handleRemoteMessage handles messages forwarded from remote clients via --from flag
func handleRemoteMessage(fromHost string, message string) error {
	config, err := loadConfig()
	if err != nil {
		return fmt.Errorf("not configured: %v", err)
	}

	// Get current working directory (passed from remote via hook)
	cwd, _ := os.Getwd()

	// Find session matching fromHost and path
	for name, info := range config.Sessions {
		if info == nil {
			continue
		}
		// Check if this is a session from the specified host
		if info.Host != fromHost {
			continue
		}
		// Check if path matches
		if info.Path == cwd {
			fmt.Printf("[remote] from=%s session=%s\n", fromHost, name)
			return sendMessage(config, config.GroupID, info.TopicID, message)
		}
	}

	// No matching session found, send to private chat as fallback
	fmt.Printf("[remote] from=%s no matching session for path=%s\n", fromHost, cwd)
	return sendMessage(config, config.ChatID, 0, fmt.Sprintf("[%s] %s", fromHost, message))
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

	setBotCommands(config.BotToken)

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	offset := 0
	client := &http.Client{Timeout: 35 * time.Second}

	go func() {
		<-sigChan
		fmt.Println("\nShutting down...")
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

					tmuxName := "claude-" + sessionName
					if tmuxSessionExists(tmuxName) {
						// Send arrow down keys to select option, then Enter
						for i := 0; i < optionIndex; i++ {
							exec.Command(tmuxPath, "-S", tmuxSocket, "send-keys", "-t", tmuxName, "Down").Run()
							time.Sleep(50 * time.Millisecond)
						}
						exec.Command(tmuxPath, "-S", tmuxSocket, "send-keys", "-t", tmuxName, "Enter").Run()
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
					tmuxName := "claude-" + extractProjectName(projectName)

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
								// Send to appropriate tmux
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

					// Photos for remote sessions not yet supported
					if hostName != "" {
						sendMessage(config, chatID, threadID, "⚠️ Photos not yet supported for remote sessions. Send as text or use voice.")
						continue
					}

					// Extract project name for tmux session
					_, projectName := parseSessionTarget(sessionName)
					tmuxName := "claude-" + extractProjectName(projectName)

					if tmuxSessionExists(tmuxName) {
						// Get largest photo (last in array)
						photo := msg.Photo[len(msg.Photo)-1]
						imgPath := filepath.Join(os.TempDir(), fmt.Sprintf("telegram_%d.jpg", time.Now().UnixNano()))
						if err := downloadTelegramFile(config, photo.FileID, imgPath); err != nil {
							sendMessage(config, chatID, threadID, fmt.Sprintf("❌ Download failed: %v", err))
						} else {
							caption := msg.Caption
							if caption == "" {
								caption = "Analyze this image:"
							}
							prompt := fmt.Sprintf("%s %s", caption, imgPath)
							sendMessage(config, chatID, threadID, fmt.Sprintf("📷 Image saved, sending to Claude..."))
							// Send text first, wait for image to load, then send Enter
							sendToTmuxWithDelay(tmuxName, prompt, 2*time.Second)
						}
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

				// List configured sessions with status
				for name, info := range config.Sessions {
					if info == nil {
						continue
					}

					// Check if tmux session is running
					_, projectName := parseSessionTarget(name)
					tmuxName := "claude-" + extractProjectName(projectName)

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

					// Check if session already exists
					if _, exists := config.Sessions[fullName]; exists {
						sendMessage(config, chatID, threadID, fmt.Sprintf("⚠️ Session '%s' already exists. Use %s without args in that topic to restart.", fullName, cmdName))
						continue
					}

					// Create Telegram topic
					topicID, err := createForumTopic(config, fullName)
					if err != nil {
						sendMessage(config, chatID, threadID, fmt.Sprintf("❌ Failed to create topic: %v", err))
						continue
					}

					// Resolve work directory path
					workDir, err := resolveSessionPath(config, hostName, projectName)
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

					// Create work directory and tmux session
					tmuxName := "claude-" + extractProjectName(projectName)

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
					tmuxName := "claude-" + extractProjectName(projectName)

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
				if sessionName != "" {
					// Get session info to check if remote
					sessionInfo := config.Sessions[sessionName]
					hostName := ""
					if sessionInfo != nil {
						hostName = sessionInfo.Host
					}

					// Extract project name for tmux session (without host prefix)
					_, projectName := parseSessionTarget(sessionName)
					tmuxName := "claude-" + extractProjectName(projectName)

					if hostName != "" {
						// Remote session
						address := getHostAddress(config, hostName)
						if address == "" {
							sendMessage(config, chatID, threadID, fmt.Sprintf("❌ Host '%s' not configured", hostName))
							continue
						}

						if sshTmuxHasSession(address, tmuxName) {
							if err := sshTmuxSendKeys(address, tmuxName, text); err != nil {
								sendMessage(config, chatID, threadID, fmt.Sprintf("❌ Failed to send: %v", err))
							}
						} else {
							sendMessage(config, chatID, threadID, "⚠️ Session not running. Use /new or /continue to restart.")
						}
					} else {
						// Local session
						if tmuxSessionExists(tmuxName) {
							if err := sendToTmux(tmuxName, text); err != nil {
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

TELEGRAM COMMANDS:
    /ping                   Check if bot is alive
    /away                   Toggle away mode
    /new [host:]<name>      Create new session (on host or local)
    /new ~/path/name        Create session with custom path
    /new                    Restart session in current topic
    /continue [host:]<name> Create new session with -c flag
    /continue               Restart session with -c flag
    /kill <name>            Kill a session
    /list                   List sessions with status
    /setdir [host:]<path>   Set base directory for projects
    /c <cmd>                Execute local shell command
    /rc <host> <cmd>        Execute remote shell command
    /host add <name> <addr> Add remote host
    /host list              List configured hosts
    /host check <name>      Check host connectivity

CLIENT MODE:
    client                  Show client mode config
    client enable           Enable client mode (for laptops)
    client disable          Disable client mode
    client set server <host>  Set server address
    client set name <name>    Set this machine's name

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
		if err := startSession(false); err != nil {
			os.Exit(1)
		}
		return
	}

	// Check for -c flag (continue) as first arg
	if os.Args[1] == "-c" {
		if err := startSession(true); err != nil {
			os.Exit(1)
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
		// Check for --from flag (used by client mode to forward messages)
		var fromHost string
		args := os.Args[1:]
		filteredArgs := []string{}
		for i := 0; i < len(args); i++ {
			if strings.HasPrefix(args[i], "--from=") {
				fromHost = strings.TrimPrefix(args[i], "--from=")
			} else if args[i] == "--from" && i+1 < len(args) {
				fromHost = args[i+1]
				i++ // skip next arg
			} else {
				filteredArgs = append(filteredArgs, args[i])
			}
		}

		if fromHost != "" {
			// Message from remote client
			message := strings.Join(filteredArgs, " ")
			if err := handleRemoteMessage(fromHost, message); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		} else {
			// Regular message send
			if err := send(strings.Join(os.Args[1:], " ")); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		}
	}
}
