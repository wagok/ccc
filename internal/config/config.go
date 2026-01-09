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
}

// HostInfo stores information about a remote host
type HostInfo struct {
	Address     string `json:"address"`                // SSH target (user@host)
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
