# Planning Module: Implementation Principles

The Planning module of harness9 addresses one core question: **how can an agent think things through before acting, instead of guessing its way forward?**

When facing complex tasks, a typical agent falls into a "step and peek" pattern — it cannot tell how much is done, how much remains, or what comes next. The Planning module turns the plan into a **native agent capability**: on complex tasks the agent proactively drafts an execution plan with `plan_write` before acting. The plan is a session-scoped source of truth — checkpointed on every write, immune to context compaction, and fully isolated between the main agent and sub-agents.

> Design history: early versions shipped a user-toggled Plan Mode (Shift+Tab + tool whitelist + a manual approval dialog). That design has been removed — planning should not be a "mode" the user switches on, but a "capability" the agent exercises on its own. See `docs/技术调研/planning-native-redesign-v2.md` for the design rationale.

---

## Key Design Decisions

| Decision | Choice | Rationale |
|----------|--------|-----------|
| Planning trigger | System prompt guidelines; LLM decides | Forcing plans on trivial tasks wastes tokens; missed plans on complex ones are covered by guidelines plus stall detection |
| Plan scope | One plan per session | Aligns with session lifetime; natural `/resume` semantics |
| Persistence | Write-time checkpoint (persist on every successful plan_write) | Crash window shrinks from "an entire Run" to "within a single turn" |
| Compaction immunity | Engine-level view injection (Compactors untouched) | A single code path covers compaction, recovery, and mid-run visibility, and guarantees verbatim injection rather than a summarizer's paraphrase |
| Sub-agent isolation | Delegation-scoped PlanStore | Isolation by construction; no shared channel to leak through |

## System Architecture

```
internal/planning/
├── plan.go       # PlanStore (thread-safe) + PlanItem / PlanStatus + FormatPlan
└── plan_writer.go # PlanWriter interface (consumed by plan_write; avoids import cycles)

internal/tools/
└── plan_write.go # plan_write tool: reads/writes PlanStore + anti-cheat validation

internal/engine/
├── nudge.go      # appendUserNudge (shared by plan injection) + progressToolNames (stall detection)
├── loop_phases.go # beginInteraction (plan restore) / prepareTurnInput step 4c (plan injection)
│                 # checkpointPlan (write-time checkpoint) / savePlan (deferred idempotent fallback)
└── agent_loop.go # runLoop orchestrator

internal/memory/
├── session.go    # Session interface: GetPlan / SavePlan
└── sqlite_session.go # session_plans table (single-row JSON via UPSERT)

internal/subagent/
└── runner.go     # Builds an isolated PlanStore + dedicated plan_write per delegation

cmd/harness9/
├── tui_update.go # autoExecuting activation on EventToolResult + continuation loop on EventDone
└── tui_view.go   # renderPlanLines() (icon-rendered plan) + status bar progress
```

---

## Data Model

```go
// internal/planning/plan.go

type PlanStatus string // pending / in_progress / completed / cancelled

type PlanItem struct {
    ID      string     `json:"id"`      // assigned by the LLM; basis for anti-cheat checks
    Content string     `json:"content"` // one concrete, executable action
    Status  PlanStatus `json:"status"`
}
```

Legal state transitions (enforced by the plan_write tool; PlanStore itself performs no validation):

```
pending ──► in_progress ──► completed
   │              │
   └──────────────┴──► cancelled
```

### PlanStore: Full-Replace Semantics

PlanStore uses **atomic full replacement** rather than incremental updates: every plan_write call emits the complete current plan. Full replacement matches this output style exactly and avoids the consistency pitfalls of incremental APIs.

Thread safety: `sync.RWMutex` — concurrent readers, exclusive writers; double-copying decouples the caller, internal state, and return value.

Core API:

| Method | Semantics |
|--------|-----------|
| `Write(items []PlanItem) []PlanItem` | Full replacement; returns a copy |
| `Read() []PlanItem` | Snapshot copy |
| `FormatPlan() string` | Formats active items (pending/in_progress) as injection text; empty string when nothing is active |
| `ActiveCount() (active, total int)` | Drives TUI autoExecuting continuation |

---

## The plan_write Tool

plan_write is the only plan-management surface exposed to the LLM, with two call modes:

- **Write mode** (a `steps` array is present): replaces the plan wholesale after anti-cheat validation
- **Read mode** (`steps` omitted): returns the current plan as JSON without mutating state

```json
{
  "steps": [
    {"id": "1", "content": "Create parser.go for config parsing", "status": "pending"},
    {"id": "2", "content": "Implement the load logic", "status": "in_progress"}
  ]
}
```

### Anti-Cheat Validation

LLMs exhibit a "progress forgery" failure mode — marking many items completed without doing the work. The rules:

1. **Bulk shortcut rejection**: at most one pending/new item may jump straight to completed per call (`directCompletions ≤ 1`); exceeding the limit rejects the entire batch and feeds the error back to the LLM for self-healing
2. **cancelled → completed is always rejected**: a cancelled item signals abandonment; it must first return to pending/in_progress
3. **Threshold of 1, not 0**: preserves the legitimate "finished the work, recording it now" flow while blocking bulk forgery

### Markdown Plan File (PlanWriter)

A `hooks.FilePlanWriter` can be injected via `WithPlanWriter`. After every successful PlanStore write it overwrites a human-readable markdown plan file (under `workDir/.harness9/plans/` for git repositories, otherwise under the home directory). This is a **human-readable view**; write failures fail open (logged, never fatal).

---

## Native Planning: System Prompt Guidelines

Planning behavior is driven by guidelines that `DefaultPromptBuilder` injects into the system prompt (`WithPlanEnabled(true)`, only when plan_write is registered):

```
## Planning
For complex multi-step tasks (multi-file changes, dependent steps, explore-then-implement),
draft an execution plan with plan_write first, then execute step by step:
- Each item must map to a concrete action (create a file, implement a function, run a command).
  Vague items like "clarify requirements" or "design the approach" are forbidden.
- Mark an item in_progress before starting it, and completed immediately after finishing.
- The plan is the source of truth: even if the conversation context is compacted,
  the plan remains visible — continue from the plan.
- Simple tasks (1-2 steps, Q&A, single commands) need no planning — just execute.
```

The LLM decides when to plan — **no tool filtering, no prompt prefixes, no runtime detection**. This is the essential difference between a *capability* and a *mode*: the agent behaves like a senior engineer who lists a plan for complex work and just does trivial work directly.

---

## Write-Time Checkpoints

### Mechanism

```
runLoop: after executeTools
  → scan this turn's ToolCalls for a successful plan_write invocation
  → lc.sess.SavePlan(ctx, planStore.Read())   // persisted to SQLite immediately
```

```go
// internal/engine/loop_phases.go
func (lc *loopContext) checkpointPlan(calls []schema.ToolCall, results []schema.ToolResult) {
    if lc.sess == nil || lc.planStore == nil {
        return
    }
    for i, tc := range calls {
        if tc.Name == "plan_write" && i < len(results) && !results[i].IsError {
            lc.savePlan(lc.obsCtx)
            return
        }
    }
}
```

- **Crash window**: shrinks from "an entire Run" to "within a single turn" — killing the process at any moment loses at most the in-flight turn's plan progress
- **Deferred fallback**: `savePlan` is still deferred on every exit path (idempotent), retrying any checkpoint that failed mid-run
- **Fail-open**: SavePlan failures are logged and never block tool execution or the main loop

### Persistence: the session_plans Table

```sql
CREATE TABLE IF NOT EXISTS session_plans (
    session_id TEXT PRIMARY KEY,
    items      TEXT    NOT NULL DEFAULT '[]',
    updated_at INTEGER NOT NULL DEFAULT 0,
    FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE
);
```

- `SQLiteSession.SavePlan`: single-row JSON UPSERT (write-replace)
- `SQLiteSession.GetPlan`: returns `nil, nil` when no record exists
- Foreign-key cascade: deleting a session automatically removes its plan (`PRAGMA foreign_keys=ON`)
- `MemorySession` mirrors the API with an in-memory field (used by sub-agents and tests)
- The legacy `session_todos` table is not migrated: fresh installs never create it; leftover tables in existing databases are harmless

### Recovery Path

```
process crash ──► user restarts harness9 ──► /resume
  ──► beginInteraction: sess.GetPlan() → PlanStore restore (degrades to empty on failure)
  ──► first prepareTurnInput: active plan injected into the outbound view
  ──► user says "continue" ──► agent resumes from the unfinished items
```

---

## Compaction Immunity: Engine-Level Plan Injection

### Why not preserve the plan inside the Compactors?

The earlier design appended active items to the LLM summary text. Two flaws:

1. **Not verbatim**: injected content passed through summarizer/assembly logic, violating strict "inject verbatim" semantics
2. **Incomplete**: only SummarizationCompactor / ProgressiveCompactor injected; TokenBudgetCompactor / SlidingWindowCompactor (the truncation fallbacks) injected nothing — and the fallback path is exactly where plans were most likely to be lost

### Solution: inject after compaction, before the LLM call

Step 4c of `prepareTurnInput` (after the memory and stall nudges):

```go
// internal/engine/loop_phases.go
if lc.planStore != nil {
    if planText := lc.planStore.FormatPlan(); planText != "" {
        compactedHistory = appendUserNudge(compactedHistory, planText)
    }
}
```

Key properties:

- **Zero Compactor changes**: no matter which Compactor produced the view (Summarization / TokenBudget / SlidingWindow / Progressive), injection runs afterwards — the plan is always visible, including on fallback truncation paths
- **One code path, three scenarios**: post-compaction (the view is the compacted output), post-recovery (the PlanStore was restored), and mid-run (recomputed every turn, so plan_write updates are visible on the next turn)
- **View-only copy**: injection affects only the outbound view — never written to `lc.history`, never persisted, never accumulated (same mechanism as nudges) — keeping history integrity intact
- **Manual `/compact` benefits automatically**

### Injection Format (FormatPlan output)

```
## Current Execution Plan (authoritative; continue from it after compaction or recovery)
[ ] Create parser.go for config parsing
[>] Implement the load logic
```

Only pending / in_progress items appear (`[ ]` pending, `[>]` in_progress); completed items are not re-injected.

### Cost Analysis

An active plan is typically under 500 bytes, so the per-turn token overhead is negligible — traded for a hard guarantee that plans survive compaction and recovery. The redundant `TodoInjector` plumbing inside Compactors was removed: engine-level injection is the single source of truth.

---

## Sub-Agent Plan Isolation

### Design: a Delegation-Scoped PlanStore

`subagent.Runner.Run`, on every delegation:

```go
// internal/subagent/runner.go
childPlanStore := planning.NewPlanStore()
effectiveBaseTools = append(effectiveBaseTools, tools.NewPlanWriteTool(childPlanStore))
// ...
opts := []engine.Option{
    engine.WithSession(childSession),      // MemorySession (pre-existing)
    engine.WithPlanStore(childPlanStore),  // full planning semantics for the child engine
    // ...
}
```

### Three Layers of Isolation

| Layer | Guarantee |
|-------|-----------|
| Storage instance | The child engine holds only its own PlanStore reference — zero sharing with the parent; isolation by construction, no channel to leak through |
| System prompt | The sub-agent prompt (`subagent/prompt.go`) contains only planning guidelines; there is no path for parent plan data |
| Tool instance | The child registry's plan_write is bound to childPlanStore and physically cannot touch parent state |

### Lifecycle

- A sub-agent's plan dies with its `MemorySession` (discarded when the delegation ends); nothing is written to SQLite or the markdown audit files
- Sub-agents get the same native planning capability as the main agent; `Runner` appends the dedicated plan_write instance automatically — no extra registration in main.go
- **Whitelist-style custom agent definitions** (the `tools:` list in `.harness9/agents/*.md`) must list `plan_write` explicitly to plan; omitting it degrades that sub-agent naturally into a pure executor
- The anti-recursion `denyTaskHook` is unchanged: sub-agents still cannot spawn further sub-agents

---

## TUI Integration

### Plan Rendering

- **Conversation snapshot**: after a successful plan_write, an icon-rendered plan snapshot is appended below the tool line (`renderPlanLines`): `▶` in_progress (yellow) / `✔` completed (green) / `⊘` cancelled (gray) / `○` pending
- **Status bar progress**: `N/M tasks` completion ratio

### autoExecuting Continuation

Planning is fully automatic with no confirmation gate: the agent plans and then executes; the user can interrupt at any time with Ctrl+C/ESC.

- **Activation**: turns on automatically when a plan_write succeeds (`autoExecuting = true`)
- **Continuation**: when a Run ends (EventDone) with unfinished plan items, a continuation prompt is dispatched automatically
- **Stall protection**: three consecutive EventDone events without an increase in completed items count as idling; continuation stops and the user is asked to intervene
- **Cancel stops everything**: Ctrl+C/ESC immediately disables autoExecuting
- **Natural exit**: the mode ends when every item is completed

---

## Differences from the Previous Design

| Dimension | Previous (removed) | Current |
|-----------|-------------------|---------|
| Planning trigger | User toggles Plan Mode via Shift+Tab | System prompt guidelines; the LLM decides |
| Tool control | Whitelist filtering (read-only tools) under Plan Mode | Always the full tool list |
| Plan approval | "Plan complete" review dialog (4 options) | Fully automatic; interruptible at any time |
| State storage | TodoStore + session_todos table (row-per-item) | PlanStore + session_plans table (single JSON row) |
| Persistence timing | Only on runLoop exit (defer) | Write-time checkpoint + deferred idempotent fallback |
| Compaction injection | "Active Tasks" appended inside summarizers (partial coverage) | Engine-level verbatim injection every turn (all Compactors) |
| Sub-agents | No planning capability (crippling isolation) | Independent PlanStore (two-way isolation) |
| TUI | Shift+Tab / [PLAN] label / review dialog / amber theme | Removed; plan rendering and autoExecuting retained |

## Tests and Evaluation

- **Unit tests**: `planning/plan_test.go` (write/read, snapshot isolation, FormatPlan, ActiveCount, concurrency), `tools/plan_write_test.go` (anti-cheat rules, read mode, PlanWriter invocation), `engine` (checkpoint ordering, post-compaction injection with view isolation, restore injection, skip when inactive), `memory` (GetPlan/SavePlan persistence + cross-session isolation), `subagent` (child plan writes never affect the parent store)
- **Golden dataset** (24 cases): planning × 5 (plan generated / explore-then-plan / plan-then-execute / exploration only / no plan for simple tasks) + compaction × 4 (including plan_survives compaction immunity)
