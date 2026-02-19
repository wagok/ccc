# Local API

CCC provides a Unix socket API for local integration with external agents and orchestrators.

## Overview

The Local API allows external programs running on the same host to:
- Check server health and version (`ping`)
- List available Claude Code sessions with metadata
- Send messages to sessions (blocking or non-blocking)
- Retrieve message history with filtering
- Subscribe to real-time status updates

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
      "name": "backend",
      "host": "server1",
      "status": "idle",
      "cwd": "/home/user/Projects/backend",
      "last_activity": 1705410000
    }
  ]
}
```

**Session fields:**
- `name` - Session identifier
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

## Examples

### Using nc (netcat)

```bash
# Health check
echo '{"cmd":"ping"}' | nc -U ~/.ccc.sock -q 1

# List sessions with metadata
echo '{"cmd":"sessions"}' | nc -U ~/.ccc.sock -q 1

# Send message and wait for response
echo '{"cmd":"ask","session":"myproject","text":"Hello"}' | nc -U ~/.ccc.sock -q 300

# Get recent history
echo '{"cmd":"history","session":"myproject","limit":10}' | nc -U ~/.ccc.sock -q 1

# Get only Claude's messages
echo '{"cmd":"history","session":"myproject","from_filter":"claude","limit":5}' | nc -U ~/.ccc.sock -q 1
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

This allows human oversight of automated interactions.

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
