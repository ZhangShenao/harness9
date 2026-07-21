# Sub-Agent System Implementation Principles

harness9's Sub-Agent system lets the main agent delegate **clearly bounded subtasks** to specialized agents with independent context, a restricted toolset, and optional model override. A Sub-Agent is not a new abstraction — it is simply a plain `engine.AgentEngine` instance running on an isolated Session, reusing the existing `RunStream` pipeline without changing a single line of the core `runLoop`.

---

## System Architecture

```
internal/subagent/
├── definition.go   # SubAgentDefinition struct + ResolveTools + Validate
├── registry.go     # Registry: Register / Get / List (registered at startup, read-only at runtime)
├── frontmatter.go  # parseAgentFile: YAML frontmatter + body -> SubAgentDefinition
├── loader.go       # Registry.LoadFromDir: scans .harness9/agents/*.md file-based definitions
├── prompt.go       # promptBuilder: Sub-Agent system prompt + Skills preloading + workDir injection
├── tracker.go      # TaskTracker: single source of truth for background tasks (Start/AppendLog/Finish/DrainCompleted/List/Get)
├── runner.go       # Runner: builds an isolated sub-engine + runs RunStream + bridges approval and progress
└── task_tool.go    # TaskTool: the sole delegation entry point called by the main agent (tools.BaseTool)

cmd/harness9/
├── main.go         # Wiring: registers built-in general-purpose, LoadFromDir, NewRunner, NewTaskTool
├── tui_update.go   # EventSubAgent rendering + TaskTracker.DrainCompleted injection in dispatch() + @agent direct run + task panel keybindings
└── tui_view.go     # renderSubAgentProgress(), renderTaskPanel(), background task status segment in renderStatusBar()
```

---

## Sub-Agent Definitions

### Built-in general-purpose Sub-Agent

harness9 ships a built-in **`general-purpose` Sub-Agent**, deliberately designed to match the same-named capability in two mainstream frameworks:

- **Claude Code**'s [general-purpose subagent](https://code.claude.com/docs/en/sub-agents#general-purpose): "A capable agent for complex, multi-step tasks that require both exploration and action", inheriting all tools and the model of the main conversation — the fallback delegation target for when "no more specialized Sub-Agent" exists.
- **DeepAgents**' [general-purpose subagent](https://docs.langchain.com/oss/python/deepagents/subagents#the-general-purpose-subagent): every deep agent carries one by default, for scenarios that need "context isolation without specialized behavior" — the main agent delegates a whole multi-step task and gets back only a concise conclusion, avoiding polluting the main context with intermediate steps.

Both share the same design core, which harness9 fully inherits:

| Dimension | general-purpose value | Meaning |
|------|----------------------|------|
| `Tools` | Empty (nil) | **Inherits all tools available to the parent agent** — can read/write files, execute commands, invoke skills |
| `Model` | Empty (`""`) | **Inherits the parent agent's model**, no override |
| `MaxTurns` | Empty (0) | Inherits the engine's default turn count (same as the main agent) |
| Positioning | Fallback delegation target | Use when the task is clearly bounded, can be completed independently, and you want context isolation |

**When to delegate to it**: the task requires both exploration and modification, needs complex reasoning to explain intermediate results, or involves multiple interdependent steps, and you only want the final conclusion rather than the verbose intermediate process.

### Programmatic Definition

The built-in `general-purpose` is constructed directly in `main.go` and registered into `subagent.Registry`:

```go
subAgentReg.Register(subagent.SubAgentDefinition{
    Name:         "general-purpose",
    Description:  "General-purpose Sub-Agent for tasks requiring both exploration and modification, complex reasoning, or multi-step dependencies. Use when the task boundary is clear, can be completed independently, and you want context isolation (only the final conclusion returned instead of the verbose intermediate process); it is the default fallback choice when no more specialized Sub-Agent is available. Inherits all tools and the model available to the parent agent.",
    SystemPrompt: generalPurposeSystemPrompt, // Emphasizes "context isolation + self-contained conclusion"
    Source:       "builtin", // Tools/Model/MaxTurns all left empty: tools and model inherit from parent, turn count inherits engine default
})
```

> When more specialized capabilities are needed (e.g. security auditing, documentation writing), prefer adding a new Sub-Agent via the **file-based definition** described below, instead of stacking more programmatic built-ins — keep the core minimal and leave specialized roles to the project side.

### SubAgentDefinition Field Reference

| Field | Type | Description |
|------|------|------|
| `Name` | `string` | Unique identifier, must match `^[a-z0-9][a-z0-9-]*$` |
| `Description` | `string` | The "when to use me" text written for the LLM; the core basis on which the `task` tool schedules |
| `SystemPrompt` | `string` | Sub-Agent system prompt body |
| `Tools` | `[]string` | Tool allowlist; nil/empty = inherit all tools available to the parent |
| `DisallowedTools` | `[]string` | Tool denylist (deny first, then allow) |
| `Model` | `string` | Model override; `""` = inherit the parent agent's model |
| `MaxTurns` | `int` | Maximum turn count; `0` = inherit the default (same as the main agent, currently 50) |
| `Skills` | `[]string` | Skill names to preload at startup (body injected into the Sub-Agent system prompt) |
| `Source` | `string` | Diagnostic field: `"builtin"` or a file path |

### File-Based Definition

Create a `*.md` file under `.harness9/agents/` in the working directory; harness9 scans and loads it automatically at startup. **A file-based definition overrides a programmatic definition of the same name** (logged, no error). If the file has no `name` field, it falls back to the filename (with the `.md` suffix stripped) as the Name.

**Complete example `.harness9/agents/security-auditor.md`**:

```markdown
---
name: security-auditor
description: Security audit expert. Use after code changes involving authentication, authorization, or input validation to detect OWASP Top 10 vulnerabilities.
tools: read_file, bash
disallowed_tools: write_file, edit_file
model: openai/gpt-4o
max_turns: 30
skills: security-review
---

You are an application security engineer focused on identifying security vulnerabilities in code.
When reviewing, output findings prioritized as: Critical > High > Medium > Low, each with a CWE number and a remediation suggestion.
Do not modify files; only output the review report.
```

**Frontmatter field quick reference**:

| Field | Type | Description |
|------|------|------|
| `name` | string | Same as SubAgentDefinition.Name |
| `description` | string | Same as SubAgentDefinition.Description |
| `tools` | comma-separated string | Allowlist, e.g. `read_file, bash` |
| `disallowed_tools` | comma-separated string | Denylist |
| `model` | string | Model override |
| `max_turns` | int | Maximum turn count |
| `skills` | comma-separated string | Names of skills to preload |

---

## The task Tool

`task` is a plain tool (`tools.BaseTool`) registered in the parent agent's tool registry. The LLM calls the `task` tool to delegate subtasks; a Sub-Agent's registry never includes `task`, categorically prohibiting recursion.

### Tool Parameters

| Parameter | Type | Required | Description |
|------|------|:----:|------|
| `subagent_type` | string (enum) | Yes | The Name of a registered Sub-Agent, dynamically enumerated by `Definition()` |
| `prompt` | string | Yes | The complete task description passed to the Sub-Agent. The Sub-Agent cannot see the parent's conversation history, so all necessary information must be written here |
| `description` | string | No | A short 3-5 word title (for UI display) |
| `background` | bool | No | Whether to run asynchronously in the background (default `false`) |

`Definition()` is **dynamically generated** on each invocation, enumerating the Names of all registered Sub-Agents as the `enum` for `subagent_type`, with their Descriptions concatenated into the tool description — this is the basis on which the LLM chooses "which Sub-Agent to call":

```
Delegate a clearly bounded task to a specialized Sub-Agent. The Sub-Agent has independent context and a restricted toolset.
Available Sub-Agents:
- general-purpose: General-purpose Sub-Agent for tasks requiring both exploration and modification, complex reasoning, or multi-step dependencies. Use when the task is clearly bounded, can be completed independently, and context isolation is desired; it is the default fallback choice when no more specialized Sub-Agent is available. Inherits all tools and the model available to the parent agent.
- security-auditor: Security audit expert. ... (file-based definition)
```

### Foreground Execution (`background=false`, default)

```
task tool call
    │ execCtx = the calling context of the parent
    ▼
Runner.Run(..., background=false)
    │ Builds an isolated sub-engine, calls RunStream, consumes the event stream
    │ Approval request → parentApproval(ctx, ...) → forwarded to the parent's TUI approval dialog
    ▼
Blocks until the sub-engine's channel closes
    │
    ▼
Returns <task state="completed"><task_result>...final text...</task_result></task>
```

The tool result of foreground execution is injected directly as the tool call's Output into the parent agent's context history, so the main agent can immediately read the Sub-Agent's output.

### Background Execution (`background=true`)

```
task tool call
    │
    ▼
The task tool immediately returns <task id="task-general-purpose-1" state="running"/>
    │
    ▼ Simultaneously: go func(){...}()
        execCtx is derived from the session-level baseCtx (independent of the parent turn, unaffected by the tool's 60s timeout)
        Approval requests → always denied (fail-closed), returning "no approval channel available for the Sub-Agent, automatically denied"
        Sub-engine event stream → tracker.AppendLog(id, update) (full-process logs written to memory, locked, not routed through a channel)
        Sub-engine execution completes → tracker.Finish(id, finalText, isErr)
            │ Triggers the SetNotify callback → tea.Program.Send(subAgentNotifyMsg) → TUI instantly displays a completion notice

Before the next dispatch():
    tracker.DrainCompleted() → prepended to the prompt → injected into the LLM context
```

---

## Execution Model and Context Propagation

### The Two-Phase Execution of Runner

`Runner.Run` is the core of Sub-Agent execution:

1. **Build an isolated registry**: `buildChildRegistry` filters tools by `ResolveTools` (allowlist ∩ full set - denylist - task), wrapped with `permission.NewFileHook` (inheriting the same `settings.json`) + `denyTaskHook` (anti-recursion) + sharedHooks (dangerHook + offloadHook).
2. **Resolve the Provider**: when `def.Model != ""`, a new OpenAI Provider is created and its context window is looked up; when `""`, the parent agent's model is reused.
3. **Build the PromptBuilder**: Sub-Agent system prompt + workDir injection + the body of skills listed in `def.Skills` (loaded via `skills.Index.GetFullContent`, silently ignored on failure).
4. **Independent MemorySession**: `memory.NewMemorySession(childID)` (in-memory only, containing neither the parent's conversation history nor the parent's system prompt).
5. **Start RunStream**: `sub.RunStream(execCtx, prompt)`, consuming the event stream, forwarding progress, bridging approvals, and accumulating the final text.

### Context Propagation Rules

```
Main agent ──► task tool ──► prompt string ──► Sub-Agent (the only source of information)
Sub-Agent ──► FinalText ──► tool result ──► parent agent's context (foreground)
Sub-Agent ──► TaskTracker ──► DrainCompleted ──► prepended to the parent agent's next prompt (background)
```

The Sub-Agent **cannot see** the parent agent's conversation history or system prompt. File paths, background information, and requirement details must be passed explicitly through the `task` tool's `prompt` parameter.

### Execution Context Differences

| Dimension | Foreground (`background=false`) | Background (`background=true`) |
|------|--------------------------|--------------------------|
| execCtx source | The parent caller's `ctx` (within the 60s tool timeout) | Derived from the session-level `baseCtx` (independent of the parent turn) |
| Approval policy | Forwards the parent's `ApprovalFunc`, TUI approval dialog available | Always denied (fail-closed) |
| Result delivery | tool result returned synchronously | `TaskTracker.Finish` writes to memory; injected via `DrainCompleted` at the next dispatch |
| Progress log | Rendered live to subAgentLines via `EventSubAgent` | Buffered in memory via `TaskTracker.AppendLog`, viewable via the `/tasks` panel |
| Cancellation propagation | Parent ctx cancellation → Sub-Agent cancelled accordingly | Cancelled only when baseCtx is cancelled (process shutdown) |

---

## TUI Live Progress Rendering

While a foreground Sub-Agent is executing, the TUI appends `[agent-name]`-prefixed dark-cyan progress lines in real time below the tool progress area:

```
  [general-purpose] Sub-Agent starting...
  [general-purpose] ▸ read_file
  [general-purpose]   ✓
  [general-purpose] ▸ bash
  [general-purpose]   ✓
  [general-purpose] Root cause located, summarizing conclusion...
  [general-purpose] ✓ Done
```

At most `maxSubAgentLines = 12` of the most recent progress lines are retained, preventing unbounded growth from a long-running Sub-Agent. `SubAgentThinking` (reasoning delta) is intentionally not displayed, to reduce noise.

Progress data flow: `Runner.emit(SubAgentUpdate)` → `hooks.SubAgentProgressFunc` (injected into context) → `RunStream` converts to `EventSubAgent` event → TUI `EventSubAgent` case → appended to `m.subAgentLines`.

---

## Security Guarantees

| Security layer | Mechanism | Description |
|--------|------|------|
| Anti-recursion | The child registry never includes the `task` tool | `ResolveTools` hardcodes `denied["task"]=true` |
| Anti-recursion (defense in depth) | `denyTaskHook.BeforeExecute` | Double defense: even if future code introduces `task`, the hook will still deny it |
| No privilege escalation | Inherits the same `.harness9/settings.json` | `permission.NewFileHook(settingsPath)` reuses the same rules file |
| Permissions only additively stricter | Sub-Agent additionally layers on DisallowedTools + denyTaskHook | Can only be more restricted than the parent, never more permissive |
| Context isolation | Independent `MemorySession` (in-memory only) | Contains neither the parent's conversation history nor the parent's system prompt — no data leak path |
| Tool isolation | `ResolveTools` (allowlist ∩ full set - denylist - task) | Only explicitly allowed tool instances are registered |
| Background approval fail-closed | Background Sub-Agent approvals are always denied | Without a TUI channel, dangerous operations are denied rather than auto-approved |
| Sensitive paths | sharedHooks includes `dangerHook` | 19 high-risk patterns (`~/.ssh`, `~/.aws`, etc.) protect Sub-Agents as well |

---

## TaskTracker — Single Source of Truth for Background Tasks

`TaskTracker` is the thread-safe single source of truth for background Sub-Agent tasks, replacing the old `Mailbox`, and taking on both full-process log buffering and result injection responsibilities:

### API Overview

| Method | Caller | Description |
|------|--------|------|
| `Start(agentName, prompt) string` | When a background goroutine starts | Registers a Running task, returns a unique `id` (format `task-{agent}-{seq}`) |
| `AppendLog(id, SubAgentUpdate)` | While the background goroutine streams progress | Appends the progress event to the in-memory buffer (locked), not routed through any channel |
| `Finish(id, finalText, isErr)` | When the background goroutine completes | Marks Done/Failed, triggers the `SetNotify` callback (called outside the lock) |
| `DrainCompleted() []CompletedTask` | Before TUI `dispatch()` | Returns completed-but-not-yet-injected results, marking them as injected (idempotent) |
| `List() []TaskSnapshot` | TUI task panel | Full snapshot, in creation order |
| `Get(id) (TaskDetail, bool)` | TUI task detail | Returns a `TaskDetail` with a deep copy of the full-process log |
| `RunningCount() int` | TUI status bar | Number of running tasks |
| `DoneCount() int` | TUI status bar | Number of finished (completed + failed) tasks |
| `SetNotify(fn func())` | At TUI initialization | Registers the completion notification callback |

### Two Independent Paths

**Injection path**: `Finish` writes the final text to memory; on the parent agent's **next dispatch**, `DrainCompleted` drains it and prepends it into the LLM context (the `pendingSubAgentInject` buffer). `DrainCompleted` is idempotent — an already-injected result will not be taken again.

**Notification path**: `Finish` also triggers the `SetNotify` callback — at startup the TUI registers it as `tea.Program.Send(subAgentNotifyMsg{})`, and the moment a background task completes, a "✓ background Sub-Agent completed" notice is appended to the scrollback (display only, does not consume the injection buffer; the two paths do not interfere with each other).

**Full-process log**: `AppendLog` writes directly to the in-memory buffer (locked), never going through a channel at all, fundamentally eliminating the risk of send-on-closed-channel. The log is exposed to the `/tasks` panel detail view via `Get(id).Log`.

---

## Background Task Viewer

### Status Bar Indicator

The status bar automatically shows a task count segment when background tasks exist:

```
⚙ 2 running/3 done
```

Populated by `renderStatusBar()` calling `TaskTracker.RunningCount()` and `DoneCount()` in real time; shown only when at least one task exists (running or completed), taking up no status bar space when there are zero tasks.

### Opening the Panel

Two equivalent methods:

| Method | Description |
|------|------|
| `Ctrl+T` | Keyboard shortcut toggle (available in idle state; ignored when in conflict with running, approval, review, resume-selection, or other modals) |
| `/tasks` + Enter | Slash command, with an effect identical to `Ctrl+T` |

The panel is a **modal view**: while active, `taskPanelMode = true`, and `View()` replaces the input area with the panel content rendered by `renderTaskPanel()`; ordinary input and all other shortcuts are taken over entirely by `handleTaskPanelKey`.

### List View

The panel shows the task list by default when opened, with each line formatted as:

```
{● running/✓ done/✗ failed}  {agent}  {status text}  "{first 48 bytes of prompt}"
```

The currently selected row is highlighted with `▶`. Key bindings:

| Key | Action |
|------|------|
| `↑` / `↓` | Move the cursor |
| `Enter` | Enter the detail view for the selected task |
| `Esc` or `Ctrl+T` | Close the panel, return to normal input mode |

### Detail View

Pressing `Enter` on a selected task enters the detail view, showing that background Sub-Agent's full-process log (a deep copy of `TaskDetail.Log` obtained via `TaskTracker.Get(id)`):

```
general-purpose — Done  (↑↓ to scroll, Esc to return)

Starting...
▸ read_file(main.go)
▸ bash(go vet ./...)
  ✗ Tool execution failed
Found 2 security issues...

— Final Result —
Suggested fixes for the following two issues...
```

Log rendering is done by `formatTaskLog`, covering five event kinds: `SubAgentStart / SubAgentToolStart / SubAgentDelta / SubAgentToolResult (failures only) / SubAgentError`, with `SubAgentDone` and `FinalText` merged into a trailing "Final Result" block.

| Key | Action |
|------|------|
| `↑` / `↓` | Scroll the log (`taskDetailScroll` offset) |
| `Esc` | Return to the list view (`taskDetailID = ""`) |
| `Ctrl+T` | Close the entire panel |

### Live Refresh

Running tasks read the `TaskTracker` snapshot directly (`List()` / `Get()`) on every panel render, requiring no subscription to notifications — the TUI main loop alone keeps the log line count (`LogLines`) updated in real time.

---

## @ Mention Invocation

### Basic Usage

Send in the input box using the format `@<agent> <task>`, which **bypasses the main LLM's tool decision** and directly invokes the specified Sub-Agent in the foreground:

```
@general-purpose Investigate the timeout handling logic in internal/tools/bash.go and summarize the implementation approach
```

After sending:
1. The TUI immediately appends a user message line (`▶ You: @general-purpose …`)
2. A Sub-Agent name line (`◆ general-purpose:`) is appended to the scrollback
3. `running = true`, the input box is disabled
4. Sub-Agent streaming progress is rendered live to `subAgentLines` (via exactly the same rendering path as the `task` tool's foreground execution)
5. Upon completion, the final text is appended directly to the scrollback (landing in the conversation as an assistant message), `running = false`, and the input box is re-enabled

### Tab Completion for Sub-Agent Names

After typing `@` in the input box, pressing `Tab` auto-completes registered Sub-Agent names:

```
@gen[Tab] → @general-purpose 
```

The completion logic is handled in `cycleCompletion()`, guarded by `@` alongside `/` slash command completion, sharing the same `typedPrefix / completions / completionIdx` cycling state; multiple `Tab` presses cycle through all matching names.

### Ctrl+C Cancellation

Pressing `Ctrl+C` while `@agent` is executing: `cancelFn()` cancels the derived sub-context, `execCtx.Done()` fires inside the Runner, and the sub-engine's `RunStream` exits accordingly; `subAgentDirectMsg{done: true, err: ctx.Err()}` is sent back to the TUI via the channel, `running = false`, and the input box is re-enabled.

### Foreground vs Background

The `@` syntax **only supports foreground execution** (`background=false`).

When background execution is needed, express the intent to the main agent in natural language (e.g. "check the latest commit in the background using general-purpose"), letting the main LLM decide to call the `task` tool with `background=true`, with the result appearing in the `/tasks` panel.

| Dimension | `@agent task` (foreground direct run) | Main LLM → `task(background=true)` |
|------|--------------------------|-----------------------------------|
| Trigger | Direct user input | Main LLM tool decision |
| Main LLM involvement | No, fully bypassed | Yes, the LLM chooses the Sub-Agent and prompt |
| Execution mode | Foreground blocking, streaming progress visible | Background asynchronous, result stored in TaskTracker |
| Result landing point | Shown directly in scrollback | `/tasks` panel + injected at next dispatch |
| Cancellation | Instant cancellation via `Ctrl+C` | Cancelled only when baseCtx is cancelled (process shutdown) |

---

## Data Flow Summary

```
Main agent LLM
    │  Decides to call the task tool
    ▼
TaskTool.Execute(ctx, args)
    │  Parses subagent_type / prompt / background
    ▼
Runner.Run(ctx, def, prompt, background)
    ├─ buildChildRegistry(def)
    │       ResolveTools → allowlist ∩ full set - denylist - task
    │       hookChain: permFileHook → denyTaskHook → dangerHook → offloadHook
    │
    ├─ providerFor(def.Model) → LLMProvider + ctxWindow
    │
    ├─ newPromptBuilder(def.SystemPrompt, workDir, def.Skills, skillsLoader)
    │       systemPrompt + workDir + skills body
    │
    ├─ memory.NewMemorySession(childID)   # Independent in-memory-only Session
    │
    └─ engine.NewAgentEngine(provider, childReg, workDir, opts...)
           │
           sub.RunStream(execCtx, prompt)
           │
           ▼
       Event stream consumption loop
           ├─ EventActionDelta   → emit(SubAgentDelta)   → EventSubAgent → TUI progress line
           ├─ EventThinkingDelta → emit(SubAgentThinking)  (not shown in TUI)
           ├─ EventToolStart     → emit(SubAgentToolStart) → TUI progress line
           ├─ EventToolResult    → emit(SubAgentToolResult)→ TUI progress line
           ├─ EventApprovalRequired → foreground: forwarded to parent ApprovalFunc / background: auto-denied
           ├─ EventError         → emit(SubAgentError) → returns error
           └─ EventDone          → channel closes, loop ends naturally
                   │
    Foreground: return FinalText → task tool result → parent agent context
    Background: tracker.AppendLog(id, update) (streaming, full-process log to memory)
          tracker.Finish(id, finalText, isErr)
              → next dispatch() → DrainCompleted() → prepended to prompt → main agent LLM
```

---

## Wiring Example (main.go)

```go
// 1. Sub-Agent base tool instances (sandbox root = workDir)
subAgentBaseTools := []tools.BaseTool{
    tools.NewReadFileTool(workDir),
    tools.NewWriteFileTool(workDir),
    tools.NewBashTool(workDir),
    tools.NewEditFileTool(workDir),
    skills.NewUseSkillTool(skillsIndex),
}

// 2. Definition registry: register the built-in general-purpose first, then load file-based definitions
subAgentReg := subagent.NewRegistry()
subAgentReg.Register(subagent.SubAgentDefinition{
    Name: "general-purpose", Description: "General-purpose Sub-Agent…", SystemPrompt: generalPurposeSystemPrompt,
    Source: "builtin", // Tools/Model left empty -> inherits all tools and the model available to the parent agent
})
subAgentReg.LoadFromDir(filepath.Join(workDir, ".harness9", "agents"))

// 3. Runner: hold a single global instance, read-only at runtime
subAgentTracker := subagent.NewTaskTracker()
subAgentRunner := subagent.NewRunner(subagent.RunnerConfig{
    BaseTools:       subAgentBaseTools,
    SharedHooks:     []hooks.ToolHook{dangerHook, offloadHook},
    SettingsPath:    settingsPath,
    SkillsIndex:     skillsIndex,
    WorkDir:         workDir,
    DefaultMaxTurns: agentMaxTurns, // = main agent's 50, Sub-Agent matches the main agent
    ToolTimeout:     60 * time.Second,
    ProviderFor:     func(model string) (provider.LLMProvider, int, error) { ... },
    CompactorFor:    func(p provider.LLMProvider, ctxWin int) memory.Compactor { ... },
    BaseCtx:         ctx,
})

// 4. Register the task tool into the parent agent's registry
taskTool := subagent.NewTaskTool(subAgentReg, subAgentRunner, subAgentTracker)
registry.Register(taskTool)
```

---

## File Index

| File | Responsibility |
|------|------|
| `internal/subagent/definition.go` | `SubAgentDefinition` struct, `Validate`, `ResolveTools` |
| `internal/subagent/registry.go` | `Registry`: `Register` / `Get` / `List` |
| `internal/subagent/frontmatter.go` | `parseAgentFile`: YAML frontmatter parsing |
| `internal/subagent/loader.go` | `Registry.LoadFromDir`: file-based definition loading |
| `internal/subagent/prompt.go` | `promptBuilder`: system prompt + skills + workDir assembly |
| `internal/subagent/tracker.go` | `TaskTracker`: single source of truth for background tasks (full-process log + result injection) |
| `internal/subagent/runner.go` | `Runner`: builds the isolated sub-engine + executes + forwards events |
| `internal/subagent/task_tool.go` | `TaskTool`: `task` tool implementation (foreground / background) |
| `internal/schema/subagent.go` | `SubAgentUpdate` / `SubAgentUpdateKind` type definitions |
| `internal/hooks/subagent_progress.go` | `SubAgentProgressFunc`: context injection/extraction |
| `internal/engine/stream.go` | `EventSubAgent`, `EventApprovalRequired`, progress sink injection |
| `cmd/harness9/main.go` | Complete wiring: built-in Sub-Agent registration, Runner construction, task tool registration |
| `cmd/harness9/tui_update.go` | `EventSubAgent` handling, `TaskTracker.DrainCompleted` injection in `dispatch()`, `dispatchMention` (@ foreground direct run), `handleTaskPanelKey` (task panel key bindings) |
| `cmd/harness9/tui_view.go` | `renderSubAgentProgress()` (dark-cyan progress block), `renderTaskPanel()` (panel list/detail), background task count in `renderStatusBar()` |
