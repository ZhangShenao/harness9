# harness9 CLI User Guide

harness9 runs by default as an **interactive terminal Agent (CLI REPL)**, letting you converse with the Agent directly in the terminal.

---

## Installation

### One-line install (recommended)

```bash
curl -fsSL https://raw.githubusercontent.com/ZhangShenao/harness9/master/scripts/install.sh | bash
```

Verify after installation:

```bash
harness9 --version
# harness9 v0.1.0
```

---

## Upgrading

Users who already have harness9 installed can upgrade to the latest version directly via the built-in `upgrade` command:

```bash
harness9 upgrade
```

Upgrade process:

1. Query the GitHub Releases API to get the latest version number
2. Compare against the current version; exit if already up to date
3. Download the archive for the current platform (OS / architecture)
4. Verify SHA256 (to ensure integrity)
5. Extract and atomically replace the current executable

Example output:

```
Checking for the latest version...
Found new version: v0.1.1 (current: 0.1.0)
Downloading harness9_0.1.1_darwin_arm64.tar.gz...
Verifying SHA256...
Extracting...
Upgrade successful: harness9 v0.1.1
```

> **Note**: If harness9 is installed in a directory that requires root privileges (e.g. `/usr/local/bin`), the upgrade may report a permission error — use `sudo harness9 upgrade` in that case.

### Building from source (developers)

```bash
git clone https://github.com/ZhangShenao/harness9
cd harness9
go run ./cmd/harness9
```

---

## Quick Start

```bash
# Set the API Key (recommended to add to ~/.zshrc or ~/.bashrc)
export OPENAI_API_KEY="sk-..."

# Enter your project directory (harness9 uses this directory as the Agent's sandbox)
cd /your/project

# Launch
harness9
```

Once you see the prompt, the Agent is ready:

```
harness9 │ Type "exit" or press Ctrl-C to quit

harness9>
```

---

## Environment Variables

| Variable | Required | Default | Description |
|------|:----:|--------|------|
| `OPENAI_API_KEY` | Yes | — | LLM Provider API Key |
| `LLM_MODEL` | No | `openai/gpt-4o-mini` | Model name, supports any OpenAI-compatible model |
| `OPENAI_BASE_URL` | No | OpenAI official endpoint | Custom API endpoint, can be used with OpenRouter / Azure / local models |

### Loading rules

harness9 reads the `.env` file from the **working directory (i.e. the directory you were in when you ran the `harness9` command)** at startup.
Variables manually set via `export` or a shell config file **always take precedence** over the same-named variables in the `.env` file.

**Priority:** `export VAR=value` > `work_dir/.env` file

### Option 1: manual `export` (recommended, applies globally)

Add these to `~/.zshrc` or `~/.bashrc` to apply them to all projects, without needing a `.env` file in every directory:

```bash
export OPENAI_API_KEY="sk-..."
export LLM_MODEL="openai/gpt-4o-mini"
# export OPENAI_BASE_URL="https://openrouter.ai/api/v1"
```

### Option 2: `.env` file (project-level configuration)

Place a `.env` file in the project root; it only applies to the current project:

```env
OPENAI_API_KEY=sk-...

# Optional: switch models
LLM_MODEL=openai/gpt-4o-mini

# Optional: use OpenRouter or another compatible API
# OPENAI_BASE_URL=https://openrouter.ai/api/v1
```

> **Note:** The `.env` file contains sensitive information such as API Keys — it is recommended to add it to `.gitignore` to avoid committing it to the code repository.

---

## Conversation and Operations

### Basic conversation

At the `harness9>` prompt, type any question or instruction, and the Agent will carry out the task in the startup directory:

```
harness9> List all Go files in the current directory
harness9> Help me analyze the structure of internal/engine/agent_loop.go
harness9> Add a --version flag in main.go
```

### Exiting

| Method | Description |
|------|------|
| Type `exit` | Exit normally |
| Type `quit` | Exit normally |
| `Ctrl-C` | Sends a cancel signal; the Agent exits after finishing the current operation |
| `Ctrl-D` (EOF) | Exit normally |

### Blank line

Just press Enter to skip; this does not trigger the Agent.

---

## Working Directory

harness9 uses the **process working directory (cwd) at startup** as the Agent's sandbox boundary, with no configuration required:

```bash
cd /your/project
harness9   # sandbox root = /your/project
```

All file reads/writes and command executions are restricted to this directory:

- The `read_file`, `write_file`, and `edit_file` tools refuse access to paths outside the startup directory
- The `bash` tool's working directory is also set to the startup directory
- Path traversal attacks (`../../etc/passwd`) are automatically blocked

---

## Project Guidelines (AGENTS.md)

Place an `AGENTS.md` file in the project root, and the Agent will automatically inject its content into the System Prompt at startup, serving as project-level conventions and contextual guidance.

**Typical uses:**
- Describe the project's architecture, tech stack, and coding conventions
- Specify forbidden operations (e.g. "do not modify go.mod")
- Provide domain background knowledge

**Format:** Standard Markdown, with no formatting restrictions.

```markdown
# My Project Guidelines

## Tech Stack
- Go 1.25
- PostgreSQL 16

## Conventions
- All functions must have comments
- Do not access the database directly; must go through the Repository layer
```

---

## Agent Skills

Skills are domain knowledge documents that can be loaded on demand. Just place `.md` files in the `skills/` subdirectory of your project root:

```
your-project/
├── skills/
│   ├── refactor-guide.md      # Refactoring conventions
│   ├── testing-standards.md   # Testing standards
│   └── api-design.md          # API design conventions
└── AGENTS.md
```

**Skill file format (frontmatter + content):**

```markdown
---
name: refactor-guide
description: Use when refactoring Go code — explains team conventions
trigger: refactor, clean up, restructure
---

# Refactoring Conventions

When refactoring Go code, follow these principles:
1. First run `go vet` and fix all warnings
2. Keep functions under 50 lines
...
```

**Frontmatter fields:**

| Field | Required | Description |
|------|:----:|------|
| `name` | Yes | Unique Skill name, used by the `use_skill` tool call |
| `description` | Yes | Short description, injected into the System Prompt so the Agent is aware of it |
| `trigger` | No | Trigger keywords (documentation only, not used for automatic matching) |

**How it works (Progressive Disclosure):**

1. At startup, every Skill's `name` and `description` are injected into the System Prompt to form an index
2. When needed, the Agent calls the `use_skill` tool to load the full content of a given Skill on demand
3. The full content is not injected up front, saving tokens and keeping the System Prompt lean

The Agent will see something like this in the System Prompt:

```
## Available Skills

Use the `use_skill` tool to load the full content of any skill when needed.

- refactor-guide: Use when refactoring Go code — explains team conventions
- testing-standards: Use when writing or reviewing tests
```

See [Agent Skills Design Principles](agent-skills.md) for details.

---

## Usage Examples

### Code analysis

```
harness9> Help me analyze internal/engine/agent_loop.go and explain its main loop design
```

### Code changes

```
harness9> Create a new list_dir tool under internal/tools/ that lists directory contents
```

### Using a Skill

```
harness9> Help me refactor internal/provider/openai.go (the Agent will automatically load the go-coding-standards Skill)
```

### Running tests

```
harness9> Run go test ./... and analyze the cause of any failures
```

---

## FAQ

**Q: Startup fails with `failed to create Provider`**

Check that `OPENAI_API_KEY` is set correctly, and that the `.env` file is in the directory where you ran the command.

**Q: The Agent cannot read a certain file**

Confirm the file path is within the startup directory. The Agent uses relative paths; absolute paths or `../` path traversal will be blocked.

**Q: I want to use a different model (e.g. Claude, OpenRouter)**

Set `OPENAI_BASE_URL` to point to an endpoint compatible with the OpenAI Chat Completions API, and change `LLM_MODEL` to the corresponding model name:

```env
OPENAI_BASE_URL=https://openrouter.ai/api/v1
LLM_MODEL=anthropic/claude-sonnet-4-5
```

**Q: Does each conversation have memory?**

Yes. harness9 maintains the full conversation history within the same process session (via `memory.Session` + SQLite persistence), and every `harness9>` input is appended to the current session context, so the Agent can reference all previous turns of conversation and tool results. After the process restarts, you need to restore a historical session via the TUI's `/resume` command or by explicitly calling `mgr.OpenSession(id)` in code; the CLI REPL always creates a new session at startup.
