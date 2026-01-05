# ccc - Claude Code Companion

> Your companion for [Claude Code](https://claude.ai/claude-code) - control sessions remotely via Telegram. Start sessions from your phone, interact with Claude, and receive notifications when tasks complete.

[![Go Version](https://img.shields.io/badge/Go-1.21+-00ADD8?style=flat&logo=go)](https://go.dev)
[![License](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

## Why ccc?

Ever wanted to:
- Start a Claude Code session from your phone while away from your computer?
- Continue a session seamlessly between your phone and PC?
- Get notified when Claude finishes a long-running task?

**ccc** bridges Claude Code with Telegram, letting you control sessions from anywhere.

## Features

- **100% Self-Hosted** - Runs entirely on your machine, no third-party servers
- **Privacy First** - Your code and conversations never leave your computer (except to Telegram for messages you send)
- **Remote Control** - Start and manage Claude Code sessions from Telegram
- **Multi-Session** - Run multiple concurrent sessions, each with its own Telegram topic
- **Seamless Handoff** - Start on phone, continue on PC (or vice versa)
- **Notifications** - Get Claude's responses in Telegram when away
- **Voice Messages** - Send voice messages, automatically transcribed with Whisper
- **Image Support** - Send images to Claude for analysis
- **tmux Integration** - Sessions persist and can be attached from any terminal
- **One-shot Queries** - Quick Claude questions via private chat

## Demo Workflow

```
📱 Phone (Telegram)              💻 PC (Terminal)
─────────────────────────────────────────────────────
1. /new myproject
   → Creates ~/Projects/myproject
   → Creates Telegram topic
   → Starts Claude session

2. "Fix the auth bug"
   → Claude starts working

3. Claude responds in topic
   ✅ myproject
   Fixed the auth bug by...

                                 4. cd ~/Projects/myproject && ccc
                                    → Attaches to same session

                                 5. Continue working with Claude
```

## Requirements

- macOS, Linux, or Windows (WSL)
- Go 1.21+
- [tmux](https://github.com/tmux/tmux)
- [Claude Code](https://claude.ai/claude-code) installed
- Telegram account

### Optional Dependencies

- **Voice transcription** - For voice message support (choose one):
  - Local Whisper: `pip install openai-whisper`
  - OpenAI API: Set `OPENAI_API_KEY`
  - Groq API: Set `GROQ_API_KEY` (fastest)

  See [Transcription Setup](#transcription-setup) for configuration.

> **Windows users**: Use [WSL2](https://learn.microsoft.com/en-us/windows/wsl/install) with Ubuntu. Claude Code and ccc both work best on Linux. Install WSL, then follow the Linux instructions.

## Installation

### From Source

```bash
git clone https://github.com/kidandcat/ccc.git
cd ccc
make install
```

This builds, signs (on macOS), and installs to `~/bin/`.

### Verify Installation

```bash
ccc --version
# ccc version 1.0.0
```

> **macOS troubleshooting**: If you get `killed` when running ccc, the binary needs to be signed:
> ```bash
> codesign -s - ~/bin/ccc
> ```

## Quick Start

### 1. Create a Telegram Bot

1. Open Telegram and message [@BotFather](https://t.me/botfather)
2. Send `/newbot` and follow the prompts
3. Save the bot token (looks like `123456789:ABCdefGHIjklMNOpqrsTUVwxyz`)

### 2. Run Setup

```bash
ccc setup YOUR_BOT_TOKEN
```

This single command does everything:
- Connects to Telegram (send any message to your bot when prompted)
- Optionally configures a group with topics for multiple sessions
- Installs the Claude hook for notifications
- Installs and starts the background service

### 3. Start Using

```bash
cd ~/myproject
ccc
```

That's it! You're ready to control Claude Code from Telegram.

> **Optional**: For session topics, create a Telegram group with Topics enabled, add your bot as admin, and run `ccc setgroup`

## Usage

### Terminal Commands

| Command | Description |
|---------|-------------|
| `ccc` | Start/attach Claude session in current directory |
| `ccc -c` | Continue previous session |
| `ccc "message"` | Send notification (if away mode on) |
| `ccc doctor` | Check all dependencies and configuration |
| `ccc config` | Show current configuration |
| `ccc config projects-dir <path>` | Set base directory for new projects |
| `ccc --help` | Show help |
| `ccc --version` | Show version |

### Telegram Commands

**In your group:**

| Command | Description |
|---------|-------------|
| `/new <name>` | Create new session + topic (in projects directory) |
| `/new ~/path/name` | Create session in custom location |
| `/new` | Restart session in current topic (kills if running) |
| `/continue <name>` | Create new session with conversation history |
| `/continue` | Restart with `-c` flag (continues conversation) |
| `/kill <name>` | Kill a session |
| `/list` | List active sessions |
| `/setdir <path>` | Set base directory for new projects |
| `/ping` | Check if bot is alive |
| `/away` | Toggle away mode (notifications) |
| `/c <cmd>` | Run shell command on your machine |

**In private chat:**
- Send any message to run a one-shot Claude query

### Voice Messages & Images

**Voice Messages**:
- Send a voice message in a session topic
- Bot transcribes and sends text to Claude
- Supports multiple transcription backends (see [Transcription Setup](#transcription-setup))

**Image Attachments**:
- Send an image in a session topic (with optional caption)
- Image is saved and path is sent to Claude for analysis

### Example Session

```bash
# On your PC - start working on a project
cd ~/myproject
ccc
# Claude session starts in tmux

# Later, from phone - check on progress
# Telegram: Send message in the myproject topic
# Claude responds in the topic

# Back on PC - continue where you left off
cd ~/myproject
ccc
# Attaches to existing session
```

## Service Setup

> **Note**: `ccc setup` automatically installs and starts the service. The info below is only needed for manual setup or troubleshooting.

For the bot to run continuously, set it up as a system service.

<details>
<summary><strong>macOS (launchd)</strong></summary>

Create `~/Library/LaunchAgents/com.ccc.plist`:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.ccc</string>
    <key>ProgramArguments</key>
    <array>
        <string>/usr/local/bin/ccc</string>
        <string>listen</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>/tmp/ccc.log</string>
    <key>StandardErrorPath</key>
    <string>/tmp/ccc.log</string>
</dict>
</plist>
```

Load the service:

```bash
launchctl load ~/Library/LaunchAgents/com.ccc.plist
```

</details>

<details>
<summary><strong>Linux (systemd)</strong></summary>

Create `~/.config/systemd/user/ccc.service`:

```ini
[Unit]
Description=Claude Code Controller
After=network.target

[Service]
ExecStart=/usr/local/bin/ccc listen
Restart=always
RestartSec=10

[Install]
WantedBy=default.target
```

Enable and start:

```bash
systemctl --user enable ccc
systemctl --user start ccc
```

</details>

## Configuration

Config is stored in `~/.ccc.json`:

```json
{
  "bot_token": "your-telegram-bot-token",
  "chat_id": 123456789,
  "group_id": -1001234567890,
  "sessions": {
    "myproject": {
      "topic_id": 42,
      "path": "/home/user/Projects/myproject"
    },
    "experiment": {
      "topic_id": 43,
      "path": "/home/user/experiments/test"
    }
  },
  "projects_dir": "/home/user/Projects",
  "transcription_cmd": "~/bin/transcribe-groq",
  "away": false
}
```

| Field | Description |
|-------|-------------|
| `bot_token` | Your Telegram bot token |
| `chat_id` | Your Telegram user ID (for authorization) |
| `group_id` | Telegram group ID for session topics |
| `sessions` | Map of session names to topic ID and project path |
| `projects_dir` | Base directory for new projects (default: `~`) |
| `transcription_cmd` | Command for voice transcription (optional) |
| `away` | When true, notifications are sent |

> **Note**: Session paths are stored at creation time. Changing `projects_dir` only affects new sessions.

### Projects Directory

By default, `/new myproject` creates `~/myproject`. To organize projects in a dedicated folder:

```bash
# Via CLI
ccc config projects-dir ~/Projects

# Via Telegram
/setdir ~/Projects
```

Now `/new myproject` creates `~/Projects/myproject`.

**Override for specific projects:**
```
/new myproject              → ~/Projects/myproject
/new ~/experiments/test     → ~/experiments/test
/new /tmp/quicktest         → /tmp/quicktest
```

### Transcription Setup

Voice messages require a transcription backend. Configure via `transcription_cmd` in `~/.ccc.json`:

```json
{
  "transcription_cmd": "~/bin/transcribe-groq"
}
```

The command receives the audio file path as an argument and should output the transcription to stdout.

**Available backends** (see `examples/` directory):

| Script | Backend | Speed |
|--------|---------|-------|
| `transcribe-whisper` | Local Whisper | Slow (runs locally) |
| `transcribe-openai` | OpenAI API | Medium |
| `transcribe-groq` | Groq API | Fast (recommended) |

**Setup (Groq example):**

```bash
# 1. Copy script
cp examples/transcribe-groq ~/bin/
chmod +x ~/bin/transcribe-groq

# 2. Edit script and add your API key
nano ~/bin/transcribe-groq
# Set: GROQ_API_KEY="gsk_your_key_here"

# 3. Add to ~/.ccc.json
"transcription_cmd": "~/bin/transcribe-groq"
```

Get Groq API key: https://console.groq.com/keys (free tier available)

**Fallback:** If `transcription_cmd` is not set, ccc tries to use local `whisper` command.

### Session Lifecycle

When you create a session with `/new myproject`:

1. **Telegram topic** is created in your group
2. **Project folder** is created (if it doesn't exist)
3. **tmux session** starts with Claude Code
4. **Config** stores the session name, topic ID, and full path

**Using existing folders:** If the folder already exists, ccc uses it as-is without modifying contents. This lets you create sessions for existing projects.

### Deleting and Recovering Sessions

The `/kill` command performs a **soft delete**:

| What | Deleted? |
|------|----------|
| tmux session | Yes |
| Config entry | Yes |
| Project folder | **No** (preserved) |
| Telegram topic | **No** (preserved) |

**Scenarios:**

```
# Scenario 1: Temporarily stop a session
/kill myproject
# Later: /new myproject → new topic, same folder

# Scenario 2: Clean up topic manually
/kill myproject
# In Telegram: Archive or delete the topic via UI
# Folder with code remains on disk

# Scenario 3: Fresh start with existing code
/kill myproject
# Delete topic in Telegram UI
/new myproject
# → New topic, new chat history, existing code preserved

# Scenario 4: Reuse folder with different session name
/kill oldname
/new ~/Projects/oldname  # explicit path
# → Creates session with name "oldname" pointing to same folder
```

> **Tip**: Telegram topics can be archived (hidden) or deleted via UI. Deleting a topic removes all message history permanently.

## Remote Sessions

Run Claude Code sessions on remote machines (laptops, workstations) while controlling everything from your phone via Telegram. The server manages all sessions and routes messages to the appropriate machine via SSH.

### Architecture

```
┌─────────────┐     ┌─────────────┐     ┌─────────────┐
│  Telegram   │────▶│   Server    │────▶│   Laptop    │
│   (phone)   │◀────│ ccc listen  │◀────│ claude-code │
└─────────────┘     └─────────────┘     └─────────────┘
                          │ SSH              │
                          ▼                  │
                    ┌─────────────┐          │
                    │ Workstation │◀─────────┘
                    │ claude-code │   hooks via SSH
                    └─────────────┘
```

### Quick Start

**On the server** (where `ccc listen` runs):

```bash
# Add a remote host
ccc host add laptop user@192.168.1.100

# Set projects directory for the host
/setdir laptop:~/Projects

# Create a session on the laptop
/new laptop:myproject
```

**On the laptop** (optional client mode):

```bash
# Configure client mode to forward hooks to server
ccc client set server user@server
ccc client set name laptop
ccc client enable
```

### Server Setup

#### 1. Configure SSH Access

Ensure passwordless SSH from server to each remote host:

```bash
# Generate key if needed
ssh-keygen -t ed25519

# Copy to remote host
ssh-copy-id user@laptop

# Test connection
ssh user@laptop "echo ok"
```

#### 2. Add Remote Hosts

```bash
# Via Telegram
/host add laptop user@192.168.1.100
/host add workstation user@192.168.1.101 ~/Code

# Check connectivity
/host check laptop
```

#### 3. Create Remote Sessions

```bash
# Create session on laptop
/new laptop:myproject

# Create with custom path
/new laptop:~/experiments/test

# Continue existing conversation
/continue laptop:myproject
```

### Telegram Commands for Remote

| Command | Description |
|---------|-------------|
| `/new laptop:name` | Create session on remote host |
| `/continue laptop:name` | Continue session with history |
| `/kill laptop:name` | Kill remote session |
| `/list` | List all sessions with status (🟢/⚪) |
| `/setdir laptop:~/path` | Set projects directory for host |
| `/rc laptop <cmd>` | Execute command on remote host |
| `/host add <name> <addr>` | Add remote host |
| `/host list` | List configured hosts |
| `/host check <name>` | Check host connectivity |
| `/host del <name>` | Remove host |

### Client Mode (Optional)

Client mode allows laptops to forward Claude's responses back to Telegram through the server. This is useful when you want real-time updates from sessions running on laptops.

**On the laptop:**

```bash
# Configure client
ccc client set server user@homeserver
ccc client set name laptop
ccc client enable

# Verify configuration
ccc client
```

When client mode is enabled:
- Claude hooks forward messages to the server via SSH
- Server sends them to the appropriate Telegram topic
- You get real-time responses even when session runs on laptop

**Disable client mode:**

```bash
ccc client disable
```

### Requirements for Remote Hosts

Each remote machine needs:

1. **SSH access** from server (passwordless)
2. **tmux** installed
3. **Claude Code** installed and authenticated
4. **ccc** binary (only if using client mode)

Verify with:
```bash
/host check laptop
# ✅ laptop (user@192.168.1.100)
#   SSH: ok
#   tmux: ok
#   claude: ok
```

### Example Workflow

```
📱 Phone (Telegram)              🖥️ Server              💻 Laptop
─────────────────────────────────────────────────────────────────
1. /host add laptop user@laptop
   → Adds host config
   → Checks SSH/tmux/claude

2. /new laptop:webapp
   → SSH: creates ~/Projects/webapp
   → SSH: starts tmux + claude
   → Creates Telegram topic

3. "Add login page"
   → SSH: sends to tmux
                                                    Claude works...
                                 ← SSH hook ←       Claude responds
   ← Telegram ←

4. /list
   🟢 laptop:webapp
   ⚪ localproject (stopped)

5. /rc laptop git status
   → SSH: runs command
   ← Shows output
```

### Troubleshooting Remote Sessions

**SSH connection fails:**
```bash
# Test manually
ssh -o BatchMode=yes -o ConnectTimeout=5 user@host "echo ok"

# Check SSH key
ssh-add -l
```

**Session starts but dies:**
```bash
# Check claude on remote
/rc laptop "which claude"
/rc laptop "claude --version"

# Check tmux
/rc laptop "tmux list-sessions"
```

**Messages not reaching session:**
```bash
# Verify session is running
/list

# Check tmux directly
/rc laptop "tmux has-session -t claude-myproject && echo running"
```

**Client mode not forwarding:**
```bash
# On laptop, verify config
ccc client

# Test SSH back to server
ssh user@server "ccc --version"
```

## How It Works

```
┌─────────────┐     ┌─────────────┐     ┌─────────────┐
│  Telegram   │────▶│     ccc     │────▶│    tmux     │
│   (phone)   │◀────│   listen    │◀────│   session   │
└─────────────┘     └─────────────┘     └─────────────┘
                           │                   │
                           │                   ▼
                           │            ┌─────────────┐
                           └───────────▶│ Claude Code │
                              hook      └─────────────┘
```

1. `ccc listen` runs as a service, polling Telegram for messages
2. Messages in topics are forwarded to the corresponding tmux session
3. Claude Code runs inside tmux with a hook that sends responses back
4. You can attach to any session from terminal with `ccc`

## Privacy & Security

### Privacy

**ccc runs 100% on your machine.** There are no external servers, no analytics, no data collection.

- Your code stays on your computer
- Claude Code runs locally via Anthropic's official CLI
- Only messages you explicitly send go through Telegram
- No telemetry, no tracking, no cloud dependencies

The only external communication is:
1. **Telegram API** - For sending/receiving your messages (your bot, your control)
2. **Anthropic API** - Claude Code's own connection (handled by Claude Code itself)

### Security

- **Authorization**: Bot only accepts messages from the configured `chat_id`
- **Config permissions**: `~/.ccc.json` is created with `0600` (owner-only)
- **Open source**: Full code transparency, audit it yourself

> ⚠️ Note: Uses `--dangerously-skip-permissions` for automation - understand the implications

## Troubleshooting

**First, run diagnostics:**
```bash
ccc doctor
```
This checks tmux, claude, config, hooks, and service status.

**Bot not responding?**
- Check if `ccc listen` is running: `systemctl --user status ccc`
- Verify bot token in `~/.ccc.json`
- Check logs: `journalctl --user -u ccc -f`

**Session not starting?**
- Ensure tmux is installed: `which tmux`
- Check if Claude Code is installed: `which claude`
- On Linux, verify tmux socket exists: `ls /tmp/tmux-$(id -u)/`

**Messages not reaching Claude?**
- Verify you're in the correct topic
- Check if session exists: `/list`
- Try restarting: `/new`

**Session dies immediately?**
- Check `ccc doctor` output
- Verify Claude can start: `claude --version`
- Check tmux session: `tmux list-sessions`

## Contributing

Contributions welcome! Please:

1. Fork the repository
2. Create a feature branch
3. Run tests: `go test ./...`
4. Submit a PR

## License

[MIT License](LICENSE) - feel free to use in your projects!

---

Made with Claude Code 🤖
