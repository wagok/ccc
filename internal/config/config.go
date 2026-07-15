// Package config handles ccc configuration loading, saving, and migration.
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// SessionInfo stores information about a session
type SessionInfo struct {
	TopicID int64  `json:"topic_id"`
	Path    string `json:"path"`
	Host    string `json:"host,omitempty"`    // Remote host name or "" for local
	Deleted bool   `json:"deleted,omitempty"` // Soft-deleted (killed but topic preserved)
	Group   string `json:"group,omitempty"`   // Project-group alias; "" = the default group

	// Account overrides the group's subscription account for THIS session (an
	// alias in Config.Accounts). Empty = inherit the group's account
	// (GroupInfo.Account). Lets a few agents in a group run on a separate
	// Anthropic account while staying in the group's mail domain / secretary.
	Account string `json:"account,omitempty"`

	// IntegrationMode selects the Claude-Code integration behavior for this
	// session: "legacy" (Stop-only, transcript-JSONL capture — robust, change-
	// resistant) or "live" (richer hook-driven streaming/buttons/typing).
	// "" means inherit the global default (see SessionIntegrationMode).
	IntegrationMode string `json:"integration_mode,omitempty"`

	// StreamOff disables live response streaming for this session even when it
	// is in "live" mode (typing/buttons stay on). Default (false) = streaming on
	// in live mode. Toggled on the fly with /stream on|off.
	StreamOff bool `json:"stream_off,omitempty"`
}

// GroupInfo stores a project-group (a separate Telegram group/channel). Groups
// are added manually to the config (alias -> chat_id).
type GroupInfo struct {
	ChatID  int64  `json:"chat_id"`
	Name    string `json:"name,omitempty"`    // optional human-friendly label
	Account string `json:"account,omitempty"` // subscription-account alias (see Config.Accounts); empty = default
}

// HostInfo stores information about a remote host
type HostInfo struct {
	Address     string `json:"address"`                // SSH target (user@host)
	ProjectsDir string `json:"projects_dir,omitempty"` // Base directory for projects on this host
}

// WebhookConfig stores configuration for an outgoing webhook
type WebhookConfig struct {
	URL    string   `json:"url"`
	Token  string   `json:"token,omitempty"`
	Events []string `json:"events"`
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

	// Outgoing webhooks
	Webhooks []WebhookConfig `json:"webhooks,omitempty"`

	// Project groups: alias -> group. Each project belongs to exactly one group
	// (a separate Telegram group/channel). Added manually. The implicit
	// "default" group is the original GroupID (see GroupChatID).
	Groups map[string]*GroupInfo `json:"groups,omitempty"`

	// Subscription accounts: alias -> Claude config directory (CLAUDE_CONFIG_DIR).
	// A group whose GroupInfo.Account matches an alias here runs its agents under
	// that config dir (separate login/usage limit). Empty/unset = the default
	// ~/.claude. Lets different Telegram groups bill to different subscriptions.
	Accounts map[string]string `json:"accounts,omitempty"`

	// DefaultIntegrationMode is the fleet-wide default Claude-Code integration
	// mode for sessions that don't override it. "" -> IntegrationLegacy.
	DefaultIntegrationMode string `json:"default_integration_mode,omitempty"`
}

// Claude-Code integration modes. legacy = current robust behavior (Stop hook +
// transcript JSONL). live = richer hook-driven UX (streaming, buttons, typing).
const (
	IntegrationLegacy = "legacy"
	IntegrationLive   = "live"
)

// ValidIntegrationMode reports whether s is a known integration mode.
func ValidIntegrationMode(s string) bool {
	return s == IntegrationLegacy || s == IntegrationLive
}

// SessionIntegrationMode returns the effective integration mode for a session:
// its own override, else the global default, else legacy.
func SessionIntegrationMode(config *Config, info *SessionInfo) string {
	if info != nil && info.IntegrationMode != "" {
		return info.IntegrationMode
	}
	if config != nil && config.DefaultIntegrationMode != "" {
		return config.DefaultIntegrationMode
	}
	return IntegrationLegacy
}

// DefaultGroup is the alias of the implicit group backed by the original
// config.GroupID — where every pre-existing project lives.
const DefaultGroup = "default"

// SessionGroup returns the group alias a session belongs to ("" -> default).
func SessionGroup(info *SessionInfo) string {
	if info == nil || info.Group == "" {
		return DefaultGroup
	}
	return info.Group
}

// GroupChatID resolves a group alias to its Telegram chat id. The default group
// falls back to the original config.GroupID for backward compatibility. Returns
// 0 for an unknown (unconfigured) group.
func GroupChatID(config *Config, group string) int64 {
	if group == "" {
		group = DefaultGroup
	}
	if config.Groups != nil {
		if g, ok := config.Groups[group]; ok && g != nil && g.ChatID != 0 {
			return g.ChatID
		}
	}
	if group == DefaultGroup {
		return config.GroupID
	}
	return 0
}

// SessionGroupChatID returns the Telegram chat id of the group owning a session.
func SessionGroupChatID(config *Config, sessionName string) int64 {
	return GroupChatID(config, SessionGroup(config.Sessions[sessionName]))
}

// GroupExists reports whether a group alias is configured (default always is, as
// long as a GroupID is set).
func GroupExists(config *Config, group string) bool {
	if group == "" || group == DefaultGroup {
		return GroupChatID(config, DefaultGroup) != 0
	}
	return GroupChatID(config, group) != 0
}

// GetSessionByGroupTopic finds the session in a specific group's chat by topic.
// chatID is the Telegram group chat id; topicID is the message_thread_id.
// Matching on the (group, topic) pair disambiguates topic ids that can collide
// across different groups.
func GetSessionByGroupTopic(config *Config, chatID int64, topicID int64) string {
	if config.Sessions == nil {
		return ""
	}
	for name, info := range config.Sessions {
		if info != nil && info.TopicID == topicID && GroupChatID(config, SessionGroup(info)) == chatID {
			return name
		}
	}
	return ""
}

// Path returns the config file path (~/.ccc.json)
func Path() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".ccc.json")
}

// LoadOrCreate loads config or returns empty config if file doesn't exist
func LoadOrCreate() (*Config, error) {
	config, err := Load()
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

// Load loads config from disk
func Load() (*Config, error) {
	data, err := os.ReadFile(Path())
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
		Save(&config)
	} else {
		// Parse with new format
		if err := json.Unmarshal(data, &config); err != nil {
			return nil, err
		}
	}

	if config.Sessions == nil {
		config.Sessions = make(map[string]*SessionInfo)
	}
	if config.Hosts == nil {
		config.Hosts = make(map[string]*HostInfo)
	}

	return &config, nil
}

// Save saves config to disk with proper permissions (0600)
func Save(config *Config) error {
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(Path(), data, 0600)
}

// GetProjectsDir returns the base directory for projects
func GetProjectsDir(config *Config) string {
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

// ResolveProjectPath resolves the full path for a project
// If name starts with / or ~/, it's treated as absolute/home-relative path
// Otherwise, it's relative to projects_dir
func ResolveProjectPath(config *Config, name string) string {
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
	return filepath.Join(GetProjectsDir(config), name)
}

// ExpandPath expands ~ to home directory
func ExpandPath(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, path[2:])
	}
	return path
}

// GetHostAddress returns SSH address for a host, or empty if local/not found
func GetHostAddress(config *Config, hostName string) string {
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

// GetHostProjectsDir returns projects dir for a host
func GetHostProjectsDir(config *Config, hostName string) string {
	if hostName == "" {
		return GetProjectsDir(config)
	}
	if config.Hosts != nil {
		if host, ok := config.Hosts[hostName]; ok && host.ProjectsDir != "" {
			return host.ProjectsDir
		}
	}
	return "~"
}

// GetSessionByTopic finds session name by topic ID
func GetSessionByTopic(config *Config, topicID int64) string {
	if config.Sessions == nil {
		return ""
	}
	for name, info := range config.Sessions {
		if info.TopicID == topicID {
			return name
		}
	}
	return ""
}
