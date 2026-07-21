# harness9 Quick Start Guide

This guide walks you through installation, configuration, and running your first harness9 Agent session in 5 minutes.

---

## 1. Installation

### Option 1: One-line install script (recommended)

```bash
curl -fsSL https://raw.githubusercontent.com/ZhangShenao/harness9/master/scripts/install.sh | bash
```

Verify the installation:

```bash
harness9 --version
# harness9 v0.1.0
```

### Option 2: Build from source (developers)

Requires Go 1.25+:

```bash
git clone https://github.com/ZhangShenao/harness9
cd harness9
go build -o harness9 ./cmd/harness9
```

---

## 2. Configure the API Key

harness9 uses an OpenAI-compatible interface (Anthropic, OpenRouter, and others are also supported).

```bash
export OPENAI_API_KEY="sk-..."
```

It's recommended to add the command above to `~/.zshrc` or `~/.bashrc` so you don't need to set it every time.

### Optional: switch models

```bash
export LLM_MODEL="openai/gpt-4o"          # Default: openai/gpt-4o-mini
```

### Optional: use OpenRouter or another compatible API

```bash
export OPENAI_BASE_URL="https://openrouter.ai/api/v1"
export OPENAI_API_KEY="<your-openrouter-key>"
export LLM_MODEL="openai/gpt-4o"
```

### Using Anthropic

```bash
export ANTHROPIC_API_KEY="sk-ant-..."
export LLM_MODEL="claude-sonnet-4-6"
```

### Using a project-level `.env` file

Place a `.env` file in the project directory (**do not commit it to Git**):

```env
OPENAI_API_KEY=sk-...
LLM_MODEL=openai/gpt-4o-mini
```

> Priority: `export`-ed environment variables > `.env` file

---

## 3. First Run

```bash
cd /your/project   # Enter your project directory
harness9           # Start (automatically sets the current directory as the Agent's working sandbox)
```

In an interactive terminal, it automatically enters full-screen TUI mode:

```
         ╦ ╦  ╔╦╗  ╔═╗  ╔╗╦  ╔══  ╔══  ╔══  ╔═╗
         ╠═╣  ╠╩╣  ╠╦╝  ║╚╗  ╠═   ╚═╗  ╚═╗  ╚═╣
         ╩ ╩  ╚ ╝  ╩╗   ╩ ╩  ╚══  ══╝  ══╝    ╝

  harness9  ·  An AI-powered coding agent
  model: gpt-4o-mini  │  workdir: /your/project
  › Type a task and press Enter to send
```

Type a task and press Enter; the Agent will automatically read code, run commands, and analyze results:

```
  ▶ You: Help me analyze the bug in main.go

  ◆ harness9:
    Sure, let me read the file first...
    ✓ read_file(main.go) — 234ms
    Found a nil pointer dereference issue on line 42...
```

---

## 4. Basic Commands

| Command | Description |
|------|------|
| `/new` | Start a brand-new session (clears the current conversation history) |
| `/resume` | List historical sessions and choose one to resume |
| `/exit` | Exit the TUI |
| `Tab` | Autocomplete a command or Skill name |
| `↑ / ↓` | Scroll through conversation history |
| `Ctrl-C` | Interrupt the running Agent; press again to exit |

---

## 5. Configure Project Conventions (optional)

Place an `AGENTS.md` file in the project root; it is automatically injected into the System Prompt at startup:

```markdown
# My Project Conventions

## Tech Stack
- Go 1.25, PostgreSQL 16

## Coding Conventions
- Every function must have a comment
- Never access the database directly — always go through the Repository layer
```

---

## 6. Add Skills (optional)

Place domain knowledge that the Agent can load on demand under `skills/<name>/SKILL.md`:

```bash
mkdir -p skills/refactor-guide
```

```markdown
---
name: refactor-guide
description: Use when refactoring Go code — explains team conventions
---

# Refactoring Conventions
1. Run go vet first and fix all warnings
2. Keep functions under 50 lines
```

---

## 7. Context Management

Conversation history is automatically persisted to `~/.harness9/sessions.db` and can be restored via `/resume` after a process restart.

The status bar shows real-time token usage:

```
ctx: 45.2K/128K (35%)   ← Green: normal
ctx: 92.1K/128K (72%)   ← Yellow: warning
ctx: 108K/128K (84%)    ← Red: compaction about to trigger
```

When the context approaches the model limit, harness9 automatically calls the LLM to generate a conversation summary (SummarizationCompactor), retaining key information while continuing the session:

```
⚡ Context compacted — 12.5K → 6.2K tokens (45 → 22 messages)
```

---

## 8. Non-TTY / CI Mode

When invoked via a pipe or in CI, it automatically falls back to CLI REPL mode:

```bash
$ echo "List all Go files in the directory" | harness9
```

Or the interactive CLI:

```
harness9> Help me analyze the structure of internal/engine/agent_loop.go
harness9> exit
```

---

## FAQ

**Q: The API Key isn't working?**
Confirm that `export` was run in the current shell, or check whether the `.env` file is in the correct directory.

**Q: How do I use an Anthropic Claude model?**
Set `ANTHROPIC_API_KEY` and set `LLM_MODEL` to a Claude model name such as `claude-sonnet-4-6`.

**Q: Where is session data stored?**
`~/.harness9/sessions.db` (SQLite); deleting this file clears all historical sessions.

**Q: How do I fully clear the current session?**
Type `/new` in the TUI to create a new session; the old session data is still retained and can be restored via `/resume`.
