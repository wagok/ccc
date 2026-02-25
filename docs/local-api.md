# Local API

CCC provides a Unix socket API for local integration with external agents and orchestrators.

## Overview

The Local API allows external programs running on the same host to:
- Check server health and version (`ping`)
- List available Claude Code sessions with metadata (`sessions`)
- Send messages to sessions — blocking (`ask`) or non-blocking (`send`)
- Restart crashed or stopped sessions (`continue`)
- Retrieve message history with filtering (`history`)
- Poll last activity across all sessions (`activity`)
- Capture raw tmux terminal output (`screenshot`)
- Handle interactive questions from Claude (`questions`, `answer`)
- Subscribe to real-time status updates (`subscribe`)

## Socket Location

```
~/.ccc.sock
```

The socket is created when `ccc listen` starts and removed on shutdown.
Permissions are set to owner-only (0600) for security.

## Running as a Service

The API server runs as part of `ccc listen`. For production use, run it as a systemd user service:

```bash
# Install service (done automatically during ccc setup)
ccc install-service

# Or manually create ~/.config/systemd/user/ccc.service
```

### Service Management

```bash
# Status
systemctl --user status ccc

# Logs
journalctl --user -u ccc -f

# Restart (after updating binary)
systemctl --user restart ccc

# Stop
systemctl --user stop ccc
```

### Enable Lingering

To keep the service running after logout:

```bash
sudo loginctl enable-linger $USER
```

**Note:** `make install` automatically restarts the service if it's running.

## Protocol

- **Transport**: Unix socket
- **Format**: Newline-delimited JSON (one JSON object per line)
- **Encoding**: UTF-8

## Commands

### ping

Health check. Returns server version, uptime, and active session count.

**Request:**
```json
{"cmd": "ping"}
```

**Response:**
```json
{
  "ok": true,
  "version": "1.0.0",
  "uptime_seconds": 3600,
  "sessions_active": 3
}
```

**Response fields:**
- `version` - Server version string
- `uptime_seconds` - Seconds since server started
- `sessions_active` - Number of configured (non-deleted) sessions

**Notes:**
- This command is instant (no tmux or SSH calls). Use it as a health/readiness probe.
- To check real-time session activity (active/idle), use `sessions` instead.

---

### sessions

List all available sessions with their current status and metadata.

**Request:**
```json
{"cmd": "sessions"}
```

**Response:**
```json
{
  "ok": true,
  "sessions": [
    {
      "name": "myproject",
      "host": "local",
      "status": "active",
      "cwd": "/home/user/Projects/myproject",
      "last_activity": 1705412400
    },
    {
      "name": "msi:backend",
      "host": "msi",
      "status": "idle",
      "cwd": "/home/user/Projects/backend",
      "last_activity": 1705410000
    }
  ]
}
```

**Session fields:**
- `name` - Session identifier (e.g. `"myproject"` or `"msi:backend"`)
- `host` - `"local"` or remote host name
- `status` - `"active"`, `"idle"`, or `"stopped"`
- `cwd` - Project working directory (the path Claude operates in)
- `last_activity` - Unix timestamp of last history file modification (0 if no history)

**Status values:**
- `active` - Claude is currently processing
- `idle` - Claude is waiting for input

**Notes:**
- `last_activity` is based on history file mtime, not tmux state. A session with no messages via CCC will have `last_activity: 0` even if Claude is active in tmux.
- `cwd` is the configured project path, not Claude's runtime working directory (Claude may `cd` elsewhere during a task).

---

### ask

Send a message and wait for Claude's response (blocking).

**Request:**
```json
{
  "cmd": "ask",
  "session": "myproject",
  "text": "What's the current API version?",
  "from": "orchestrator"
}
```

**Response:**
```json
{
  "ok": true,
  "response": "The current API version is 2.3.1...",
  "message_id": 287,
  "duration_ms": 8500
}
```

**Parameters:**
- `session` (required) - Session name
- `text` (required) - Message to send
- `from` (optional) - Agent identifier, shown in Telegram as `[from]`

**Response fields:**
- `response` - Claude's response text
- `message_id` - ID of the stored response message in history
- `duration_ms` - Wall-clock time from send to response

**Notes:**
- **Auto-start**: If session is not running, it will be automatically started with `-c` flag (continue)
- Timeout: 5 minutes
- Message appears in Telegram: `🤖 [orchestrator] What's the current API version?`
- Returns when Claude finishes responding
- Both the sent message and Claude's response are stored in history. The `message_id` in the response refers to Claude's reply, not the sent message.
- **One at a time**: Do not send concurrent `ask` requests to the same session. Check `sessions` status or use `ping` to verify the session is idle before sending.

---

### send

Send a message without waiting for response (non-blocking).

**Request:**
```json
{
  "cmd": "send",
  "session": "myproject",
  "text": "Run the full test suite",
  "from": "ci-bot"
}
```

**Response:**
```json
{
  "ok": true,
  "message_id": 285
}
```

**Notes:**
- **Auto-start**: If session is not running, it will be automatically started with `-c` flag (continue)
- The `message_id` refers to the sent message (not Claude's response)
- To retrieve Claude's response later, poll with `history` using `after` set to the returned `message_id`

---

### continue

Kill and restart a Claude Code session with conversation history preserved. Equivalent to the Telegram `/continue` command.

**Request:**
```json
{
  "cmd": "continue",
  "session": "msi:myproject",
  "from": "orchestrator"
}
```

**Response:**
```json
{"ok": true}
```

**Parameters:**
- `session` (required) - Session name (e.g. `"myproject"` or `"msi:myproject"`)
- `from` (optional) - Agent identifier, shown in Telegram notification

**Notes:**
- Kills the existing tmux session (if running), then creates a new one with `claude --dangerously-skip-permissions -c`
- The `-c` flag preserves Claude's conversation history
- Waits ~5 seconds for Claude to initialize, then verifies it's running
- Works for both local and remote (SSH) sessions
- Sends a notification to Telegram: `🔄 [orchestrator] Session continued`
- **Use case**: Recovering from crashed sessions. When `send` or `ask` returns errors like `"failed to restart Claude"`, call `continue` to force a clean restart.
- **Blocking**: This command takes ~6-8 seconds due to kill + create + initialization wait

---

### history

Retrieve message history for a session.

**Request:**
```json
{
  "cmd": "history",
  "session": "myproject",
  "after": 12345,
  "limit": 50,
  "from_filter": "claude"
}
```

**Response:**
```json
{
  "ok": true,
  "messages": [
    {"id": 12346, "ts": 1705412345, "from": "claude", "text": "Tests passed..."},
    {"id": 12347, "ts": 1705412400, "from": "claude", "text": "Refactoring complete."}
  ]
}
```

**Parameters:**
- `session` (required) - Session name
- `after` (optional) - Return messages after this ID
- `limit` (optional) - Maximum messages to return (default: 100)
- `from_filter` (optional) - Only return messages from this sender: `"human"`, `"claude"`, or `"api"`

**Message fields:**
- `id` - Unique message ID
- `ts` - Unix timestamp
- `from` - `human`, `claude`, or `api`
- `text` - Message content
- `type` - `voice` or `photo` for media messages
- `transcription` - For voice messages
- `caption` - For photo messages
- `agent` - For API messages, the `from` parameter
- `username` - Telegram username for human messages

**Notes:**
- `from_filter` is an exact match. Use `"api"` (not the agent name) to get all API-originated messages regardless of which agent sent them.
- To get only Claude's responses to your API messages, combine `after` with `from_filter`: send via `send`, save the `message_id`, then poll `history(after=message_id, from_filter="claude", limit=1)`.

---

### activity

Get last message summary for all sessions in a single call. Designed for external agent polling — compare `lastMessageId` with a saved index to detect new activity without calling `history` per session.

**Request:**
```json
{"cmd": "activity"}
```

**Response:**
```json
{
  "ok": true,
  "activity": [
    {
      "name": "ccc",
      "lastMessageId": 445,
      "lastMessageTs": 1772021789,
      "lastFrom": "claude",
      "lastText": "Done. All tests pass, deployed to..."
    },
    {
      "name": "msi:openClaw_dev",
      "lastMessageId": 312,
      "lastMessageTs": 1772020100,
      "lastFrom": "human",
      "lastText": "Update the CCC plugin"
    },
    {
      "name": "testproject",
      "lastMessageId": 0,
      "lastMessageTs": 0
    }
  ]
}
```

**Activity fields:**
- `name` - Session name
- `lastMessageId` - ID of the last message (0 if no history)
- `lastMessageTs` - Unix timestamp of the last message (0 if no history)
- `lastFrom` - Sender of the last message: `"human"`, `"claude"`, or `"api"`
- `lastText` - First 100 characters of the last message (truncated with `...`)

**Notes:**
- No parameters required — returns data for all configured (non-deleted) sessions
- Fast: reads only the last 8KB of each JSONL history file (tail seek), no full scan
- Sessions with no history return `lastMessageId: 0, lastMessageTs: 0`
- For voice messages, `lastText` contains the transcription; for photos, the caption

---

### screenshot

Capture raw tmux terminal output for a session. Returns the visible text content of the tmux pane.

**Request:**
```json
{
  "cmd": "screenshot",
  "session": "myproject",
  "limit": 100
}
```

**Response:**
```json
{
  "ok": true,
  "response": "❯ Running tests...\n\n  ✓ test_auth (0.3s)\n  ✓ test_api (0.5s)\n..."
}
```

**Parameters:**
- `session` (required) - Session name
- `limit` (optional) - Number of terminal lines to capture (default: 50)

**Notes:**
- Returns raw `tmux capture-pane` output — the actual terminal content including UI elements, spinners, and ANSI artifacts
- Works for both local and remote (SSH) sessions
- Useful for debugging session state, checking what Claude is currently doing, or verifying Claude's UI is responsive
- This is a point-in-time snapshot, not a continuous stream

---

### questions

Get pending interactive questions (AskUserQuestion) for a session. When Claude Code asks the user a question with multiple-choice options, the questions are stored and available via this endpoint.

**Request:**
```json
{
  "cmd": "questions",
  "session": "msi:myproject"
}
```

**Response (with pending questions):**
```json
{
  "ok": true,
  "questions": {
    "session": "msi:myproject",
    "questions": [
      {
        "question": "Which database should we use?",
        "header": "Database",
        "options": [
          {"label": "PostgreSQL", "description": "Relational, ACID compliant"},
          {"label": "MongoDB", "description": "Document store, flexible schema"},
          {"label": "SQLite", "description": "Embedded, zero configuration"}
        ],
        "multi_select": false,
        "answered": false
      }
    ],
    "timestamp": 1705412345
  }
}
```

**Response (no pending questions):**
```json
{"ok": true}
```

**Parameters:**
- `session` (required) - Session name

**Question fields:**
- `question` - The question text
- `header` - Short label/category (e.g. "Database", "Auth method")
- `options` - Array of choices, each with `label` and optional `description`
- `multi_select` - Whether multiple options can be selected
- `answered` - Whether this question has been answered already
- `answer_index` - Index of the selected option (if answered)
- `timestamp` - Unix timestamp when the question was received

**Notes:**
- Questions expire after 5 minutes and are automatically cleaned up
- Questions are also visible in Telegram as inline keyboard buttons
- If a question is answered via Telegram, it will be marked as `answered` here too
- Use `answer` command to respond to pending questions

---

### answer

Answer a pending interactive question by selecting an option. Sends the appropriate key sequences to Claude Code's UI to select the chosen option.

**Request:**
```json
{
  "cmd": "answer",
  "session": "msi:myproject",
  "question_index": 0,
  "option_index": 1
}
```

**Response:**
```json
{
  "ok": true,
  "response": "answered question 0 with option 1 (MongoDB)"
}
```

**Parameters:**
- `session` (required) - Session name
- `question_index` (required) - Which question to answer (0-based index)
- `option_index` (required) - Which option to select (0-based index)

**Notes:**
- Sends `Down` arrow keys (option_index times) + `Enter` to tmux to select the option in Claude's UI
- When all questions in a set are answered, automatically sends an extra `Enter` to submit
- The answer is stored in history as a human message: `"Selected: <option label>"`
- Returns error if no pending questions, question already answered, or indices out of range
- Works for both local and remote (SSH) sessions

---

### subscribe

Subscribe to real-time status updates (persistent connection).

**Request:**
```json
{
  "cmd": "subscribe",
  "sessions": ["myproject", "backend"]
}
```

**Events (streamed):**
```json
{"event": "subscribed", "session": "myproject,backend"}
{"event": "status", "session": "myproject", "status": "active"}
{"event": "status", "session": "myproject", "status": "idle"}
{"event": "status", "session": "backend", "status": "stopped"}
```

**Parameters:**
- `sessions` (optional) - List of sessions to monitor. If empty, monitors all sessions.

**Notes:**
- Connection stays open until client disconnects
- Status events sent when session state changes
- Polling interval: 5 seconds

## Error Handling

All commands return `ok: false` on error:

```json
{
  "ok": false,
  "error": "session not found"
}
```

Common errors:
- `invalid JSON` - Malformed request
- `unknown command` - Invalid `cmd` value
- `session required` - Missing required parameter
- `session not found` - Session doesn't exist or is deleted
- `host not configured` - Remote host not in config
- `failed to send: ...` - tmux communication error
- `timeout waiting for response` - Ask command timeout (5 min)
- `no pending questions for this session` - No AskUserQuestion pending
- `question already answered` - Question was already answered (via API or Telegram)
- `question_index out of range` - Invalid question index
- `option_index out of range` - Invalid option index
- `session started but Claude failed to initialize` - Continue started tmux but Claude didn't start
- `failed to start: ...` - Continue failed to create tmux session

## Examples

### Using nc (netcat)

```bash
# Health check
echo '{"cmd":"ping"}' | nc -U ~/.ccc.sock -q 1

# List sessions with metadata
echo '{"cmd":"sessions"}' | nc -U ~/.ccc.sock -q 1

# Send message and wait for response
echo '{"cmd":"ask","session":"myproject","text":"Hello"}' | nc -U ~/.ccc.sock -q 300

# Send message without waiting
echo '{"cmd":"send","session":"myproject","text":"Run tests","from":"ci"}' | nc -U ~/.ccc.sock -q 1

# Restart a crashed session
echo '{"cmd":"continue","session":"msi:myproject","from":"agent"}' | nc -U ~/.ccc.sock -q 10

# Get recent history
echo '{"cmd":"history","session":"myproject","limit":10}' | nc -U ~/.ccc.sock -q 1

# Get only Claude's messages
echo '{"cmd":"history","session":"myproject","from_filter":"claude","limit":5}' | nc -U ~/.ccc.sock -q 1

# Poll activity across all sessions
echo '{"cmd":"activity"}' | nc -U ~/.ccc.sock -q 1

# Capture terminal output (100 lines)
echo '{"cmd":"screenshot","session":"myproject","limit":100}' | nc -U ~/.ccc.sock -q 1

# Check for pending questions
echo '{"cmd":"questions","session":"msi:myproject"}' | nc -U ~/.ccc.sock -q 1

# Answer a question (select option 2)
echo '{"cmd":"answer","session":"msi:myproject","question_index":0,"option_index":2}' | nc -U ~/.ccc.sock -q 1
```

### Using socat

```bash
# Interactive session
socat - UNIX-CONNECT:$HOME/.ccc.sock

# With timeout
echo '{"cmd":"sessions"}' | socat -t 5 - UNIX-CONNECT:$HOME/.ccc.sock
```

### Python Example

```python
import socket
import json
import os

def ccc_request(cmd):
    sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    sock.connect(os.path.expanduser('~/.ccc.sock'))
    sock.sendall((json.dumps(cmd) + '\n').encode())
    response = sock.recv(65536).decode()
    sock.close()
    return json.loads(response)

# Health check
ping = ccc_request({"cmd": "ping"})
print(f"CCC v{ping['version']}, up {ping['uptime_seconds']}s")

# List sessions
sessions = ccc_request({"cmd": "sessions"})
for s in sessions["sessions"]:
    print(f"  {s['name']} ({s['status']}) cwd={s.get('cwd', 'N/A')}")

# Ask Claude
response = ccc_request({
    "cmd": "ask",
    "session": "myproject",
    "text": "What tests are failing?",
    "from": "my-script"
})
print(f"Response (msg {response['message_id']}): {response['response']}")

# Restart a crashed session
result = ccc_request({
    "cmd": "continue",
    "session": "msi:myproject",
    "from": "my-script"
})
if result["ok"]:
    print("Session restarted successfully")

# Handle interactive questions
qs = ccc_request({"cmd": "questions", "session": "msi:myproject"})
if qs.get("questions"):
    for i, q in enumerate(qs["questions"]["questions"]):
        if not q["answered"]:
            print(f"Q{i}: {q['question']}")
            for j, opt in enumerate(q["options"]):
                print(f"  [{j}] {opt['label']} - {opt.get('description', '')}")
            # Answer with first option
            ccc_request({
                "cmd": "answer",
                "session": "msi:myproject",
                "question_index": i,
                "option_index": 0
            })
```

### Go Example

```go
package main

import (
    "encoding/json"
    "fmt"
    "net"
    "os"
    "path/filepath"
)

func main() {
    home, _ := os.UserHomeDir()
    conn, _ := net.Dial("unix", filepath.Join(home, ".ccc.sock"))
    defer conn.Close()

    // Send request
    req := map[string]string{"cmd": "sessions"}
    json.NewEncoder(conn).Encode(req)

    // Read response
    var resp map[string]interface{}
    json.NewDecoder(conn).Decode(&resp)

    fmt.Printf("%+v\n", resp)
}
```

## Message History Storage

Messages are stored in JSONL files:

```
~/.ccc/history/<topic_id>/messages/2026-01-16-14.jsonl
```

- Files are organized by Telegram topic ID
- New file created each hour
- Format: one JSON message per line
- Stored indefinitely (no automatic rotation)

## Telegram Integration

All messages sent via the API appear in the corresponding Telegram topic:

```
🤖 [orchestrator] What's the current API version?
```

This allows human oversight of automated interactions. Session restarts via `continue` also send a notification:

```
🔄 [orchestrator] Session continued
```

## Patterns and Use Cases

### 1. Orchestrator Agent

An orchestrator can route requests to appropriate Claude sessions:

```
User → Orchestrator: "Check if tests pass in backend"
Orchestrator → CCC API: ask(session="backend", text="Run tests")
CCC API → Claude: Runs tests
Claude → CCC API: "All 47 tests pass"
CCC API → Orchestrator: response
Orchestrator → User: "Backend tests all pass (47/47)"
```

### 2. Fire-and-Poll (Non-blocking)

For long-running tasks, use `send` + `history` instead of `ask` to avoid blocking:

```python
# 1. Send the task
result = ccc_request({
    "cmd": "send",
    "session": "backend",
    "text": "Run the full test suite and fix failures",
    "from": "orchestrator"
})
sent_id = result["message_id"]

# 2. Poll for Claude's response
import time
while True:
    time.sleep(10)
    history = ccc_request({
        "cmd": "history",
        "session": "backend",
        "after": sent_id,
        "from_filter": "claude",
        "limit": 1
    })
    if history["messages"]:
        print("Claude responded:", history["messages"][0]["text"])
        break
```

This pattern is preferable when:
- The task may take longer than the 5-minute `ask` timeout
- You need to monitor multiple sessions concurrently
- You want to check on progress without blocking the caller

### 3. Pre-flight Check

Before sending work, verify the server is healthy and the target session is idle:

```python
# Check server
ping = ccc_request({"cmd": "ping"})
if not ping["ok"]:
    raise RuntimeError("CCC is down")

# Check target session is idle
sessions = ccc_request({"cmd": "sessions"})
target = next((s for s in sessions["sessions"] if s["name"] == "backend"), None)
if target is None:
    raise RuntimeError("Session not found")
if target["status"] == "active":
    raise RuntimeError("Session is busy, retry later")

# Safe to send
ccc_request({"cmd": "ask", "session": "backend", "text": "...", "from": "agent"})
```

### 4. Background Monitoring

Monitor all sessions for status changes:

```python
# Subscribe to all sessions
sock.sendall(b'{"cmd":"subscribe"}\n')
while True:
    event = json.loads(sock.recv(4096))
    if event["event"] == "status":
        print(f"{event['session']}: {event['status']}")
```

### 5. CI/CD Integration

Trigger Claude tasks from CI pipelines:

```bash
# In CI script
echo '{"cmd":"send","session":"myproject","text":"Run linter and fix issues","from":"github-actions"}' \
  | nc -U ~/.ccc.sock -q 1
```

### 6. Session Discovery by Path

Find the session for a specific project using `cwd`:

```python
sessions = ccc_request({"cmd": "sessions"})
target = next(
    (s["name"] for s in sessions["sessions"]
     if s.get("cwd", "").endswith("/myproject")),
    None
)
if target:
    ccc_request({"cmd": "ask", "session": target, "text": "...", "from": "agent"})
```

### 7. Crash Recovery

Automatically recover crashed sessions:

```python
def send_with_recovery(session, text, from_agent="agent"):
    """Send a message, restarting the session if needed."""
    result = ccc_request({
        "cmd": "send",
        "session": session,
        "text": text,
        "from": from_agent
    })

    if not result["ok"] and "failed to restart" in result.get("error", ""):
        # Session is crashed beyond auto-recovery, force restart
        restart = ccc_request({
            "cmd": "continue",
            "session": session,
            "from": from_agent
        })
        if not restart["ok"]:
            raise RuntimeError(f"Cannot recover session: {restart['error']}")

        # Retry the send
        result = ccc_request({
            "cmd": "send",
            "session": session,
            "text": text,
            "from": from_agent
        })

    return result
```

### 8. Handling Interactive Questions

Programmatically answer Claude's questions (e.g. tool permissions, clarifications):

```python
import time

def poll_and_answer_questions(session, decision_fn, timeout=300):
    """Poll for questions and answer them using a decision function.

    decision_fn(question, options) -> option_index
    """
    deadline = time.time() + timeout
    while time.time() < deadline:
        qs = ccc_request({"cmd": "questions", "session": session})
        if not qs.get("questions"):
            time.sleep(2)
            continue

        for i, q in enumerate(qs["questions"]["questions"]):
            if q["answered"]:
                continue
            option_idx = decision_fn(q["question"], q["options"])
            ccc_request({
                "cmd": "answer",
                "session": session,
                "question_index": i,
                "option_index": option_idx
            })
        return True

    return False  # Timed out

# Example: always pick the first option
poll_and_answer_questions("msi:myproject", lambda q, opts: 0)
```
