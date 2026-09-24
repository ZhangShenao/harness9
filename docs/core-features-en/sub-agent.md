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
├── builtin.go      # RegisterBuiltins: six built-in Sub-Agents (compiled into the binary, overridable by same-name files)
├── prompt.go       # promptBuilder: Sub-Agent system prompt + Skills preloading + workDir injection
├── tracker.go      # TaskTracker: single source of truth for background tasks (Start/AppendLog/Finish/Control/List/Get)
├── control.go      # TaskController: the control plane for one background task (implements engine.TaskGate)
├── runner.go       # Runner: builds an isolated sub-engine + runs RunStream + bridges approval and progress
├── task_tool.go    # TaskTool: the sole delegation entry point called by the main agent (tools.BaseTool)
├── task_status.go  # TaskStatusTool: observe background tasks (the task_status tool)
├── task_wait.go    # TaskWaitTool: wait for background tasks to finish (the task_wait join primitive)
└── task_control.go # TaskControlTool: control background tasks (the task_control tool)

internal/engine/
└── task_gate.go    # TaskGate interface: turn-boundary control gate (interface defined on the consumer side, injected via WithTaskGate)

cmd/harness9/
├── main.go         # Wiring: RegisterBuiltins, LoadFromDir, NewRunner, task + the three coordination tools
├── tui_update.go   # EventSubAgent rendering + DrainCompleted injection + auto-wake (maybeAutoWake) + @agent direct run + task panel keybindings
└── tui_view.go     # renderSubAgentProgress(), renderTaskPanel() (five-state coloring), background task status segment in renderStatusBar()
```

---

## Sub-Agent Definitions

### Built-in Sub-Agent Library (Six)

harness9 ships **six built-in Sub-Agents** (`general-purpose` / `explorer` / `researcher` / `implementer` / `reviewer` / `planner`), compiled into the binary and available out of the box — the complete roster is in the built-in table of the [Async Scheduling and Coordination Control](#async-scheduling-and-coordination-control) chapter. Among them, `general-purpose` is the fallback delegation target, deliberately designed to match the same-named capability in two mainstream frameworks:

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

The six built-in Sub-Agents are defined and registered centrally in `RegisterBuiltins(reg)` in `internal/subagent/builtin.go` (called once at startup from `main.go`; returns an error on invalid or duplicate definitions). Taking `general-purpose` as an example:

```go
subagent.SubAgentDefinition{
    Name:         "general-purpose",
    Description:  "General-purpose Sub-Agent for tasks requiring both exploration and modification, complex reasoning, or multi-step dependencies. ... Inherits all tools and the model available to the parent agent.",
    SystemPrompt: generalPurposeSystemPrompt, // Emphasizes "context isolation + self-contained conclusion"
    Source:       "builtin", // Tools/Model/MaxTurns all left empty: tools and model inherit from parent, turn count inherits engine default
}
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
- explorer: Read-only deep exploration expert: understand project structure, locate implementations, trace call relationships. …
- security-auditor: Security audit expert. … (file-based definition)
Usage patterns:
- Parallel delegation: issue multiple background=true tasks in one reply to run in parallel, then aggregate results with task_wait
- Observe/wait: query background task state with task_status, block for completion with task_wait
- Course-correct: when a background task drifts, inject a steer instruction via task_control (effective next turn) instead of cancelling and rerunning
- Control: task_control supports pause/resume/cancel/steer
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
| Anti-recursion | The child registry never includes the task family of four tools | `ResolveTools` hardcodes the removal of `task` / `task_status` / `task_wait` / `task_control` (`alwaysDeniedTools`, regardless of how the allowlist/denylist is declared) |
| Anti-recursion (defense in depth) | `denyTaskHook.BeforeExecute` | Double defense: even if future code introduces a task-family tool, the hook will still deny it at runtime |
| No cross-task manipulation | Same `alwaysDeniedTools` | A Sub-Agent must not manipulate sibling tasks or probe/control the main agent's TaskTracker (the coordination plane is main-agent-exclusive) |
| No privilege escalation | Inherits the same `.harness9/settings.json` | `permission.NewFileHook(settingsPath)` reuses the same rules file |
| Permissions only additively stricter | Sub-Agent additionally layers on DisallowedTools + denyTaskHook | Can only be more restricted than the parent, never more permissive |
| Context isolation | Independent `MemorySession` (in-memory only) | Contains neither the parent's conversation history nor the parent's system prompt — no data leak path |
| Tool isolation | `ResolveTools` (allowlist ∩ full set - denylist - task family) | Only explicitly allowed tool instances are registered |
| Background approval fail-closed | Background Sub-Agent approvals are always denied | Without a TUI channel, dangerous operations are denied rather than auto-approved |
| Exactly-once result injection | `injected` flag shared by three paths | `DrainCompleted` (auto injection) / `task_status` / `task_wait` share the same flag — the same result never enters the context twice |
| Sensitive paths | sharedHooks includes `dangerHook` | 19 high-risk patterns (`~/.ssh`, `~/.aws`, etc.) protect Sub-Agents as well |

---

## TaskTracker — Single Source of Truth for Background Tasks

`TaskTracker` is the thread-safe single source of truth for background Sub-Agent tasks, replacing the old `Mailbox`, and taking on both full-process log buffering and result injection responsibilities:

### API Overview

| Method | Caller | Description |
|------|--------|------|
| `Start(agentName, description, prompt) string` | When a background goroutine starts | Registers a Running task, returns a unique `id` (format `task-{agent}-{seq}`) |
| `AppendLog(id, SubAgentUpdate)` | While the background goroutine streams progress | Appends the progress event to the in-memory buffer (locked), not routed through any channel |
| `Finish(id, finalText, isErr)` | When the background goroutine completes | Marks Done/Failed, triggers the `SetNotify` callback (called outside the lock); a terminal task cannot be overwritten |
| `Attach(id, *TaskController)` | Before the TaskTool background path starts | Attaches the control plane to the task record for Control routing |
| `Control(id, action, message)` | The `task_control` tool / TUI panel | The single control entry: validates existence and terminal state, then routes to the controller, `action ∈ pause/resume/cancel/steer`; pause/resume synchronously write the snapshot state back on success |
| `Cancel(id, reason)` | The TaskTool background goroutine | Marks TaskCancelled (distinct from Failed), writes the reason into finalText and notifies |
| `MarkInjected(ids...)` | `task_status` / `task_wait` | Marks results as consumed, sharing the `injected` flag with `DrainCompleted` (exactly-once injection) |
| `DrainCompleted() []CompletedTask` | Before TUI `dispatch()` | Returns completed-but-not-yet-injected results, marking them as injected (idempotent); only terminal states (Done/Failed/Cancelled) are drained — Paused is not |
| `List() []TaskSnapshot` | TUI task panel | Full snapshot, in creation order |
| `Get(id) (TaskDetail, bool)` | TUI task detail | Returns a `TaskDetail` with a deep copy of the full-process log |
| `RunningCount() int` | TUI status bar | Count of active (non-terminal) tasks: running + paused |
| `DoneCount() int` | TUI status bar | Count of finished (completed + failed + cancelled) tasks |
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

A separate live segment, "Tasks N▸M", tracks active tasks in real time: N counts active tasks (running plus paused), and when any task is paused a `▸M` suffix breaks out the paused count (making pause states produced by the `p` key visible at a glance); the segment is hidden when there are no active tasks.

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
{icon}  {id} [{state}]  {agent}  "{description}"  {elapsed}; last: {activity}
```

The icon takes only three values: the default `●` (shared by running / paused / cancelled), `✓` for done, and `✗` for failed — paused and cancelled tasks do not switch icons and are told apart by the color of the `[{state}]` label. The state label is color-coded per the five states (running green / paused yellow / cancelled gray / done blue / failed red); the line format mirrors the `task_status` tool's single-line summary. The currently selected row is highlighted with `▶`. Key bindings:

| Key | Action |
|------|------|
| `↑` / `↓` | Move the cursor |
| `Enter` | Enter the detail view for the selected task |
| `Esc` or `Ctrl+T` | Close the panel, return to normal input mode |

The four control actions — pause / resume / cancel / steer — can also be issued directly from the list view with keys (`p` / `r` / `x` / `s`); see the ["TUI Panel Control" section](#tui-panel-control) in the Async Scheduling and Coordination Control chapter below.

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

## Async Scheduling and Coordination Control

A `background=true` task is not fire-and-forget — harness9 builds a complete loop of control, observation, and result back-flow around it: the main agent can pause, resume, cancel, or steer a running background Sub-Agent at any time, wait on and aggregate multiple parallel tasks, and is automatically woken when a task finishes. Three planes cooperate to deliver this.

### Three-Plane Architecture

```
┌──────────────────────────────────────────────────────────────────┐
│ Control Plane                                                     │
│   TaskController (internal/subagent/control.go, one per task)     │
│   implements engine.TaskGate (internal/engine/task_gate.go)       │
│   Pause / Resume / Cancel / Steer — turn-boundary gate + execCtx  │
└───────────────▲──────────────────────────────────┬───────────────┘
                │ tracker.Control(id, action, msg) │ WithTaskGate(ctl) injected into the sub-engine
┌───────────────┴──────────────────────────────────▼───────────────┐
│ Coordination Plane — main-agent-exclusive tools                   │
│   task_status (observe)  task_wait (join)  task_control (control) │
│   all routed through TaskTracker (single source of truth)         │
└───────────────┬──────────────────────────────────────────────────┘
                │ TaskTracker.Start / Attach / Finish / DrainCompleted
┌───────────────▼──────────────────────────────────────────────────┐
│ Data Plane                                                       │
│   Runner: builds an isolated sub-engine (own registry +           │
│   MemorySession + PlanStore), runs RunStream; progress buffered   │
│   via AppendLog, terminal state booked via Finish / Cancel        │
└──────────────────────────────────────────────────────────────────┘
```

- **Control plane**: `TaskController` is the control handle for one background task — concurrency-safe, with `Pause` / `Resume` / `Cancel` idempotent on non-target states. It implements the `engine.TaskGate` interface (defined on the consumer side, in the engine package) and is injected into the sub-engine by `Runner` via `engine.WithTaskGate(ctl)`; the main engine path injects nothing (nil) at zero cost.
- **Coordination plane**: the three tools `task_status` / `task_wait` / `task_control` are registered **only** in the main agent's registry — the sole window through which the main agent's LLM observes, waits on, and controls background tasks. They all route through `TaskTracker.Control` and friends and never touch the sub-engine directly.
- **Data plane**: `Runner` builds a fully isolated sub-engine per delegation and runs `RunStream`; progress is buffered via `AppendLog` and terminal state is booked into `TaskTracker` via `Finish` / `Cancel`.

### Turn-Boundary Gating Semantics

When a control action takes effect is decided by the insertion point of `engine.TaskGate`: the sub-engine's `runLoop` calls `AwaitTurn` **before every turn** (before `beginTurn`) — it blocks while paused, and on release returns the queued steer messages.

| Action | Effective | Semantics |
|------|---------|------|
| `pause` | After the current turn | In-flight LLM calls and tool executions **run to the end of the current turn**, then halt at the turn boundary; `AwaitTurn` blocks while paused and **consumes no MaxTurns quota** |
| `resume` | Immediately | Closes the blocking channel, releases the gate, and continues with the next turn |
| `cancel` | Immediately | Cancels the Sub-Agent's execCtx — in-flight LLM calls and tool executions are **interrupted on the spot** (same semantics as the user's Ctrl+C), and a gate blocked in pause is woken as well |
| `steer` | At the start of the next turn | The steer message enters a mailbox (steerBox); when the gate releases, all queued messages are taken in one shot and **persisted into the Sub-Agent's history as user-role messages** (prefixed `[主代理转向指令]`, "main-agent steering instruction") before the loop continues |

Three key semantic boundaries:

1. **Pause does not consume MaxTurns**: the paused interval sits before `beginTurn` counting, so a Sub-Agent can never "run out of turns by being paused too long". This is the core trade of turn-boundary gating over mid-turn preemption — it never corrupts in-flight calls, at the cost of at most one turn of delay before a pause takes effect.
2. **Steer persists as a user message**: steering is part of the Sub-Agent's conversation (unlike nudges, which are defensive copies) — persisted with the history and visible to every subsequent turn of reasoning. Steer does **not** auto-resume a paused task (resuming is the main agent's explicit call — separation of responsibilities); if the task ends before the mailbox is drained, the messages are dropped (best-effort).
3. **Cancel interrupts immediately**: after deriving execCtx, `Runner` binds the cancel function via `ctl.bindExec(cancel)`; if `Cancel` arrives first (during the sandbox-creation window), `bindExec` replays the cancellation — closing the lost-cancel window. Terminal states (Done / Failed / Cancelled) cannot migrate: a late `Finish` after cancellation is a no-op and never overwrites.

### The Three Coordination Tools

#### task_status — Observe

| Parameter | Type | Required | Description |
|------|------|:----:|------|
| `task_id` | string | No | The task id to query; **omitted = all tasks** |

Single-task output (terminal tasks append the final result, truncated to 2048 runes):

```
task-general-purpose-1 [done] general-purpose "survey timeout handling" 1m32s; last: see task panel
Fix the following two spots…
```

The all-tasks output is one summary line per task, with result text appended for terminal tasks. **A finished task's result is returned with the query and marked injected** (`MarkInjected`, sharing the flag with `DrainCompleted` / `task_wait`) — the auto-injection channel will not deliver it again. Running tasks are **not** marked (otherwise `DrainCompleted` would skip them forever after `Finish`, severing the auto-injection channel).

#### task_wait — Wait (join primitive)

| Parameter | Type | Required | Description |
|------|------|:----:|------|
| `task_ids` | string[] | No | Task ids to wait for (each validated for existence — a typo'd id errors out instead of silently waiting on nothing) |
| `all` | bool | No | Wait for all running tasks (the default semantics when `task_ids` is omitted) |
| `timeout_sec` | int | No | Wait cap in seconds (default 60, clamped to 600) |

Key semantics:

- **The wait ctx derives from the session-level baseCtx with its own timeout**, deliberately ignoring the parent Turn's 60s tool timeout (the same technique as `Runner.Run`'s execCtx derivation); the user's Ctrl+C propagates through baseCtx to end the wait.
- **A timeout is not an error**: it returns a status snapshot of the still-running tasks, letting the LLM decide whether to keep waiting or do something else first:

```
[general-purpose still running task-explorer-2] (running)
[general-purpose done task-researcher-3]
Research conclusion: …
Wait timed out; the tasks above are still running. You can task_wait again, or handle other matters first.
```

- Terminal states render three ways: **done / failed / cancelled** — an agent-cancelled task (re-run it) and a failed one (read the cause and investigate) demand different follow-ups and must not be conflated. Finished results are likewise `MarkInjected` (exactly-once injection).

#### task_control — Control

| Parameter | Type | Required | Description |
|------|------|:----:|------|
| `task_id` | string | Yes | The target task id |
| `action` | string (enum) | Yes | `pause` / `resume` / `cancel` / `steer` |
| `message` | string | No | The steer instruction content (required for `steer`); the reason for `cancel` |

State-machine errors (operating on a terminal task, `steer` without a message, etc.) are **returned as normal text results rather than Go errors** — the LLM can read the reason and adjust, e.g.:

```
Operation failed: task task-explorer-2 has ended (done); cannot steer
```

Success outputs (one confirmation line per action):

```
task-explorer-2 paused (halts after the current turn; resume to continue)
task-explorer-2 steering instruction injected; takes effect at the Sub-Agent's next turn
```

### The Auto-Wake Loop

When a background task finishes, its result does not sit quietly in the TaskTracker waiting for the user's next message — the TUI builds an async loop of "notify → harvest → idle with budget → synthesized dispatch":

```
tracker.Finish(id, ...)
    │ triggers the SetNotify callback
    ▼
tea.Program.Send(subAgentNotifyMsg)          # instant: result shown in the conversation (user sees it immediately)
    ▼
handleSubAgentNotify()
    ├─ harvestSubAgentResults()               # DrainCompleted: display once + write to the pendingSubAgentInject buffer
    └─ maybeAutoWake()                        # auto-wake decision
         │ conditions: enabled && main agent idle (not running) && not compacting && buffer non-empty && budget > 0
         ├─ budget -1
         ├─ take the injection buffer (prevents double-prefix injection from dispatch's fallback harvest)
         ├─ append "⟳ background Sub-Agent task finished, auto-waking the main agent" to the conversation
         └─ dispatch("[System auto-wake] The following background Sub-Agent tasks have ended; process their results and keep driving the overall task; …\n\n{result blocks}")
```

Budget and switches:

| Item | Value | Description |
|----|----|------|
| Session-level budget | `autoWakeBudgetMax = 10` | Prevents a chain of completing background tasks from sending the main agent into a runaway loop |
| Budget reset | Any real user input | The reset lives in the Enter-submit branch (plain prompts, `/` commands, `@mentions`, and Shell mode all pass through it); it cannot live inside dispatch — auto-wake itself goes through dispatch and would refill its own budget |
| Exhaustion | Prompt once, then stand down | "⚠ auto-wake budget exhausted; background results will be injected when you next send a message"; `exhausted` resets with the budget per cycle (user input opens a new cycle) |
| Compaction window skip | Skip while `compacting` | During a `/compact` (the LLM summarization takes seconds), a waking dispatch's history persistence would race and mutually erase with Compact's Clear+AddMessages write-back; results stay in the injection buffer and are consumed by the first dispatch after compaction |
| Off switch | `HARNESS9_AUTOWAKE=false` | Enabled by default |

### Background Task Lifecycle Sequence

```
Main agent LLM                  TaskTool                     Runner / sub-engine                TaskTracker
    │ task(background=true)       │                              │                            │
    ├────────────────────────────►│ Start(def, desc, prompt) ────┼───────────────────────────►│ register Running, returns id
    │                             │ NewTaskController(sink)      │                            │
    │                             │ Attach(id, ctl) ─────────────┼───────────────────────────►│ control plane attached
    │ <task id state="running"/>  │ go func(){ Runner.Run(bgCtx, def, prompt, true, ctl) }    │
    │ (returns immediately;       │                              │ Sandbox + PlanStore + isolated registry
    │  the Turn goes on)          │                              │ engine.WithTaskGate(ctl)    │
    │                             │                              │ execCtx ← derived from baseCtx
    │                             │                              │ ctl.bindExec(cancel)       │
    │                             │                              ▼                            │
    │                             │                    ┌─ per turn: AwaitTurn (the gate)        │
    │                             │                    │    ├─ paused → block (no MaxTurns cost)│
    │                             │                    │    └─ released → drain the steer mailbox│
    │                             │                    │        → persist as user messages [main-agent steering]
    │                             │                    │  beginTurn → LLM → tools → Observation │
    │                             │                    └─ natural end / error / cancelled        │
    │                             │                              │                            │
    │ task_control(cancel) ───────┼─ Control(id,"cancel",reason) │                            │
    │                             ├─────────────────────────────►│ ctl.Cancel: cancelExec()   │
    │                             │                              │ (in-flight interrupted)    │
    │                             │                              ▼                            │
    │                             │          RunStream returns err; ctl.State()==TaskCancelled   │
    │                             │          tracker.Cancel(id, "cancelled by main agent: reason") ► Cancelled terminal
    │                             │                              │                            │
    │                             │          normal end: ctl.Finish(nil) → emit Done             │
    │                             │          tracker.Finish(id, finalText, false) ───────────►│ Done terminal + notify
    │                             │                              │                            │
    │                             │                              │            notify → subAgentNotifyMsg
    │ ◄── instant display + pendingSubAgentInject buffer + maybeAutoWake synthesized dispatch ───┤
```

Highlights: **cancel propagation** follows `cancelExec → execCtx.Done() → RunStream exits → the TaskTool goroutine recognizes TaskCancelled → tracker.Cancel books it` (terminal states cannot migrate; a late Finish never overwrites); **steer injection** goes mailbox → gate release → a user message in the Sub-Agent's history, treated like any ordinary conversation message by compaction and persistence.

### Built-In Sub-Agents and Delegation Guidance

The six built-ins cover the full spectrum of "explore / research / implement / review / plan / fallback" (`internal/subagent/builtin.go`, registered by `RegisterBuiltins`; a same-name file under `.harness9/agents/` overrides a built-in):

| Name | Tool allowlist | Positioning |
|------|-----------|------|
| `general-purpose` | Empty (inherits all tools and the model of the parent) | Fallback: general tasks mixing exploration and modification, complex reasoning, multi-step dependencies |
| `explorer` | `read_file` / `glob` / `grep` / `bash` | Read-only deep exploration: understand project structure, locate implementations; returns conclusions with `file:line` references |
| `researcher` | `web_search` / `web_fetch` / `read_file` | Web multi-source research: technology selection, API usage, best practices; returns conclusions with URL references |
| `implementer` | `read_file` / `write_file` / `edit_file` / `bash` / `glob` / `grep` | Clearly bounded implementation: auto build/test verification after changes; returns a change list and verification results |
| `reviewer` | `read_file` / `glob` / `grep` / `bash` | Read-only code review: bugs, security, concurrency; findings graded by severity (never modifies code) |
| `planner` | `read_file` / `glob` / `grep` | Read-only implementation planning: step breakdown, change surface, dependency order, risks, and verification |

Shared constraints at the system-prompt level: the `bash` tool of `explorer` / `reviewer` is read-only commands only — `explorer` is limited to `ls` / `find` / `wc` and the like with no builds or installs, while `reviewer` may run tests / static checks but never modifies anything; `planner` is not given `bash` at all — its whitelist is `read_file` / `glob` / `grep` only, ruling out modifications at the tool level; `researcher` must separate facts from speculation and cross-verify key conclusions.

Two companions nudge the main agent toward delegation: the **delegation guide** (`internal/context/builder.go`, injected into the main agent's system prompt once the task-family tools are registered) and the **delegation nudge** (`engine.WithDelegationNudge`, injected once after 3 consecutive turns of "exploration without progress", at most twice per interaction) steer the main agent toward delegating bulk exploration to `explorer` to protect the main context.

### TUI Panel Control

The background task panel (`Ctrl+T` or `/tasks`) supports four control keys in the list view, routed through the same `tracker.Control` entry as the `task_control` tool:

| Key | Action | Description |
|------|------|------|
| `p` | Pause | Halts after the current turn (the panel state instantly turns yellow "paused") |
| `r` | Resume | Continues from the pause |
| `x` | Cancel | **Two-stage confirm against misfires**: the first press arms it (a "⚠ press x again to confirm cancel" hint appears at the bottom), the second executes; any other key in between (including `↑↓` movement) disarms it |
| `s` | Steer | Expands a one-line input at the bottom of the panel; `Enter` submits (injects the steering instruction, effective at the Sub-Agent's next turn), `Esc` cancels, other keys go to the input box |

Feedback is instant in the conversation area (e.g. "⏸ task-explorer-2 paused", "↪ steering instruction injected"); the panel re-reads the `TaskTracker` snapshot every frame — pause/resume are synchronously booked on success, so the state color lights up immediately.

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
    tools.NewGlobTool(workDir),
    tools.NewGrepTool(workDir),
    skills.NewUseSkillTool(skillsIndex),
}

// 2. Definition registry: register the six built-ins first, then load file-based definitions (files override same-name built-ins)
subAgentReg := subagent.NewRegistry()
if err := subagent.RegisterBuiltins(subAgentReg); err != nil { /* … */ }
subAgentReg.LoadFromDir(filepath.Join(workDir, ".harness9", "agents"))

// 3. Runner: hold a single global instance, read-only at runtime
subAgentTracker := subagent.NewTaskTracker()
subAgentRunner := subagent.NewRunner(subagent.RunnerConfig{
    BaseTools:       subAgentBaseTools,
    SharedHooks:     []hooks.ToolHook{dangerHook, offloadHook},
    SettingsPath:    settingsPath,
    SkillsIndex:     skillsIndex,
    WorkDir:         workDir,
    DefaultMaxTurns: agentMaxTurns, // = main agent's 500, Sub-Agent matches the main agent
    ToolTimeout:     60 * time.Second,
    ProviderFor:     func(model string) (provider.LLMProvider, int, error) { ... },
    CompactorFor:    func(p provider.LLMProvider, ctxWin int) memory.Compactor { ... },
    BaseCtx:         ctx,
})

// 4. Register the task tool + the three coordination tools into the parent agent's registry
//    (task_wait's baseCtx uses the session-level ctx — bypassing the parent Turn's 60s tool
//     timeout, while Ctrl+C still propagates)
taskTool := subagent.NewTaskTool(subAgentReg, subAgentRunner, subAgentTracker)
registry.Register(taskTool)
for _, ct := range []tools.BaseTool{
    subagent.NewTaskStatusTool(subAgentTracker),
    subagent.NewTaskWaitTool(subAgentTracker, ctx),
    subagent.NewTaskControlTool(subAgentTracker),
} {
    registry.Register(ct)
}
```

---

## File Index

| File | Responsibility |
|------|------|
| `internal/subagent/definition.go` | `SubAgentDefinition` struct, `Validate`, `ResolveTools` (with `alwaysDeniedTools`: the task family of four tools permanently stripped) |
| `internal/subagent/registry.go` | `Registry`: `Register` / `Get` / `List` |
| `internal/subagent/frontmatter.go` | `parseAgentFile`: YAML frontmatter parsing |
| `internal/subagent/loader.go` | `Registry.LoadFromDir`: file-based definition loading |
| `internal/subagent/builtin.go` | `RegisterBuiltins`: the six built-in Sub-Agent definitions |
| `internal/subagent/prompt.go` | `promptBuilder`: system prompt + skills + workDir assembly |
| `internal/subagent/tracker.go` | `TaskTracker`: single source of truth for background tasks (full-process log + result injection + `Control` routing) |
| `internal/subagent/control.go` | `TaskController`: the control plane (implements `engine.TaskGate`; Pause/Resume/Cancel/Steer + the `AwaitTurn` gate) |
| `internal/subagent/runner.go` | `Runner`: builds the isolated sub-engine + executes + forwards events + `WithTaskGate` / `bindExec` wiring |
| `internal/subagent/task_tool.go` | `TaskTool`: `task` tool implementation (foreground / background; the background path creates and attaches a controller) |
| `internal/subagent/task_status.go` | `TaskStatusTool`: the `task_status` observation tool |
| `internal/subagent/task_wait.go` | `TaskWaitTool`: the `task_wait` join primitive (timeout returns a snapshot, not an error) |
| `internal/subagent/task_control.go` | `TaskControlTool`: the `task_control` tool (state-machine errors returned as text) |
| `internal/engine/task_gate.go` | `TaskGate` interface + `WithTaskGate`: the turn-boundary control gate (interface defined on the consumer side) |
| `internal/schema/subagent.go` | `SubAgentUpdate` / `SubAgentUpdateKind` type definitions (incl. Paused / Resumed / Cancelled) |
| `internal/hooks/subagent_progress.go` | `SubAgentProgressFunc`: context injection/extraction |
| `internal/engine/stream.go` | `EventSubAgent`, `EventApprovalRequired`, progress sink injection |
| `internal/context/builder.go` | `WithDelegationGuide`: delegation guide section injected into the system prompt |
| `cmd/harness9/main.go` | Complete wiring: `RegisterBuiltins`, Runner construction, task + coordination tool registration, `WithDelegationNudge` |
| `cmd/harness9/tui_update.go` | `EventSubAgent` handling, `DrainCompleted` injection, `maybeAutoWake` auto-wake, `dispatchMention` (@ foreground direct run), `handleTaskPanelKey` (task panel keys + steer input mode) |
| `cmd/harness9/tui_view.go` | `renderSubAgentProgress()` (dark-cyan progress block), `renderTaskPanel()` (panel list/detail, five-state coloring), background task count in `renderStatusBar()` |
