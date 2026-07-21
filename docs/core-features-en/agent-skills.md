# Agent Skills Technical Design

## 1. Design Rationale: Progressive Disclosure

The Skills system follows the **Progressive Disclosure** principle:

- **At startup**: only the skills' `name` + `description` index is injected into the System Prompt — the full text is not loaded
- **At runtime**: the LLM decides whether a given skill is needed based on the user's task, and loads the full text on demand by calling the `use_skill` tool

This avoids the token bloat caused by injecting the full text of every skill into the System Prompt up front, keeping the context window used efficiently.

## 2. Directory Structure

Each skill is an independent subdirectory; the directory name is the skill identifier, and `SKILL.md` is the fixed filename:

```
{workdir}/skills/
├── go-coding-standards/
│   └── SKILL.md
├── debugging-guide/
│   └── SKILL.md
└── architecture-overview/
    └── SKILL.md
```

On startup, harness9 automatically scans all subdirectories under `{workdir}/skills/` and loads each one's `SKILL.md`.

## 3. Skill File Format

Each `SKILL.md` is a standard Markdown file that starts with YAML frontmatter:

```markdown
---
name: go-refactor
description: Use when refactoring Go code — team conventions and patterns
trigger: "refactor, clean up, restructure, simplify"
---

# Go 重构指南

## 重构前必做

1. 运行 `go vet ./...` 确认无静态分析错误
2. 运行 `go test ./...` 确认测试全部通过
3. 查看 git diff 确认修改范围
```

### frontmatter Field Reference

| Field | Type | Required | Description |
|------|------|------|------|
| `name` | string | **Yes** | Unique skill identifier (used by the `use_skill` tool call) |
| `description` | string | **Yes** | Short description injected into the System Prompt index, helping the LLM decide when to use it |
| `trigger` | string | No | Trigger keywords, documentation only, not used for automatic matching |

## 4. Trigger Mechanisms

Skills support two trigger mechanisms:

### 4.1 Tool-Calling (Primary Mechanism)

After seeing the skills index in the System Prompt, the LLM autonomously decides whether a given skill is needed and triggers it by calling the `use_skill` tool:

```
End of System Prompt (skills index) → LLM identifies an applicable skill
  → tool_use: {name: "use_skill", arguments: {skill_name: "go-refactor"}}
  → the framework loads the SKILL.md body and returns it as a tool_result
  → the LLM continues execution under the full skill instructions
```

### 4.2 Slash Commands (CLI Shortcut Path)

In CLI REPL mode, users can directly type `/skill-name` to trigger a skill; the framework loads the body directly, bypassing the LLM's judgment:

```
/go-refactor               → prompt = skill body
/go-refactor 清理 main.go  → prompt = skill body + "\n\n" + "清理 main.go"
```

> Slash commands are only supported in CLI mode (non-TTY pipe environments); in TUI mode, skills are activated via the `/skill` command or direct conversation.

## 5. System Prompt Injection Result

Once harness9 starts, the skills index is appended to the end of the System Prompt:

```
## Available Skills

Use the `use_skill` tool to load the full content of any skill when needed.

- go-refactor: Use when refactoring Go code — team conventions and patterns
- testing-guide: Use when writing or reviewing tests
- deploy-guide: Use when deploying to production
```

## 6. LLM Calling the use_skill Tool

When the LLM determines that it needs the full content of a skill, it issues a tool call:

```json
{
  "name": "use_skill",
  "arguments": {
    "skill_name": "go-refactor"
  }
}
```

The tool returns the full body content of that skill file (everything after the frontmatter), and the LLM then uses this content to guide task execution.

## 7. Module Implementation

| Module | File | Responsibility |
|------|------|------|
| `skills.Skill` | `internal/skills/skill.go` | Data structure + frontmatter parsing |
| `skills.Index` | `internal/skills/index.go` | Index summary + lazy full-text loading |
| `skills.LoadSkills` | `internal/skills/loader.go` | Scans subdirectories to build the Index |
| `skills.UseSkillTool` | `internal/skills/use_skill_tool.go` | `use_skill` tool implementation |
| `context.DefaultPromptBuilder` | `internal/context/builder.go` | Assembles the System Prompt |

## 8. Error Handling

| Scenario | Behavior |
|------|------|
| `skills/` directory does not exist | Returns an empty Index, silently skipped |
| Subdirectory missing `SKILL.md` | Skips the directory, prints a warn log |
| Skill file missing `name` or `description` | Skips the file, prints a warn log |
| `use_skill` called with a nonexistent skill | Returns an error message containing the list of available names, allowing the LLM to self-heal |
| CLI slash command points to a nonexistent skill | Prints the error to stderr, continues the REPL loop |
| `AGENTS.md` does not exist | PromptBuilder skips this section |

## 9. CLI Startup

```bash
cd /your/project
harness9   # TTY → TUI, pipe/CI → CLI REPL
```
