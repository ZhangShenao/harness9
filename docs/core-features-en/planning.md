# Planning Module Implementation Principles

The Planning module in harness9 addresses a core problem: **how can an Agent think things through before acting, instead of guessing as it goes?**

A typical Agent facing a complex task easily falls into a "figure it out step by step" pattern — it has no way of knowing how much it has completed, how much remains, or what to do next. The Planning module introduces a lightweight two-phase workflow to solve this: **plan first (Plan Mode), then execute (Exec Mode)**, turning an opaque reasoning process into an observable, verifiable, resumable sequence of operations through a structured todo list (TodoStore).

---

## System Architecture

```
internal/planning/
├── mode.go       # PlanMode enum (Default / Plan / AutoEdit)
└── todo.go       # TodoStore (thread-safe) + TodoItem / TodoStatus + FormatForInjection

internal/tools/
└── todo_write.go # todo_write tool: reads/writes TodoStore + batch anti-cheat validation

internal/engine/
└── agent_loop.go # filterReadOnlyTools (tool-layer permission filtering) + Plan Mode prompt injection
                  # TodoStore load/save within runLoop (cross-session persistence)

cmd/harness9/
├── tui_update.go # execPrompt / execContinuePrompt constants
│                 # dispatch() launches the inference stream
│                 # autoExecuting resumption loop + stagnation detection in EventDone
│                 # updateTodoBlock() appends a todo snapshot to the conversation stream
└── tui_view.go   # renderTodoLines() (task list rendering with icons)
                  # renderPlanReviewDialog() (review dialog after Plan Mode completes)
                  # task progress display within renderStatusBar()
```

---

## Workflow Overview

```
Shift+Tab ──► Plan Mode activated (status bar turns amber)
User enters task description ──► dispatch(planModePrompt)
                        │
                        ▼
           engine.runLoop (filterReadOnlyTools filters out write tools)
           Agent explores the codebase, calls todo_write to output a structured plan
           Naturally stops after a brief textual summary
                        │
                        ▼
           TUI shows the review dialog (planReviewing = true)
           [1] Approve and auto-execute    [2] Approve and confirm step by step
           [3] Keep revising the plan      [4] Cancel
                        │ press 1 or 2
                        ▼
           planMode → Default, dispatch(execPrompt)
           autoExecuting = true
                        │
                        ▼
           Agent executes the checklist item by item
           ┌─ Each item: in_progress → actual tool call → completed ─┐
           │                                               │
           └────────────────────────────────────────────────┘
                        │ EventDone
                        ▼
           pending > 0 and stuck < 3 → dispatch(execContinuePrompt)
           pending == 0             → autoExecuting = false, done
           stuck ≥ 3               → stop, prompt for manual intervention
```

---

## PlanMode Enum

```go
// internal/planning/mode.go
type PlanMode int

const (
    PlanModeDefault  PlanMode = iota // 0: full tool access (default)
    PlanModePlan                     // 1: read-only planning mode
    PlanModeAutoEdit                 // 2: reserved extension slot
)
```

`PlanMode` is an integer enum; `Shift+Tab` cycles through it in the TUI, and `Next()` implements the cycle via `(m + 1) % 3`:

```
Default(0) → Plan(1) → AutoEdit(2) → Default(0) → ...
```

**Why an enum instead of a bool?** There may be more execution modes in the future (e.g. "silent mode", "sandbox mode"). Using an enum instead of an `isPlanMode bool` costs the same but scales more naturally.

`eng.SetPlanMode(mode)` protects writes with a mutex, and `runLoop` snapshots the current mode on startup, ensuring the mode stays consistent throughout the entire reasoning loop, unaffected by TUI goroutine switches:

```go
// agent_loop.go — runLoop entry
e.mu.RLock()
planMode := e.planMode   // snapshot, unchanged for the duration of the loop
e.mu.RUnlock()
```

---

## TodoStore

`TodoStore` is the core data structure of the Planning module — a thread-safe in-memory task list using **atomic replace** semantics.

### Design choice: atomic replace vs. incremental update

Most task management systems use an incremental API (add / update / delete). harness9 chooses atomic replace, for the following reasons:

1. **The LLM's natural output form is a list**: every time the LLM calls `todo_write`, it directly outputs the complete current list, rather than an incremental instruction like "change item 3's status from pending to in_progress". Atomic replace matches this output form exactly.
2. **Avoids state inconsistency**: an incremental API requires the LLM to maintain an accurate understanding of prior state, and once it slips (e.g. a typo in an ID), state diverges. Atomic replace makes every write a deterministic snapshot.
3. **Simple to implement**: `Write` only needs `copy(s.items, items)` — no merge logic, no conflict handling.

### Implementation Details

```go
// internal/planning/todo.go
type TodoStore struct {
    mu    sync.RWMutex
    items []TodoItem
}

// Write holds the write lock, atomically replaces items, and returns a copy of the replaced result.
func (s *TodoStore) Write(items []TodoItem) []TodoItem {
    s.mu.Lock()
    defer s.mu.Unlock()
    s.items = make([]TodoItem, len(items))
    copy(s.items, items)
    return s.copy()
}

// Read holds the read lock and returns a copy (the caller may safely modify it without affecting internal state).
func (s *TodoStore) Read() []TodoItem {
    s.mu.RLock()
    defer s.mu.RUnlock()
    return s.copy()
}
```

`Write` uses a double-copy strategy:
- First copy (`s.items = make(...)` + `copy`): decouples internal storage from the input `items`, preventing the caller from later mutating `items` and affecting `TodoStore`'s internal state.
- Second copy (`s.copy()`): decouples the return value from internal storage, preventing the caller from mutating the return value and affecting `TodoStore`.

Compared to a direct assignment `s.items = items`, the double copy ensures the caller, internal storage, and input argument are all fully independent, eliminating potential data race risk.

### TodoItem State Machine

```
pending ──► in_progress ──► completed
   │              │
   └──► cancelled └──► cancelled
```

The four states correspond to `TodoStatus` string constants:

```go
const (
    TodoPending    TodoStatus = "pending"
    TodoInProgress TodoStatus = "in_progress"
    TodoCompleted  TodoStatus = "completed"
    TodoCancelled  TodoStatus = "cancelled"
)
```

State transition constraints are enforced by the `todo_write` tool (not by `TodoStore` itself). `TodoStore` performs no validation on written content — it is purely a non-judging storage layer; business constraints are expressed at the tool layer.

---

## The todo_write Tool

`todo_write` is the core tool of the Planning module, through which the LLM reads and writes the task list. It is registered in the engine's tool registry, on par with tools like `read_file` and `bash`.

### Tool Definition

```go
// internal/tools/todo_write.go
func (t *TodoWriteTool) Definition() schema.ToolDefinition {
    return schema.ToolDefinition{
        Name: "todo_write",
        Description: "Maintains the current session's task list." +
            "When the todos array is provided, it performs an atomic replace; when todos is omitted, it reads the current list.\n" +
            "When a task involves 3 or more independent steps, call this tool before starting to record the task list, " +
            "and immediately update the corresponding item's status to in_progress or completed after finishing each step.",
        InputSchema: map[string]interface{}{
            "type": "object",
            "properties": map[string]interface{}{
                "todos": map[string]interface{}{
                    "type":        "array",
                    "description": "The complete task list (atomic replace). Omit this field to only read the current list.",
                    "items": map[string]interface{}{
                        "type": "object",
                        "properties": map[string]interface{}{
                            "id":      ...,
                            "content": ...,
                            "status":  {"type": "string", "enum": ["pending","in_progress","completed","cancelled"]},
                        },
                    },
                },
            },
        },
    }
}
```

The tool has two invocation modes:

| Invocation | Effect |
|---------|------|
| Pass a `todos` array | Atomic replace of the task list (write operation) |
| Omit the `todos` field | Returns the current task list JSON (read operation) |

### Anti-Cheat Validation: Batch Direct-Completion Detection

An LLM might mark a large number of `pending` tasks as `completed` in a single `todo_write` call without having done any actual work, faking progress. This is "hallucinated execution" — it looks done, but nothing was actually done.

**Original bug**: in one continuous conversation, the LLM batch-completed 9 out of 11 tasks in a single shot (2/11 → 11/11), with no corresponding file creation or bash execution operations.

**Protection strategy**: within a single `todo_write` call, at most **1** task is allowed to jump directly from a non-`in_progress` status to `completed`. More than 1 is considered batch cheating and the write is rejected.

```go
// internal/tools/todo_write.go — Execute()
var directCompletions int
for _, item := range input.Todos {
    if item.Status != planning.TodoCompleted {
        continue
    }
    prior, exists := prevStatus[item.ID]
    if !exists || prior == planning.TodoPending {
        directCompletions++ // new item, or pending → completed: counted as a direct completion
        continue
    }
    if prior == planning.TodoCancelled {
        return "", fmt.Errorf("task %q is cancelled and cannot be marked completed directly; "+
            "restore it to pending or in_progress first if it needs to be redone.", item.ID)
    }
    // in_progress / completed → completed: legal, not counted
}
if directCompletions > 1 {
    return "", fmt.Errorf(
        "not allowed to mark %d tasks as completed directly in a single call (without going through in_progress). "+
            "Please process them one at a time: update the item's status only after completing one piece of actual work.",
        directCompletions)
}
```

**Why is the threshold 1 instead of 0?**

A threshold of 0 (completely disallowing `pending → completed`) is too strict for resumption scenarios: when an Agent completes one piece of actual work during a resumed run (calling bash or write_file) and then directly marks the corresponding todo as `completed` without passing through `in_progress`, that is legitimate behavior — the Agent skipped the intermediate `in_progress` step, but the work was genuinely done. Setting the threshold to 0 would cause the Agent to repeatedly receive rejection errors, disrupting the execution flow.

A threshold of 1 retains protection against the original bug pattern (mass batch completion) while allowing the normal usage of an Agent completing a single item directly.

**Error propagation mechanism**: when `todo_write` returns an `error`, the engine wraps it as `ToolResult{IsError: true, Output: errMsg}` and injects it into the context. The LLM sees the tool call's failure message and is forced to reorganize its call arguments — this is an embodiment of harness9's "self-healing" design: don't terminate the loop, let the LLM correct itself.

---

## Tool-Layer Permission Control (filterReadOnlyTools)

In Plan Mode, `write_file` and `edit_file` are **completely removed** from the tool list, rather than being restricted by declaring "don't create files" in the prompt.

```go
// agent_loop.go
var planModeWhitelist = map[string]bool{
    "read_file":  true,
    "bash":       true,
    "use_skill":  true,
    "todo_write": true,
}

func filterReadOnlyTools(tools []schema.ToolDefinition) []schema.ToolDefinition {
    var result []schema.ToolDefinition
    for _, t := range tools {
        if planModeWhitelist[t.Name] {
            result = append(result, t)
        }
    }
    return result
}

// within runLoop
if planMode == planning.PlanModePlan {
    availableTools = filterReadOnlyTools(availableTools)
}
```

**Why control this at the tool layer rather than the prompt layer?**

A prompt is a soft constraint. The LLM may forget restrictions stated in the prompt (especially after context compaction), may be "lured" by tool usage patterns appearing in historical messages, and may in some cases deliberately choose to ignore the constraint. The tool layer is a hard constraint: once a tool is removed from the tool schema, the LLM cannot call it under any context state — it simply doesn't exist at the API layer.

`todo_write` is on the whitelist, because the core goal of Plan Mode is precisely to have the LLM output a structured plan via `todo_write`.

---

## Plan Mode Prompt Injection

Tool-layer filtering cannot express behavioral constraints like "bash may only be used for read-only commands", so `runLoop` prepends a prefix to the user prompt in Plan Mode:

```go
// agent_loop.go — runLoop
if planMode == planning.PlanModePlan {
    userPrompt = "Analyze the following request, output a directly executable implementation plan using todo_write, " +
        "then give a brief plain-text summary of the plan and stop.\n" +
        "Todo item requirements: each item must correspond to a concrete implementation action (e.g.: create a certain file, implement a certain function, run a certain command), \n" +
        "not a high-level planning description (do not write items like \"clarify requirements\" or \"design the approach\" that cannot be directly executed).\n" +
        "You may use read_file or bash (read-only commands: ls, cat, find, grep) to understand the current codebase.\n" +
        "Do not create files, run build/install, or make any actual modifications.\n\n" +
        userPrompt
}
```

Injection principle: **state behavior, not permission**. A prompt declaration like "you now have permission X" is redundant — permission is decided at the tool layer. The prompt only guides the LLM on "what to do," not "what it is able to do."

---

## Exec-Phase Prompt Design

After the user approves the plan, the TUI does not simply send "start executing"; instead it sends a carefully designed behavioral-specification prompt:

```go
// tui_update.go
const execPrompt = "Execute the todo checklist item by item. Rules:\n" +
    "1. Before starting each item, set its status to in_progress with todo_write\n" +
    "2. Use tools to do the actual work for that item — create files, write code, run commands, etc.; " +
    "merely updating the todo_write status without calling any other tool does not count as completing the item\n" +
    "3. After confirming the actual output, set its status to completed with todo_write\n" +
    "4. Do not output progress summary text; immediately move on to the next item\n" +
    "Once everything is complete, report the overall result in one sentence."
```

The intent behind rule 2 is key: **explicitly tell the LLM that "merely updating the status without calling any other tool does not count as completed"**. This is a prompt-layer countermeasure against hallucinated execution, forming a dual defense together with the tool-layer batch-completion detection.

A more concise prompt is used for resumption, avoiding repeating the full rule statement:

```go
const execContinuePrompt = "Continue processing the next pending or in_progress task item in the todo checklist. " +
    "First mark it in_progress with todo_write, then use tools to complete the actual work (write files, run commands, etc.), " +
    "and after confirming the output mark it completed, then move on to the next item. " +
    "Do not merely update the status without doing actual work, and do not output a progress summary."
```

---

## Auto-Execution Loop and Stagnation Detection

Once the `autoExecuting` flag is on, every `EventDone` event triggers the following decision logic:

```go
// tui_update.go — EventDone handler
if m.autoExecuting && m.todoStore != nil {
    items := m.todoStore.Read()
    var pending, done int
    for _, item := range items {
        switch item.Status {
        case planning.TodoPending, planning.TodoInProgress:
            pending++
        case planning.TodoCompleted:
            done++
        }
    }
    if pending > 0 {
        if done > m.autoExecPrevDone {
            m.autoExecStuck = 0  // progress made, reset stagnation count
        } else {
            m.autoExecStuck++    // no progress, stagnation count +1
        }
        if m.autoExecStuck < 3 {
            m.autoExecPrevDone = done
            return m.dispatch(execContinuePrompt)  // resume
        }
        // 3 consecutive rounds with no progress → stop
        m.autoExecuting = false
        m.lines = append(m.lines, dimStyle.Render("  ⚠ Execution stalled, please describe the next step manually"))
    } else {
        m.autoExecuting = false  // all complete
    }
}
```

**Stagnation-detection trigger condition**: the count of completed tasks (`done`) has not increased over 3 consecutive `EventDone` occurrences. This indicates the Agent is spinning idly — it is finishing its reasoning without advancing any task, possibly outputting plain text, repeatedly failing, or stuck in a loop.

Stagnation detection uses the `done` count (rather than the `pending` count) to judge progress, because only `completed` represents genuine work output. A `pending → in_progress` transition should not count as progress, since it is merely a status marker and does not represent completed work.

`dispatch()` has built-in concurrency protection:

```go
func (m tuiModel) dispatch(prompt string) (tuiModel, tea.Cmd) {
    if m.running {
        return m, nil  // an inference run is already in progress, silently ignore
    }
    // ...
}
```

---

## TUI Visual Integration

### Plan Mode Color Scheme

When Plan Mode is active, the TUI switches from the standard cyan (`#81`) to an amber color scheme, giving the user a clear visual signal: currently in the planning phase, the Agent will not modify files.

```go
// tui.go — package-level style variables
planAccentStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("220"))       // amber
planStatusBarStyle  = lipgloss.NewStyle().
    Background(lipgloss.Color("94")).
    Foreground(lipgloss.Color("220")).
    Padding(0, 1)
planModeLabelStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("208")).Bold(true)
```

The `accentStyle()` and `activeStatusBarStyle()` methods return the corresponding style based on the current `planMode`, called uniformly at the View layer, avoiding scattered `if planMode ==` checks.

### Real-Time Todo Snapshot

After every successful execution of the `todo_write` tool, the TUI appends the current task list snapshot right below the tool-completion line:

```go
// tui_update.go — EventToolResult
if m.currentTool == "todo_write" && !result.IsError && m.todoStore != nil {
    m = m.updateTodoBlock()
}

// updateTodoBlock simply appends, it does not replace in place
func (m tuiModel) updateTodoBlock() tuiModel {
    todoLines := m.renderTodoLines(m.todoStore.Read())
    if len(todoLines) == 0 {
        return m
    }
    m.lines = append(m.lines, todoLines...)
    return m
}
```

The snapshot uses status icons to visualize each item:

| Icon | Status |
|------|------|
| `✔` | completed |
| `▶` | in_progress |
| `○` | pending |
| `⊘` | cancelled |

**Why append instead of replacing in place?** Appending preserves the full conversation history — the user can scroll up to see every status change. In-place replacement would only show the final state, losing the progress trail. The tradeoff is that the conversation stream grows as the todo list updates, but the todo list is usually short, so this tradeoff is acceptable.

### Review Dialog

After Plan Mode completes, the TUI displays a rounded-border options dialog, pausing input while waiting for the user's decision:

```
╭──────────────────────────────────────────────╮
│  Plan Mode Complete — Choose Next Step         │
│                                              │
│  [1]  Approve and auto-execute                 │
│  [2]  Approve and confirm edits step by step    │
│  [3]  Keep revising the plan (stay in Plan Mode) │
│  [4]  Cancel                                   │
╰──────────────────────────────────────────────╯
```

Options 1 and 2 both set `autoExecuting` to `true` and immediately dispatch `execPrompt`; current behavior is identical, the difference lies in the `planMode` set after execution:

- Option 1: `planMode = PlanModeDefault`, full tool permissions during the execution phase
- Option 2: `planMode = PlanModeAutoEdit` (labeled "not implemented"), tool-layer behavior is the same as Default, reserved for a future step-by-step confirmation mode extension

Option 3: keeps `planMode == PlanModePlan`, allowing the user to continue asking the Agent questions or request plan adjustments.

### Status Bar Task Progress

```go
// tui_view.go — renderStatusBar
items := m.todoStore.Read()
var completed int
for _, item := range items {
    if item.Status == planning.TodoCompleted { completed++ }
}
// accent color follows the current planMode: cyan for Default, amber for Plan/AutoEdit
tasksPart = dimStyle.Render("  │  ") + accent.Render(fmt.Sprintf("%d/%d tasks", completed, len(items)))
```

Only items with `TodoCompleted` status are counted as "completed"; `in_progress` is not counted. The status bar displays something like `3/11 tasks`, reflecting real completion progress in real time. The color follows the current `planMode`'s `accentStyle()` (amber in Plan Mode, cyan in default mode).

---

## Cross-Session Todo Persistence

The contents of `TodoStore` are persisted to SQLite alongside the Session, so unfinished tasks can be recovered after a process restart or session switch.

`runLoop` restores it on startup and saves it at the end (`defer` guarantees it runs even in the event of a panic):

```go
// agent_loop.go — runLoop
// Restore TodoStore from the Session at startup
if sess != nil && todoStore != nil {
    if todos, err := sess.GetTodos(ctx); err == nil {
        todoStore.Write(todos)
    }
}

// Save at the end (defer executes on all paths)
defer func() {
    if sess != nil && todoStore != nil {
        if err := sess.SaveTodos(ctx, todoStore.Read()); err != nil {
            log.Print(...)
        }
    }
}()
```

**State continuity across runLoop invocations**: in `autoExecuting` mode, every resumption triggers a new `runLoop`. Each `runLoop` invocation restores `TodoStore` from the DB at startup, ensuring the initial state of a resumed run matches the state at the end of the previous run — this is the prerequisite for `todo_write`'s anti-cheat validation to work correctly: `pending` tasks are saved to the DB at the end of the previous run and loaded back into memory on the next run, so the validation logic can accurately recognize a task's historical state.

The TUI's `/new` and `/resume` commands trigger `todoStore.Write(nil)`, clearing the in-memory task list, so a new session starts from an empty list.

---

## Todo Injection During Context Compaction

When the conversation history grows too long and triggers `SummarizationCompactor`, old messages are compressed by the LLM into a single summary message. If unfinished todos fall within the "old messages" range being compacted, the LLM might forget them after compaction.

`TodoStore` implements the `TodoInjector` interface, appending active tasks (`pending` and `in_progress`) to the end of the summary every time a summary is generated:

```go
// internal/planning/todo.go
func (s *TodoStore) FormatForInjection() string {
    var lines []string
    for _, item := range s.items {
        if item.Status == TodoPending || item.Status == TodoInProgress {
            prefix := "[ ]"
            if item.Status == TodoInProgress { prefix = "[>]" }
            lines = append(lines, fmt.Sprintf("%s %s", prefix, item.Content))
        }
    }
    // ...
}
```

```go
// internal/memory/summarization.go — Compact()
summaryContent := summaryMarker + "\n" + summary
if c.TodoInjector != nil {
    if todoText := c.TodoInjector.FormatForInjection(); todoText != "" {
        summaryContent += "\n\n## Active Tasks\n" + todoText
    }
}
```

Format of the summary message after compaction:

```
[Conversation Summary]
**Goal:** Build a Go web application...
**Progress:** Directory structure created, go.mod initialized...
**Next Steps:** Implement route registration...

## Active Tasks
[ ] Implement handler/user.go
[ ] Add route registration
[>] Configure database connection
```

This ensures that even when a long conversation triggers multiple compactions, the Agent won't "forget" which tasks are still outstanding.

---

## Data Flow Summary

```
User presses Shift+Tab
    │
    ▼
tuiModel.planMode = PlanModePlan
eng.SetPlanMode(PlanModePlan)           # thread-safe write, read as a snapshot by runLoop

User enters a task → dispatch(userPrompt)
    │
    ▼
engine.runLoop
    ├─ snapshot planMode, todoStore
    ├─ GetTodos(ctx) → todoStore.Write(todos)    # restore task state from the DB
    ├─ inject the Plan Mode prefix prompt
    ├─ filterReadOnlyTools()                     # remove write_file/edit_file from the tool list
    └─ ReAct loop
           │ LLM calls todo_write
           ▼
       TodoWriteTool.Execute()
           ├─ read prevStatus (current store snapshot)
           ├─ compute directCompletions (batch-completion detection)
           ├─ directCompletions > 1 → error → LLM retries
           └─ store.Write(todos) → TUI EventToolResult → updateTodoBlock()
           │ LLM stops naturally (no ToolCall)
           ▼
       defer SaveTodos(ctx, store.Read())        # persist to SQLite
       EventDone → planReviewing = true
           │
           ▼
       Review dialog (user presses 1)
           │
           ▼
       planMode = Default / eng.SetPlanMode(Default)
       autoExecuting = true
       dispatch(execPrompt)
           │
           ▼
       engine.runLoop (full tool list, exec prompt)
           │ Agent executes tools, each item: in_progress → tool call → completed
           ▼
       EventDone
           ├─ pending > 0, done > prevDone → stuck=0, dispatch(execContinuePrompt)
           ├─ pending > 0, no progress    → stuck++
           │     stuck ≥ 3               → autoExecuting=false, warning
           └─ pending == 0               → autoExecuting=false, done
```
