package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/kidandcat/ccc/internal/config"
)

func TestDispatchWebhooks(t *testing.T) {
	var mu sync.Mutex
	var received []WebhookPayload
	var headers []http.Header

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p WebhookPayload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			t.Errorf("failed to decode payload: %v", err)
			w.WriteHeader(400)
			return
		}
		mu.Lock()
		received = append(received, p)
		headers = append(headers, r.Header.Clone())
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer srv.Close()

	msg := HistoryMessage{
		ID:        42,
		Timestamp: time.Date(2025, 1, 15, 10, 30, 0, 0, time.UTC).Unix(),
		From:      "human",
		Text:      "Hello webhook",
	}

	webhooks := []config.WebhookConfig{
		{URL: srv.URL, Token: "secret123", Events: []string{"message"}},
	}

	dispatchWebhooks(webhooks, "test-session", msg)

	mu.Lock()
	defer mu.Unlock()

	if len(received) != 1 {
		t.Fatalf("expected 1 webhook call, got %d", len(received))
	}

	p := received[0]
	if p.Event != "message" {
		t.Errorf("event = %q, want %q", p.Event, "message")
	}
	if p.Session != "test-session" {
		t.Errorf("session = %q, want %q", p.Session, "test-session")
	}
	if p.From != "human" {
		t.Errorf("from = %q, want %q", p.From, "human")
	}
	if p.MessageID != 42 {
		t.Errorf("messageId = %d, want %d", p.MessageID, 42)
	}
	if p.Preview != "Hello webhook" {
		t.Errorf("preview = %q, want %q", p.Preview, "Hello webhook")
	}

	auth := headers[0].Get("Authorization")
	if auth != "Bearer secret123" {
		t.Errorf("Authorization = %q, want %q", auth, "Bearer secret123")
	}
}

func TestDispatchWebhooksEventFilter(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(200)
	}))
	defer srv.Close()

	msg := HistoryMessage{ID: 1, Timestamp: time.Now().Unix(), From: "claude", Text: "test"}

	// Webhook only subscribes to "status" event, not "message"
	webhooks := []config.WebhookConfig{
		{URL: srv.URL, Events: []string{"status"}},
	}

	dispatchWebhooks(webhooks, "session", msg)

	if called {
		t.Error("webhook was called despite not subscribing to 'message' event")
	}
}

func TestDispatchWebhooksPreviewFallback(t *testing.T) {
	var mu sync.Mutex
	var received []WebhookPayload

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p WebhookPayload
		json.NewDecoder(r.Body).Decode(&p)
		mu.Lock()
		received = append(received, p)
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer srv.Close()

	webhooks := []config.WebhookConfig{
		{URL: srv.URL, Events: []string{"message"}},
	}

	tests := []struct {
		name    string
		msg     HistoryMessage
		preview string
	}{
		{
			name:    "text message",
			msg:     HistoryMessage{ID: 1, Timestamp: time.Now().Unix(), From: "human", Text: "hello"},
			preview: "hello",
		},
		{
			name:    "voice transcription",
			msg:     HistoryMessage{ID: 2, Timestamp: time.Now().Unix(), From: "human", Type: "voice", Transcription: "transcribed text"},
			preview: "transcribed text",
		},
		{
			name:    "photo with caption",
			msg:     HistoryMessage{ID: 3, Timestamp: time.Now().Unix(), From: "human", Type: "photo", Caption: "nice photo"},
			preview: "nice photo",
		},
		{
			name:    "photo no caption",
			msg:     HistoryMessage{ID: 4, Timestamp: time.Now().Unix(), From: "human", Type: "photo"},
			preview: "[photo]",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mu.Lock()
			received = received[:0]
			mu.Unlock()

			dispatchWebhooks(webhooks, "s", tt.msg)

			mu.Lock()
			defer mu.Unlock()
			if len(received) != 1 {
				t.Fatalf("expected 1 call, got %d", len(received))
			}
			if received[0].Preview != tt.preview {
				t.Errorf("preview = %q, want %q", received[0].Preview, tt.preview)
			}
		})
	}
}

func TestDispatchWebhooksPreviewTruncation(t *testing.T) {
	var mu sync.Mutex
	var received WebhookPayload

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		json.NewDecoder(r.Body).Decode(&received)
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer srv.Close()

	longText := ""
	for i := 0; i < 300; i++ {
		longText += "x"
	}

	webhooks := []config.WebhookConfig{
		{URL: srv.URL, Events: []string{"message"}},
	}
	msg := HistoryMessage{ID: 1, Timestamp: time.Now().Unix(), From: "human", Text: longText}

	dispatchWebhooks(webhooks, "s", msg)

	mu.Lock()
	defer mu.Unlock()
	if len(received.Preview) != 200 {
		t.Errorf("preview length = %d, want 200", len(received.Preview))
	}
}
