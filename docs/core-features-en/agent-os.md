# Agent OS: Local Multi-Agent Operating System

## Overview

harness9 Agent OS is the M2 milestone deliverable, upgrading harness9 from a single-agent runtime to a **local-first multi-agent operating system**. It enables multiple stateless generic agents to collaborate on complex, cross-package feature development with tests and documentation in a recoverable, auditable, and isolated environment.

### Core Design

**Unified runtime + escalation router** architecture:

- **Fast Lane**: existing `engine.Run/RunStream`, zero friction for simple tasks, **no code changes**
- **Deep Lane (Mission Control)**: Coordinator decomposes -> Scheduler dispatches -> Workers execute in parallel -> Verifier validates with evidence -> Integration merges -> Mission auto-completes
- **Router**: heuristic + optional LLM triage, auto-routes simple tasks to Fast Lane, complex tasks to Deep Lane

### Relationship to Existing System

Agent OS **does not deprecate** any existing functionality. All existing modules (Run, RunStream, TUI, Skills, Sandbox, MCP, /autodev) are preserved. New Agent OS modules build on existing infrastructure:

| Existing Component | Role in Agent OS |
|---------|------------------|
| `engine.Run/RunStream` | Fast Lane, unchanged |
| `subagent.Runner` | Deep Lane Worker execution kernel |
| `sandbox.Manager` | Worker OS-level isolation |
| `internal/mission` | Mission Control Store (enhanced) |
| `internal/ltm` | Memory Plane base (enhanced) |

---

## Architecture

```
User Input (TUI/CLI/Dashboard)
     |
     v
+---------------------------------------------+
|           Router (Escalation Router)          |
|  Heuristic + optional LLM triage -> simple|complex
+----------+------------------+---------------+
           | simple            | complex
           v                   v
   +---------------+   +------------------------------+
   |  Fast Lane    |   |  Deep Lane (Mission Control)  |
   | engine.Run/   |   |  Coordinator -> Plan ->       |
   | RunStream     |   |  Scheduler -> Workers ->      |
   | (unchanged)   |   |  Integration -> Verifier      |
   +---------------+   +------------------------------+
```

### Package Structure

| Package | Responsibility |
|---------|---------------|
| `internal/mission` | Domain model + Store + CommandService |
| `internal/scheduler` | Deterministic scheduler + Dispatcher interface + crash recovery |
| `internal/worker` | WorkerAdapter + git worktree + ImplementationContract |
| `internal/verifier` | VerifierAdapter (go build/vet/test evidence) |
| `internal/integration` | IntegrationAdapter (branch merge + joint tests) |
| `internal/router` | Smart router (heuristic signals + `/mission` prefix) |
| `internal/coordinator` | Coordinator (decompose + create tasks + monitor) |
| `internal/dashboard` | Local web console (HTTP + html/template) |
| `internal/ltm` | Long-Term Memory (enhanced: 4-level scope + mission promotion) |

---

## Domain Model

### Core Objects

| Object | Responsibility |
|--------|------|
| **Mission** | User goal, frozen acceptance contract, Policy, lifecycle |
| **Plan / PlanVersion** | Task graph draft and immutable approved snapshot, versioned |
| **PlanChangeRequest** | Execution-time change request, requires human approval |
| **Task** | Input contract, dependencies, resource boundaries, acceptance |
| **TaskAttempt** | One Worker execution, associated with Lease, events, artifacts |
| **WorkspaceLease** | Task-exclusive worktree + branch + sandbox |
| **Artifact** | Worker output (commit/diff/files), SHA256-pinned |
| **Evidence** | Verification result (build/test/vet), immutable, SHA256-pinned |
| **Policy** | Per-mission concurrency, tool scope, budget, retry rules |
| **AuditEvent** | Immutable audit record (actor/action/target/reason/idempotency) |

### Task Contract

Task behavior is described by Contract, not by Agent personality:

```go
type ContractKind string  // "implementation" | "verification" | "integration"

type TaskInput struct {
    Kind         ContractKind
    Goal         string
    DependsOn    []string
    Acceptance   []string
    AllowedTools []string
    Budget       Budget
    MaxRetries   int
}
```

### State Machines

**Mission**: `draft -> planning -> ready -> running -> verifying -> succeeded/failed/needs_attention/cancelled`

**Task**: `blocked -> queued -> leased -> running -> verifying -> succeeded/failed/awaiting_input/indeterminate`

### Key Invariants

1. Coordinator can only propose; Scheduler/Control Plane dispatches
2. Workers can only submit Artifacts, **cannot mark success**
3. Only Verifier can advance final acceptance based on Evidence
4. Approved Plans are immutable (new versions only)
5. `indeterminate` requires reconciliation first; no blind retries

---

## Mission Control Plane

### Store

SQLite persistence, reuses `~/.harness9/state.db`. Idempotent schema migration with 10+ tables.

### CommandService (Sole Mutation Entry Point)

All state changes flow through `CommandService.Execute(Command)`:

- **Idempotent**: `IdempotencyKey` deduplication
- **Auditable**: immutable `AuditEvent` on every execution
- 12 CommandKinds: plan submit/approve/reject + change request + mission pause/resume/cancel + retry/escalate/exempt

### Plan Governance

```
Coordinator submit_plan_draft
    -> Plan v1 (draft, editable)
    -> user edits
    -> approve_plan -> PlanVersion v1 (immutable, schedulable)
    -> execution-time change -> request_plan_change
    -> user approve -> PlanVersion v2 (supersedes v1)
```

---

## Execution Loop

### Scheduler

Deterministic, LLM-free dispatch loop:

1. `ListSchedulableTasks`: find queued + deps satisfied + active PlanVersion
2. `ActiveTaskCounts`: check global + per-mission concurrency
3. `StartAttempt` -> `AcquireLease` -> async `Dispatch`
4. Event-driven + periodic tick

### WorkerAdapter

Each Attempt gets exclusive execution environment:

1. `CreateWorktree` + branch
2. Build `ImplementationContract` prompt
3. Call `subagent.Runner.Run` (background mode, isolated session)
4. `ParseResult` extracts `TASK_RESULT` JSON
5. `AddArtifact` records output
6. `RemoveWorktree` cleanup (finally)

### Crash Recovery

`Scheduler.Reconcile()` on startup:
- Find all `running` attempts (process gone)
- Mark as `indeterminate` (**never blindly retry**)
- GC expired leases

---

## Verification & Integration

### VerifierAdapter

Runs deterministic checks in a fresh worktree, **never verifies its own output**:

- `go build ./...` -> Evidence(build)
- `go vet ./...` -> Evidence(vet)
- `go test ./... -count=1` -> Evidence(test)

### IntegrationAdapter

Merges dependency branches in a mission-level worktree + joint tests:

1. `git merge` each dependency Task's branch
2. Conflict -> `integration_fail` Evidence
3. Joint `go test ./...` + `go vet ./...`
4. All passed -> Mission auto-complete

### Mission Auto-Complete

`TryCompleteMission`: when all Tasks succeed, Mission `running -> verifying -> succeeded`.

---

## Smart Routing

### Router

| Signal | Decision | Lane |
|--------|----------|------|
| `/mission <goal>` prefix | Force Deep | Deep Lane |
| Complexity signals (refactor/cross-package/implement+test+docs) | Suspected complex | Deep Lane |
| No signals | Simple | Fast Lane |

Non-destructive: Fast tasks can `/escalate` to Mission; Router fail-opens to Fast Lane.

### Coordinator

- **DecomposeGoal**: create Mission + draft Plan
- **CreateTaskFromPlan**: create Task records from approved Plan
- **Monitor**: observe progress, return status summary

---

## Memory Plane

Extends `internal/ltm` with 4-level scope:

| Scope | Lifecycle | Injected To |
|-------|-----------|-------------|
| Project | Cross-mission | All Agents |
| User | Cross-project | All Agents |
| Mission | During mission | Mission's Agents |
| Agent | Cross-mission | Same Agent type |

- **Mission success**: `PromoteMissionToProject` promotes memory
- **Mission failure**: `ArchiveMission` archives (no promotion)

---

## Dashboard

Local web console (`harness9 dashboard`):

- **Listen**: `127.0.0.1:7777` (local only)
- **Tech**: Go `net/http` + `html/template`, zero external frontend dependencies
- **Features**:
  - Mission list (status badges + task count)
  - Create Mission (goal form)
  - Mission detail (tasks + plan versions + audit trail + change requests)
  - Submit Plan Draft (JSON editor)
  - Add Task (title + contract kind)
  - Commands (Pause/Resume/Cancel/Approve/Reject)
  - JSON API (`GET /api/missions`)

---

## Usage

```bash
# Start Dashboard
harness9 dashboard

# Open browser
open http://127.0.0.1:7777
```

---

## M2 Completion Criteria

1. Smart routing: simple tasks Fast Lane, complex auto-decompose
2. Recoverable: restart-safe, no blind retries
3. Isolated: parallel tasks don't share writable worktrees
4. Approval-gated: unapproved changes not scheduled
5. Evidence-driven: no success without verified evidence
6. End-to-end: multi-task missions deliver code+tests+integration+evidence
7. Consistent: Dashboard and TUI show same state
8. Memory persistence: mission knowledge promoted on success
9. No regression: `go build/test/vet` + existing evals all green
10. Auditable: all changes via CommandService, AuditEvent complete
