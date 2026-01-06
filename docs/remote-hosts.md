# Remote Hosts - Detailed Architecture

This document describes the complete architecture and mechanics of remote host support in ccc.

## Overview

Remote hosts allow you to run Claude Code sessions on different machines (laptops, workstations, servers) while controlling everything from Telegram. There are two ways to start a session:

1. **Server-initiated** - You send `/new laptop:project` from Telegram
2. **Client-initiated** - You run `ccc` directly on the client machine

Both methods result in the same outcome: a tmux session with Claude on the client, connected to a Telegram topic on the server.

## Architecture

```
                          ┌─────────────────────────────────────────┐
                          │              SERVER                      │
                          │         (ccc listen runs here)           │
                          │                                          │
┌──────────┐              │  ┌──────────────┐    ┌──────────────┐   │
│ Telegram │◀────────────▶│  │ ccc listen   │    │ ~/.ccc.json  │   │
│  (phone) │              │  │              │    │              │   │
└──────────┘              │  │ - polls TG   │    │ - sessions   │   │
                          │  │ - routes msgs│    │ - hosts      │   │
                          │  │ - manages    │    │ - topics     │   │
                          │  │   topics     │    │              │   │
                          │  └──────┬───────┘    └──────────────┘   │
                          │         │                                │
                          └─────────┼────────────────────────────────┘
                                    │ SSH (bidirectional)
                    ┌───────────────┼───────────────┐
                    │               │               │
                    ▼               ▼               ▼
           ┌──────────────┐ ┌──────────────┐ ┌──────────────┐
           │   LAPTOP     │ │ WORKSTATION  │ │   OTHER PC   │
           │              │ │              │ │              │
           │ ┌──────────┐ │ │ ┌──────────┐ │ │ ┌──────────┐ │
           │ │   tmux   │ │ │ │   tmux   │ │ │ │   tmux   │ │
           │ │ session  │ │ │ │ session  │ │ │ │ session  │ │
           │ │          │ │ │ │          │ │ │ │          │ │
           │ │ ┌──────┐ │ │ │ │ ┌──────┐ │ │ │ │ ┌──────┐ │ │
           │ │ │claude│ │ │ │ │ │claude│ │ │ │ │ │claude│ │ │
           │ │ └──┬───┘ │ │ │ │ └──┬───┘ │ │ │ │ └──┬───┘ │ │
           │ └────┼─────┘ │ │ └────┼─────┘ │ │ └────┼─────┘ │
           │      │       │ │      │       │ │      │       │
           │      ▼       │ │      ▼       │ │      ▼       │
           │  Stop hook   │ │  Stop hook   │ │  Stop hook   │
           │  ccc hook    │ │  ccc hook    │ │  ccc hook    │
           │      │       │ │      │       │ │      │       │
           └──────┼───────┘ └──────┼───────┘ └──────┼───────┘
                  │                │                │
                  └────────────────┴────────────────┘
                                   │
                                   │ SSH: ccc --from=hostname --cwd=/path "message"
                                   ▼
                             ┌───────────┐
                             │  SERVER   │
                             │ ccc       │
                             │ receives  │
                             │ forwards  │
                             │ to TG     │
                             └───────────┘
```

## Configuration

### Server Configuration (`~/.ccc.json` on server)

```json
{
  "bot_token": "...",
  "chat_id": 123456789,
  "group_id": -1001234567890,
  "sessions": {
    "laptop:myproject": {
      "topic_id": 123,
      "path": "/home/user/Projects/myproject",
      "host": "laptop",
      "deleted": false
    }
  },
  "hosts": {
    "laptop": {
      "address": "user@192.168.1.100",
      "projects_dir": "~/Projects"
    }
  }
}
```

### Client Configuration (`~/.ccc.json` on client)

```json
{
  "mode": "client",
  "server": "user@server-ip",
  "host_name": "laptop"
}
```

## Session Naming Convention

Sessions are named using the format `hostname:projectname`:

| Session Name | Host | Project | Path on Client |
|--------------|------|---------|----------------|
| `laptop:myproject` | laptop | myproject | ~/Projects/myproject |
| `laptop:webapp` | laptop | webapp | ~/Projects/webapp |
| `workstation:api` | workstation | api | ~/Dev/api |

The project directory is determined by the host's `projects_dir` setting.

## Server-Initiated Sessions

When you send `/new laptop:myproject` from Telegram:

### Step 1: Parse and Validate

```
/new laptop:myproject
      │        │
      │        └─ project name
      └─ host name (must exist in hosts config)
```

### Step 2: Create Telegram Topic

```go
topicID := createForumTopic(config, "laptop:myproject")
```

### Step 3: SSH to Client

```bash
ssh user@laptop "cd ~/Projects/myproject && tmux new-session -d -s claude-myproject"
ssh user@laptop "tmux send-keys -t claude-myproject 'ccc run' Enter"
```

### Step 4: Save Session

```go
config.Sessions["laptop:myproject"] = &SessionInfo{
    TopicID: topicID,
    Path:    "/home/user/Projects/myproject",  // full path on client
    Host:    "laptop",
}
```

### Step 5: Claude Starts

Inside the tmux session, `ccc run` executes Claude with the Stop hook configured.

## Client-Initiated Sessions

When you run `ccc` directly on a client machine:

### Step 1: Detect Client Mode

```go
if config.Mode == "client" && config.Server != "" && config.HostName != "" {
    startClientSession(config, args)
}
```

### Step 2: Register on Server

```bash
# Client executes via SSH:
ssh user@server "ccc register-session laptop /home/user/Projects/myproject"

# Server returns topic ID
```

### Step 3: Server Creates/Finds Topic

```go
func getOrCreateTopic(config, "laptop:myproject", path, "laptop") {
    // Check if session exists
    if info, exists := config.Sessions[fullName]; exists {
        // Verify topic still exists via editForumTopic
        // If deleted, create new topic
        // Return existing topic ID
    }
    // Create new topic
    return createForumTopic(config, fullName)
}
```

### Step 4: Client Creates tmux

```go
createTmuxSession("claude-myproject", projectPath, continueSession)
// Starts: ccc run (which runs claude)
```

### Step 5: Client Attaches

```go
exec.Command(tmuxPath, "attach-session", "-t", "claude-myproject")
```

## Message Flow

### Telegram → Claude (User sends message)

```
1. User sends "Fix the bug" in topic "laptop:myproject"

2. Server (ccc listen) receives message:
   - Identifies topic → session "laptop:myproject"
   - Looks up host "laptop" → address "user@192.168.1.100"

3. Server sends via SSH:
   ssh user@192.168.1.100 "tmux send-keys -t claude-myproject 'Fix the bug' Enter"

4. Claude receives and processes
```

### Claude → Telegram (Claude responds)

```
1. Claude finishes response, Stop hook fires

2. Hook runs: ccc hook
   - Reads hook data (cwd, transcript_path)
   - Extracts last assistant message from transcript

3. Client detects client mode:
   forwardToServer(config, cwd, message)

4. Client sends via SSH:
   ssh user@server "ccc --from=laptop --cwd=/home/user/Projects/myproject 'Claude response...'"

5. Server (handleRemoteMessage):
   - Finds session by host+path
   - Sends to corresponding Telegram topic
```

## Topic Management

### Topic Lifecycle

```
┌─────────────┐     ┌─────────────┐     ┌─────────────┐
│   CREATE    │────▶│   ACTIVE    │────▶│   DELETED   │
│             │     │             │     │ (soft)      │
└─────────────┘     └─────────────┘     └─────────────┘
      │                   │                   │
      │                   │                   │
      ▼                   ▼                   ▼
 createForumTopic    editForumTopic      Deleted=true
                     (verify exists)     (in config)
```

### Soft Delete

When you run `/kill laptop:myproject`:

```go
func killSession(config, "laptop:myproject") {
    // Kill tmux session (local or remote)
    // Mark as deleted (NOT removed from config)
    sessionInfo.Deleted = true
    saveConfig(config)
}
```

- **Telegram topic**: Preserved (not deleted)
- **Config entry**: Preserved with `deleted: true`
- **tmux session**: Killed
- **Project folder**: Preserved

### Why Soft Delete?

1. **Prevents duplicates**: When session is recreated, reuses existing topic
2. **Preserves history**: Telegram messages remain accessible
3. **Allows recovery**: `/new laptop:myproject` restores the session

### Topic Verification

When creating/finding a topic, `getOrCreateTopic` verifies it exists:

```go
err := editForumTopic(config, topicID, fullName)
if err != nil {
    // Check error type
    if isTopicDeleted(err) {
        // Topic was manually deleted in Telegram
        // Create new topic
        topicID = createForumTopic(config, fullName)
    }
    // If error is "not modified", topic exists - continue
}
```

## Fallback Topic Creation

If a hook message arrives from an unknown session (e.g., user started Claude manually on client), the server automatically creates a topic:

```go
func handleRemoteMessage(fromHost, cwd, message) {
    // Try to find existing session
    for name, info := range config.Sessions {
        if info.Host == fromHost && info.Path == cwd {
            // Found - send to existing topic
            return sendMessage(config, groupID, info.TopicID, message)
        }
    }

    // Not found - create topic automatically
    fullName := fromHost + ":" + filepath.Base(cwd)
    topicID := getOrCreateTopic(config, fullName, cwd, fromHost)
    return sendMessage(config, groupID, topicID, message)
}
```

## The `/movehere` Command

Fixes duplicate topics by moving a session to the current topic:

```
Scenario:
- Topic A: "laptop:myproject" (original)
- Topic B: "temp" (manually created)

In Topic B, send: /movehere laptop:myproject

Result:
1. Topic B renamed to "laptop:myproject"
2. Session now points to Topic B
3. Topic A deleted
```

```go
func handleMovehere(name, currentTopicID) {
    info := config.Sessions[name]
    oldTopicID := info.TopicID

    // Rename current topic
    editForumTopic(config, currentTopicID, name)

    // Update session
    info.TopicID = currentTopicID

    // Delete old topic
    deleteForumTopic(config, oldTopicID)
}
```

## SSH Command Details

### Commands Sent to Client

| Action | SSH Command |
|--------|-------------|
| Check connection | `ssh host "echo ok"` |
| Check command | `ssh host "bash -i -l -c 'which claude'"` |
| Create directory | `ssh host "mkdir -p ~/Projects/name"` |
| Create tmux | `ssh host "tmux new-session -d -s claude-name -c ~/Projects/name"` |
| Send to tmux | `ssh host "tmux send-keys -t claude-name 'text' Enter"` |
| Kill tmux | `ssh host "tmux kill-session -t claude-name"` |
| Check tmux | `ssh host "tmux has-session -t claude-name"` |

### Commands Sent to Server (from client hook)

```bash
ssh user@server "ccc --from=laptop --cwd=/home/user/Projects/myproject 'Claude message'"
```

Parameters:
- `--from=laptop` - Client hostname (must match config)
- `--cwd=/path` - Full project path on client
- Message is base64-encoded for safety

## Requirements Summary

### Server Requirements

- `ccc listen` running as service
- SSH access to all clients (passwordless)
- Telegram bot configured

### Client Requirements

- `ccc` installed with client mode enabled
- SSH access back to server (passwordless)
- `tmux` installed
- `claude` installed and authenticated
- Stop hook configured in `~/.claude/settings.json`

### Bidirectional SSH

```
Server ──SSH──▶ Client   (for sending commands)
Server ◀──SSH── Client   (for hook responses)
```

Both directions must work without password prompts.

## Troubleshooting

### Check Client Mode

```bash
ccc client
# Should show:
#   mode: client
#   server: user@server
#   host_name: laptop
```

### Test Hook Manually

```bash
cd ~/Projects/myproject
echo '{"cwd":"'$(pwd)'","session_id":"test"}' | ccc hook
# Should show: hook: forwarding to server...
```

### Check Server Logs

```bash
journalctl --user -u ccc -f
# Watch for: [remote] from=laptop ...
```

### Verify Topic Exists

```bash
# On server
cat ~/.ccc.json | jq '.sessions["laptop:myproject"]'
```

### Test SSH Both Ways

```bash
# Server → Client
ssh user@client "echo ok"

# Client → Server
ssh user@server "ccc --version"
```
