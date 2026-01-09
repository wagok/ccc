// Package hook handles Claude Code hook events and transcript parsing.
package hook

import (
	"bufio"
	"encoding/json"
	"os"
)

// Data represents data received from Claude hook
type Data struct {
	Cwd            string `json:"cwd"`
	TranscriptPath string `json:"transcript_path"`
	SessionID      string `json:"session_id"`
	HookEventName  string `json:"hook_event_name"`
	ToolName       string `json:"tool_name"`
	Prompt         string `json:"prompt"` // For UserPromptSubmit hook
	ToolInput      ToolInput `json:"tool_input"`
}

// ToolInput represents tool input data from hooks
type ToolInput struct {
	Questions []Question `json:"questions"`
}

// Question represents a question from AskUserQuestion tool
type Question struct {
	Question    string   `json:"question"`
	Header      string   `json:"header"`
	MultiSelect bool     `json:"multiSelect"`
	Options     []Option `json:"options"`
}

// Option represents an option in a question
type Option struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

// TranscriptEntry represents an entry in the transcript file
type TranscriptEntry struct {
	Type    string `json:"type"`
	Message struct {
		Content []ContentBlock `json:"content"`
	} `json:"message"`
}

// ContentBlock represents a content block in a message
type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// GetLastAssistantMessage reads the transcript and returns the last assistant message
func GetLastAssistantMessage(transcriptPath string) string {
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
		var entry TranscriptEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}
		if entry.Type == "assistant" {
			// Extract text content
			for _, block := range entry.Message.Content {
				if block.Type == "text" && block.Text != "" {
					lastMessage = block.Text
				}
			}
		}
	}
	return lastMessage
}
