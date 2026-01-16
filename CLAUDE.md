# Project Instructions

## Language Policy

**All code, comments, commit messages, and documentation in this repository MUST be in English.**

This includes:
- Source code comments
- Variable/function names
- Commit messages
- Documentation files (README, CLAUDE.md, etc.)
- Issue descriptions in beads

Note: Conversation with the user may be in any language they prefer.

## Task Tracking Policy

### Use beads (bd) exclusively

For ANY task tracking, progress monitoring, or TODO management, use ONLY `bd`:

```bash
bd ready              # Find available work
bd create "Task"      # Create a task
bd update <id> --status in_progress  # Start work
bd close <id>         # Complete a task
bd sync               # Sync with git
```

### Prohibited for task tracking

| Tool | Status | Reason |
|------|--------|--------|
| **TodoWrite** | PROHIBITED | Use `bd` instead |
| **Linear MCP** | PROHIBITED | External system, not integrated |
| **Asana MCP** | PROHIBITED | External system, not integrated |
| **GitHub Issues** | Upstream only | Not for internal tasks |
| **GitLab Issues** | PROHIBITED | Not used |
| **TODO.md files** | PROHIBITED | Tasks only in beads |

### Replacing TodoWrite with beads

When planning multiple tasks, instead of TodoWrite:

```bash
# Create tasks in beads
bd create "Implement feature X" -p 1
bd create "Add tests for X" -p 2
bd create "Update documentation" -p 3

# Track progress via bd
bd update <id> --status in_progress
bd close <id>
```

### Auxiliary tools (read-only)

- **qdrant-mcp** — documentation search only
- **claude-context** — code structure analysis only
- **Grep/Glob** — code search

These tools are NOT used for storing tasks or status.
