package telegram

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Client provides Telegram Bot API functionality
type Client struct {
	BotToken string
}

// NewClient creates a new Telegram client
func NewClient(botToken string) *Client {
	return &Client{BotToken: botToken}
}

// API calls a Telegram Bot API method
func (c *Client) API(method string, params url.Values) (*Response, error) {
	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/%s", c.BotToken, method)
	resp, err := http.PostForm(apiURL, params)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var result Response
	json.Unmarshal(body, &result)
	return &result, nil
}

// SendMessage sends a text message to a chat
func (c *Client) SendMessage(chatID int64, threadID int64, text string) error {
	const maxLen = 4000

	// Split long messages
	messages := SplitMessage(text, maxLen)

	for _, msg := range messages {
		params := url.Values{
			"chat_id": {fmt.Sprintf("%d", chatID)},
			"text":    {msg},
		}
		if threadID > 0 {
			params.Set("message_thread_id", fmt.Sprintf("%d", threadID))
		}

		result, err := c.API("sendMessage", params)
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

// SendMessageWithKeyboard sends a message with inline keyboard
func (c *Client) SendMessageWithKeyboard(chatID int64, threadID int64, text string, buttons [][]InlineKeyboardButton) error {
	keyboard := InlineKeyboardMarkup{InlineKeyboard: buttons}
	keyboardJSON, _ := json.Marshal(keyboard)

	params := url.Values{
		"chat_id":      {fmt.Sprintf("%d", chatID)},
		"text":         {text},
		"reply_markup": {string(keyboardJSON)},
	}
	if threadID > 0 {
		params.Set("message_thread_id", fmt.Sprintf("%d", threadID))
	}

	result, err := c.API("sendMessage", params)
	if err != nil {
		return err
	}
	if !result.OK {
		return fmt.Errorf("telegram error: %s", result.Description)
	}
	return nil
}

// AnswerCallbackQuery answers a callback query
func (c *Client) AnswerCallbackQuery(callbackID string) {
	params := url.Values{
		"callback_query_id": {callbackID},
	}
	c.API("answerCallbackQuery", params)
}

// EditMessageRemoveKeyboard edits a message to remove its keyboard
func (c *Client) EditMessageRemoveKeyboard(chatID int64, messageID int, newText string) {
	params := url.Values{
		"chat_id":    {fmt.Sprintf("%d", chatID)},
		"message_id": {fmt.Sprintf("%d", messageID)},
		"text":       {newText},
	}
	c.API("editMessageText", params)
}

// SendTypingAction sends a typing action indicator
func (c *Client) SendTypingAction(chatID int64, threadID int64) {
	params := url.Values{
		"chat_id": {fmt.Sprintf("%d", chatID)},
		"action":  {"typing"},
	}
	if threadID > 0 {
		params.Set("message_thread_id", fmt.Sprintf("%d", threadID))
	}
	c.API("sendChatAction", params)
}

// CreateForumTopic creates a new forum topic
func (c *Client) CreateForumTopic(groupID int64, name string) (int64, error) {
	if groupID == 0 {
		return 0, fmt.Errorf("no group configured")
	}

	params := url.Values{
		"chat_id": {fmt.Sprintf("%d", groupID)},
		"name":    {name},
	}

	result, err := c.API("createForumTopic", params)
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

// EditForumTopic renames a topic and verifies it exists
func (c *Client) EditForumTopic(groupID int64, topicID int64, name string) error {
	if groupID == 0 {
		return fmt.Errorf("no group configured")
	}

	params := url.Values{
		"chat_id":           {fmt.Sprintf("%d", groupID)},
		"message_thread_id": {fmt.Sprintf("%d", topicID)},
		"name":              {name},
	}

	result, err := c.API("editForumTopic", params)
	if err != nil {
		return err
	}
	if !result.OK {
		return fmt.Errorf("failed to edit topic: %s", result.Description)
	}

	return nil
}

// DeleteForumTopic deletes a topic
func (c *Client) DeleteForumTopic(groupID int64, topicID int64) error {
	if groupID == 0 {
		return fmt.Errorf("no group configured")
	}

	params := url.Values{
		"chat_id":           {fmt.Sprintf("%d", groupID)},
		"message_thread_id": {fmt.Sprintf("%d", topicID)},
	}

	result, err := c.API("deleteForumTopic", params)
	if err != nil {
		return err
	}
	if !result.OK {
		return fmt.Errorf("failed to delete topic: %s", result.Description)
	}

	return nil
}

// DownloadFile downloads a file from Telegram
func (c *Client) DownloadFile(fileID string, destPath string) error {
	// Get file path from Telegram
	resp, err := http.Get(fmt.Sprintf("https://api.telegram.org/bot%s/getFile?file_id=%s", c.BotToken, fileID))
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
	if !result.OK || result.Result.FilePath == "" {
		return fmt.Errorf("failed to get file path")
	}

	// Download file
	fileURL := fmt.Sprintf("https://api.telegram.org/file/bot%s/%s", c.BotToken, result.Result.FilePath)
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

// SetBotCommands sets the bot's command list
func (c *Client) SetBotCommands(commands []BotCommand) error {
	commandsJSON, _ := json.Marshal(commands)
	params := url.Values{
		"commands": {string(commandsJSON)},
	}
	_, err := c.API("setMyCommands", params)
	return err
}

// BotCommand represents a bot command
type BotCommand struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

// SplitMessage splits a long message into chunks
func SplitMessage(text string, maxLen int) []string {
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
