// Package telegram provides Telegram Bot API client functionality.
package telegram

import "encoding/json"

// Message represents a Telegram message
type Message struct {
	MessageID       int    `json:"message_id"`
	MessageThreadID int64  `json:"message_thread_id,omitempty"` // Topic ID
	Chat            Chat   `json:"chat"`
	From            User   `json:"from"`
	Text            string `json:"text"`
	ReplyToMessage  *Message `json:"reply_to_message,omitempty"`
	Voice           *Voice   `json:"voice,omitempty"`
	Photo           []Photo  `json:"photo,omitempty"`
	Caption         string   `json:"caption,omitempty"`
}

// Chat represents a Telegram chat
type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"` // "private", "group", "supergroup"
}

// User represents a Telegram user
type User struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

// Voice represents a voice message
type Voice struct {
	FileID   string `json:"file_id"`
	Duration int    `json:"duration"`
}

// Photo represents a photo
type Photo struct {
	FileID   string `json:"file_id"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	FileSize int    `json:"file_size"`
}

// CallbackQuery represents a callback query (button press)
type CallbackQuery struct {
	ID      string   `json:"id"`
	From    User     `json:"from"`
	Message *Message `json:"message"`
	Data    string   `json:"data"`
}

// Update represents an update from Telegram
type Update struct {
	OK          bool   `json:"ok"`
	Description string `json:"description"`
	Result      []UpdateResult `json:"result"`
}

// UpdateResult represents a single update result
type UpdateResult struct {
	UpdateID      int           `json:"update_id"`
	Message       Message       `json:"message"`
	CallbackQuery *CallbackQuery `json:"callback_query"`
}

// Response represents a response from Telegram API
type Response struct {
	OK          bool            `json:"ok"`
	Description string          `json:"description,omitempty"`
	Result      json.RawMessage `json:"result,omitempty"`
}

// TopicResult represents the result of creating a forum topic
type TopicResult struct {
	MessageThreadID int64  `json:"message_thread_id"`
	Name            string `json:"name"`
}

// InlineKeyboardButton represents an inline keyboard button
type InlineKeyboardButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data,omitempty"`
}

// InlineKeyboardMarkup represents an inline keyboard
type InlineKeyboardMarkup struct {
	InlineKeyboard [][]InlineKeyboardButton `json:"inline_keyboard"`
}
