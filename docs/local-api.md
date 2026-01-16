# Local API

CCC provides a Unix socket API for local integration with external agents and orchestrators.

## Overview

The Local API allows external programs running on the same host to:
- List available Claude Code sessions
- Send messages to sessions (blocking or non-blocking)
- Retrieve message history
- Subscribe to real-time status updates

## Socket Location

```
~/.ccc.sock
```

The socket is created when `ccc listen` starts and removed on shutdown.
Permissions are set to owner-only (0600) for security.

## Protocol

- **Transport**: Unix socket
- **Format**: Newline-delimited JSON (one JSON object per line)
- **Encoding**: UTF-8

## Commands

### sessions

List all available sessions with their current status.

**Request:**
```json
{"cmd": "sessions"}
```

**Response:**
```json
{
  "ok": true,
  "sessions": [
    {"name": "myproject", "host": "local", "status": "active"},
    {"name": "backend", "host": "server1", "status": "idle"}
  ]
}
```

**Status values:**
- `active` - Claude is currently processing
- `idle` - Claude is waiting for input
- `stopped` - tmux session not running

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
  "duration_ms": 8500
}
```

**Parameters:**
- `session` (required) - Session name
- `text` (required) - Message to send
- `from` (optional) - Agent identifier, shown in Telegram as `[from]`

**Notes:**
- Timeout: 5 minutes
- Message appears in Telegram: `🤖 [orchestrator] What's the current API version?`
- Returns when Claude finishes responding

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
  "message_id": 12345
}
```

Use `history` command with `after` parameter to retrieve the response later.

---

### history

Retrieve message history for a session.

**Request:**
```json
{
  "cmd": "history",
  "session": "myproject",
  "after": 12345,
  "limit": 50
}
```

**Response:**
```json
{
  "ok": true,
  "messages": [
    {"id": 12346, "ts": 1705412345, "from": "claude", "text": "Tests passed..."},
    {"id": 12347, "ts": 1705412400, "from": "human", "text": "Great!", "username": "wlad"}
  ]
}
```

**Parameters:**
- `session` (required) - Session name
- `after` (optional) - Return messages after this ID
- `limit` (optional) - Maximum messages to return (default: 100)

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
# List sessions
echo '{"cmd":"sessions"}' | nc -U ~/.ccc.sock -q 1

# Send message and wait for response
echo '{"cmd":"ask","session":"myproject","text":"Hello"}' | nc -U ~/.ccc.sock -q 300

# Get recent history
echo '{"cmd":"history","session":"myproject","limit":10}' | nc -U ~/.ccc.sock -q 1
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

def ccc_request(cmd):
    sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    sock.connect(os.path.expanduser('~/.ccc.sock'))
    sock.sendall((json.dumps(cmd) + '\n').encode())
    response = sock.recv(65536).decode()
    sock.close()
    return json.loads(response)

# List sessions
sessions = ccc_request({"cmd": "sessions"})
print(sessions)

# Ask Claude
response = ccc_request({
    "cmd": "ask",
    "session": "myproject",
    "text": "What tests are failing?",
    "from": "my-script"
})
print(response["response"])
```

### Go Example

```go
package main

import (
    "encoding/json"
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

## Use Cases

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

### 2. Background Monitoring

Monitor all sessions for status changes:

```python
# Subscribe to all sessions
sock.sendall(b'{"cmd":"subscribe"}\n')
while True:
    event = json.loads(sock.recv(4096))
    if event["event"] == "status":
        print(f"{event['session']}: {event['status']}")
```

### 3. CI/CD Integration

Trigger Claude tasks from CI pipelines:

```bash
# In CI script
echo '{"cmd":"send","session":"myproject","text":"Run linter and fix issues","from":"github-actions"}' \
  | nc -U ~/.ccc.sock -q 1
```
