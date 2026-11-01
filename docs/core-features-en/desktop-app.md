# Desktop App

harness9 Desktop is the macOS desktop client of harness9 (phase 1), benchmarked against the OpenAI Codex desktop app: a session workspace, streaming conversation, human approvals, and a Plan/Diff review loop — all on top of the same Go engine. Phase 1 is macOS-only; the stack is Wails v2 (Go embedding WKWebView in-process) + React/TypeScript.

---

## File Layout

```
desktop/                            # Nested Go module (Wails desktop app)
├── go.mod                          # module github.com/harness9/desktop + replace ../
├── wails.json                      # Wails project config (product name/version/frontend build)
├── main.go                         # Entry: Wails runtime assembly + log redirection
├── app.go                          # Binding layer: appservice → frontend methods + event bridge
├── build/
│   ├── appicon.png                 # App icon (1024x1024)
│   └── bin/harness9.app            # Build artifact (ad-hoc signed)
└── frontend/                       # React + TypeScript + Vite
    └── src/
        ├── types.ts                # Type contract mirroring the Go DTOs
        ├── store.ts                # zustand store + event assembly (mirrors TUI handleEvent)
        ├── App.tsx                 # Layout skeleton and bootstrap flow
        └── components/
            ├── Sidebar.tsx         # Session list / new task / sandbox & model status
            ├── Chat.tsx            # Conversation: Markdown/Thinking/tool cards/token bar
            ├── Composer.tsx        # Input: slash completion (/skill + built-ins)
            ├── ApprovalModal.tsx   # Five-option approval dialog (queued approvals)
            ├── RightPanel.tsx      # Plan progress / Diff review tabbed panel
            ├── SettingsDialog.tsx  # Settings (model/permission mode/sandbox backend)
            └── Welcome.tsx         # Working-directory picker
internal/appservice/                # Application service layer (core, see below)
```

---

## Architecture: the Codex-style Three Layers

Codex's product line (CLI / IDE extension / macOS app) shares one Rust core, with a JSON-RPC app-server protocol on top serving multiple clients. harness9 adopts the same layering, but phase 1 picks the lighter in-process form:

```
┌─ harness9.app (Wails v2) ─────────────────────────────┐
│  React frontend (WKWebView)                            │
│   ▲ agent:event pushes          ▼ bound methods        │
│  app.go thin binding layer (no business logic)         │
└──────────────┬────────────────────────────────────────┘
               │ in-process calls (desktop module replaces main module)
┌──────────────▼────────────────────────────────────────┐
│  internal/appservice — application service layer       │
│   sharedStack (cross-session) + SessionStack (per chat)│
│   event pump · approval bridge · sessions · diff       │
└──────┬───────────────────┬────────────────────────────┘
       │                   │
  engine / tools /      internal/sandbox
  hooks / memory        (Docker + Seatbelt backends)
  (reused as-is)
```

Key decisions:

- **`internal/appservice` is the single source of assembly truth.** The assembly sequence in `cmd/harness9/main.go` (env → observability → skills → provider → sandbox → memory → ltm → tools → mcp → hooks → subagent → compactor → engine) is parameterized and extracted; the desktop app no longer duplicates main.go.
- **A nested Go module solves `internal/` visibility.** Go's internal rule is a lexical directory-tree check: `desktop/go.mod` references the main module via `replace github.com/harness9 => ../`, and the importing directory sits inside the main module's tree, so `internal/` packages are importable. The nested module is also skipped automatically by the main module's `go build ./...`, so CI and GoReleaser are untouched.
- **DTO first.** Every service-layer data structure crossing the boundary is JSON-serializable (`SessionInfoDTO`/`ApprovalData`/`HistoryItem`/`DiffResult`…). When phase 2 moves the service into a standalone JSON-RPC-over-stdio app-server process (the full Codex shape), the interface carries over without redesign.

### sharedStack vs SessionStack

| Stack | Lifetime | Contents |
|-------|----------|----------|
| sharedStack | Service (per workDir) | provider, OTEL, skills index, sandbox environment, memory.Manager, LTM Store/Precis, MCP Manager, common hooks (perm/danger/obs) |
| SessionStack | Session (rebuilt on open/create) | engine, HookRegistry, tool registry (incl. task tool), PlanStore, OffloadHook, CompactionOffloader, ProgressiveCompactor, SubAgent Runner |

Rebuilding the whole session stack is deliberate: OffloadHook / FilePlanWriter / CompactionOffloader all bind to a sessionID, so per-session rebuilds keep offload files and compaction records attributed correctly (also fixing the TUI issue where switching sessions kept writing offloads under the old sessionID). The cost is a few struct initializations; heavyweight resources like the provider and sandbox containers stay shared.

---

## Event Protocol

`Service.SendMessage` starts `engine.RunStream` with single-flight semantics; a background goroutine pumps events to the listener (forwarded by the desktop app as Wails `EventsEmit("agent:event", evt)`):

| Event Type | Payload | Frontend rendering |
|------------|---------|--------------------|
| `action_delta` | string | streaming Markdown reply |
| `thinking_delta` | string | dark collapsible block (auto-expanded while streaming) |
| `tool_start` | `schema.ToolCall` | tool card (running state) |
| `tool_result` | `engine.ToolResultData` | card backfilled with output/duration/error state |
| `token_update` | `engine.TokenUpdateData` | bottom token gauge (green/yellow/red tiers) |
| `compaction` | `memory.CompactionRecord` | "context compacted" notice |
| `sub_agent` | `schema.SubAgentUpdate` | sub-agent progress card (aggregated per agent) |
| `approval_required` | **`ApprovalData`** (service DTO, not the raw engine struct) | queued into the approval dialog |
| `done` / `error` | nil / string | finalize: flush stream buffers, refresh Plan & Diff |

Two concurrency decisions that differ from the TUI:

1. **Approvals never block the pump.** The engine's event channel is unbuffered; the TUI pauses reads while a dialog is open and relies on "only one tool requests approval at a time". The appservice pump registers approval requests into `pendingApprovals` and keeps consuming, so concurrent tool approvals cannot deadlock and the UI simply queues dialogs.
2. **Exactly-once, non-blocking responses.** `RespondApproval(id, ...)` delivers the decision to the engine's `ResponseCh`; if the run was already cancelled (receiver gone), the response is dropped after a 30s timeout and the goroutine exits with the Service context.

Frontend assembly rules (`store.ts`): streaming deltas accumulate into buffers; tool boundaries (tool_start / done / error) flush them into timeline entries. Opening a historical session renders directly from the `HistoryItem` timeline (tool calls and results merged into one card).

---

## Human-in-the-Loop Approvals

The approval dialog matches the TUI's five-option semantics exactly:

| Option | Behavior |
|--------|----------|
| Allow once | `ApprovalResponse{Approved: true}` |
| Always allow | `Approved: true, Remember: true` + whitelist write |
| Deny | `Approved: false` (tool returns an IsError result to the LLM) |
| Deny with feedback | `Approved: false, Feedback: "…"` (feedback returned verbatim, triggering self-healing) |

"Always allow" persists a whitelist rule — ported from the TUI's `writeApprovalToConfig`: bash builds a glob from the command's first word (e.g. `bash(*git*)`), other tools are allowed by tool name, written to `<workDir>/.harness9/settings.json`. `permission.NewFileHook` re-reads the file on every tool call, so rules take effect immediately.

The dialog header is colored by `RiskLevel` (high/medium from DangerHook, or the default ask from permission rules) and previews the arguments JSON. Multiple simultaneous approvals are queued and handled one at a time.

---

## Plan / Diff Review Loop

Benchmarked against Codex's review loop:

- **Plan panel**: the right-hand tab renders the `PlanStore` item states (pending ○ / in_progress ◐ / completed ● / cancelled ⊘) with a progress bar; refreshed automatically after `done`. Plan auto-continuation (plan_write triggered) still lives in the engine + TUI semantics; the desktop app only displays.
- **Diff panel**: `GetSessionDiff` runs `git status --porcelain` + `git diff HEAD` (--stat + patch) in the workDir; the frontend splits the patch per file into collapsible blocks with +/- line coloring and lists untracked files separately. Phase-1 semantics are "all uncommitted changes in the workDir" (no worktree task isolation yet; once phase 2 lands, diffs will be scoped precisely to the task branch).

---

## Seatbelt: the macOS Native Sandbox (new backend)

Following Codex's approach on macOS, a new `sandbox.SeatbeltEnvironment` (`internal/sandbox/seatbelt.go`) implements the existing `Environment` interface:

- Every `RunBash` spawns the child via `sandbox-exec -p <profile> bash -c <cmd>` with a **deny-by-default** profile: `(deny default)` + `(import "system.sb")` + full read + process exec/fork;
- The **workspace-write** tier adds writable roots (workDir / TMPDIR / ~/.harness9, all symlink-resolved — macOS /tmp and /var point into /private, and unresolved roots fail silently) plus network;
- The **read-only** tier denies writes and network, suited to read-only exploration;
- File tools (read_file/write_file) still use host IO + the `safePath` path sandbox, matching the Docker backend's bind-mount semantics.

Relationship to the Docker backend: Seatbelt has no container lifecycle (no Manager, no daemon dependency, instant start) and is the desktop app's first-class default; Docker offers stronger container-grade hard isolation. Switch via the settings dialog or the `SANDBOX_BACKEND` env var (`seatbelt` / `docker` / `off`). The positioning is a behavior guardrail, not an adversarial security boundary (same threat-model wording as NetworkBlockedHosts).

Desktop settings persist in `~/.harness9/desktop/settings.json` (model / permission mode / sandbox backend / recent workdirs). Effective timing: permission mode applies on the next opened session (read during session-stack rebuild); model and sandbox backend require an app restart. API keys still come from the workDir's `.env` and never enter the settings file.

---

## Session Management

| Capability | Implementation |
|------------|----------------|
| List | `memory.Manager.ListSessions` (updated_at DESC, with title/message count) |
| Create | Every launch and "＋ New Task" creates a new session (TUI semantics) |
| Resume | `OpenSession` rebuilds the session stack + returns the `HistoryItem` timeline |
| Title | `sessions` table gains a `title` column (PRAGMA probe + idempotent ALTER TABLE, lossless for old DBs); auto-derived from the first line of the first message (30 chars); rename via double-click in the sidebar |
| Delete | `DeleteSession` cascades messages/plans/offload dirs/compaction records; running sessions cannot be deleted |

---

## Build & Run

```bash
# Prerequisites: Go 1.25+, Node 18+, Xcode CLT, wails CLI
go install github.com/wailsapp/wails/v2/cmd/wails@latest

cd desktop
wails build            # Artifact: desktop/build/bin/harness9.app (arm64, ad-hoc signed)
open build/bin/harness9.app
```

Development mode (frontend hot reload): `cd desktop && wails dev`.

Runtime logs go to `~/.harness9/desktop/logs/app.log` (all stdlib log output redirected, since stderr is invisible in GUI processes).

---

## Testing

- `internal/appservice` unit tests: session lifecycle, event pump (including concurrent approvals: allow+whitelist persistence, deny+feedback round-trip), `historyToItems` merging, git diff collection, settings round-trip, permission mode conversion;
- `internal/sandbox/seatbelt_test.go`: real `sandbox-exec` smoke tests (workspace-write can write workDir / out-of-root writes denied / read-only denies writes and network / network tiers), auto-skipped off darwin;
- Existing engine tests and the eval golden dataset show zero regression (the service layer changes no agent-behavior semantics).

---

## References

| Framework | Borrowed ideas |
|-----------|----------------|
| OpenAI Codex | app-server layering (core → service → multi-client), approval-policy/sandbox-policy separation, macOS Seatbelt sandbox, diff review loop, thread title/persistence/resume |
| Claude Code | five-option approval interaction (allow / always allow / deny / deny with feedback), tool-card information density |
| Wails v2 | single-language Go desktop shell, in-process bindings, WKWebView packaging |
