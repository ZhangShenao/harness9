# Mission Foundation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the first runnable, recoverable Mission Control plane for harness9 so local workers can execute dependency-aware coding tasks and submit immutable verification evidence.

**Architecture:** A focused `internal/mission` package uses the existing SQLite connection as the durable source of truth for Missions, Tasks, Attempts, Artifacts, Evidence, and worktree leases. A scheduler dispatches only dependency-ready tasks to a Worker interface; the first worker adapts the existing local sub-agent runner. The initial management surface is an explicit CLI plus a loopback-only read dashboard.

**Tech Stack:** Go 1.25.3, `database/sql`, existing `modernc.org/sqlite`, standard-library `net/http`, existing `internal/subagent`, `go test`, and `go vet`.

## Global Constraints

- Preserve existing `Run`, `RunStream`, TUI, session, skills, sandbox, MCP client, and `/autodev` behavior.
- Use no new runtime dependency and no generic workflow engine.
- Bind the dashboard only to `127.0.0.1`; do not expose unauthenticated remote writes.
- Treat worker output as untrusted: a Worker cannot mark its own Task or Mission successful.
- Give every write-capable Task an exclusive, persisted git-worktree lease.
- Record artifacts and verification output as append-only SHA-256-addressed rows.
- Follow repository Go conventions and leave unrelated `.codex/` changes unstaged.

---

## File Structure

| Path | Responsibility |
|---|---|
| `internal/mission/types.go` | IDs, records, state transitions, and errors. |
| `internal/mission/store.go` | SQLite schema plus transactional durable CRUD. |
| `internal/mission/store_test.go` | State, persistence, dependency, and integrity tests. |
| `internal/mission/lease.go` | Durable exclusive worktree lease manager. |
| `internal/mission/lease_test.go` | Lease conflict and command-seam tests. |
| `internal/mission/worker.go` | Worker interface and local sub-agent adapter. |
| `internal/mission/scheduler.go` | Dispatch and reconciliation. |
| `internal/mission/scheduler_test.go` | Fake-worker scheduling tests. |
| `internal/mission/verifier.go` | Independent command verifier. |
| `internal/mission/verifier_test.go` | Evidence and pass/fail tests. |
| `internal/mission/http.go` | Loopback dashboard and read-only JSON API. |
| `internal/mission/http_test.go` | HTTP and bind-address tests. |
| `cmd/harness9/mission.go` | Mission command line entry points. |
| `cmd/harness9/mission_test.go` | Command parsing smoke tests. |
| `cmd/harness9/main.go` | Early mission-command dispatch. |
| `skills/autodev/SKILL.md` | `/autodev` Mission entry behavior. |
| `README.md` | Quick-start and self-hosting demo. |

## Task 1: Durable Mission Store

**Files:**
- Create: `internal/mission/types.go`
- Create: `internal/mission/store.go`
- Create: `internal/mission/store_test.go`

**Interfaces:**
- Produces `NewStore(db *sql.DB) (*Store, error)`, `CreateMission`, `CreateTask`, `GetTask`, `ListTasks`, and `TransitionTask`.
- Defines `MissionStatus`, `TaskStatus`, `Mission`, `Task`, `TaskAttempt`, `Artifact`, `Evidence`, `WorkspaceLease`, and `ErrInvalidTransition`.

- [ ] **Step 1: Write the failing persistence and transition tests**

```go
func TestStoreCompletingDependencyQueuesBlockedTask(t *testing.T) {
	store := newTestStore(t)
	mission, _ := store.CreateMission(context.Background(), CreateMissionInput{Goal: "ship"})
	first, _ := store.CreateTask(context.Background(), CreateTaskInput{MissionID: mission.ID, Title: "spec"})
	second, _ := store.CreateTask(context.Background(), CreateTaskInput{MissionID: mission.ID, Title: "code", DependsOn: []string{first.ID}})
	if second.Status != TaskBlocked { t.Fatalf("status=%s", second.Status) }
	mustTransition(t, store, first.ID, TaskLeased, TaskRunning, TaskVerifying, TaskSucceeded)
	got, _ := store.GetTask(context.Background(), second.ID)
	if got.Status != TaskQueued { t.Fatalf("status=%s want queued", got.Status) }
}

func TestStoreRejectsDirectSuccess(t *testing.T) {
	store := newTestStore(t)
	task := newQueuedTask(t, store)
	if _, err := store.TransitionTask(context.Background(), task.ID, TaskSucceeded); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("err=%v", err)
	}
}
```

- [ ] **Step 2: Run the test and verify RED**

Run: `go test ./internal/mission -run 'TestStoreCompletingDependencyQueuesBlockedTask|TestStoreRejectsDirectSuccess' -count=1`

Expected: FAIL because the `mission` package does not yet exist.

- [ ] **Step 3: Write the minimal schema and implementation**

```go
const (
	TaskBlocked TaskStatus = "blocked"
	TaskQueued TaskStatus = "queued"
	TaskLeased TaskStatus = "leased"
	TaskRunning TaskStatus = "running"
	TaskVerifying TaskStatus = "verifying"
	TaskSucceeded TaskStatus = "succeeded"
	TaskFailed TaskStatus = "failed"
	TaskAwaitingInput TaskStatus = "awaiting_input"
	TaskIndeterminate TaskStatus = "indeterminate"
)

func (s *Store) TransitionTask(ctx context.Context, id string, next TaskStatus) (Task, error) {
	// Read and validate current state in one transaction, update the row,
	// then queue dependents only when every dependency has succeeded.
}
```

Create `missions`, `tasks`, `task_dependencies`, `task_attempts`, `artifacts`, `evidence`, and `workspace_leases` with foreign keys and lookup indexes. `TaskSucceeded` must only be valid from `TaskVerifying`.

- [ ] **Step 4: Verify GREEN**

Run: `go test ./internal/mission -count=1`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/mission
git commit -m "feat: add durable mission store"
```

## Task 2: Append-only Artifacts and Evidence

**Files:**
- Modify: `internal/mission/types.go`
- Modify: `internal/mission/store.go`
- Modify: `internal/mission/store_test.go`

**Interfaces:**
- Produces `StartAttempt`, `AddArtifact`, `AddEvidence`, and `ListEvidence` on `Store`.

- [ ] **Step 1: Write the failing evidence-integrity test**

```go
func TestStoreEvidenceIsContentAddressedAndAppendOnly(t *testing.T) {
	store := newTestStore(t)
	task := newQueuedTask(t, store)
	attempt, _ := store.StartAttempt(context.Background(), task.ID, "local")
	evidence, err := store.AddEvidence(context.Background(), CreateEvidenceInput{
		MissionID: task.MissionID, TaskID: task.ID, AttemptID: attempt.ID,
		Kind: "go_test", Content: []byte("ok mission"), Passed: true,
	})
	if err != nil || evidence.SHA256 == "" { t.Fatalf("evidence=%+v err=%v", evidence, err) }
	if err := store.ReplaceEvidenceForTest(context.Background(), evidence.ID, []byte("tampered")); !errors.Is(err, ErrImmutable) {
		t.Fatalf("err=%v", err)
	}
}
```

- [ ] **Step 2: Verify RED**

Run: `go test ./internal/mission -run TestStoreEvidenceIsContentAddressedAndAppendOnly -count=1`

Expected: FAIL because evidence APIs are absent.

- [ ] **Step 3: Implement minimal append-only writes**

```go
func digest(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func (s *Store) AddEvidence(ctx context.Context, in CreateEvidenceInput) (Evidence, error) {
	if len(in.Content) == 0 { return Evidence{}, fmt.Errorf("evidence content is required") }
	// Insert only. Store exposes no evidence-update method.
}
```

Persist raw output, command metadata, pass/fail, timestamps, and digest. Deduplicate identical evidence for the same attempt without ever rewriting an existing row.

- [ ] **Step 4: Verify GREEN and commit**

Run: `go test ./internal/mission -count=1`

Expected: PASS.

```bash
git add internal/mission
git commit -m "feat: record immutable mission evidence"
```

## Task 3: Exclusive Worktree Leases

**Files:**
- Create: `internal/mission/lease.go`
- Create: `internal/mission/lease_test.go`
- Modify: `internal/mission/store.go`

**Interfaces:**
- Produces `CommandRunner`, `LeaseManager`, `NewLeaseManager`, `Acquire`, and `Release`.

- [ ] **Step 1: Write the failing lease-conflict test**

```go
func TestLeaseManagerRejectsSecondActiveLease(t *testing.T) {
	store := newTestStore(t)
	task := newQueuedTask(t, store)
	leases := NewLeaseManager(store, t.TempDir(), &recordingRunner{})
	first, err := leases.Acquire(context.Background(), task.ID)
	if err != nil { t.Fatal(err) }
	defer leases.Release(context.Background(), first.ID)
	if _, err := leases.Acquire(context.Background(), task.ID); !errors.Is(err, ErrLeaseConflict) {
		t.Fatalf("err=%v", err)
	}
}
```

- [ ] **Step 2: Verify RED**

Run: `go test ./internal/mission -run TestLeaseManagerRejectsSecondActiveLease -count=1`

Expected: FAIL because `LeaseManager` is undefined.

- [ ] **Step 3: Implement minimal lease manager**

```go
type CommandRunner interface {
	Run(ctx context.Context, dir, name string, args ...string) (string, error)
}

func (m *LeaseManager) Acquire(ctx context.Context, taskID string) (WorkspaceLease, error) {
	// Atomically persist active lease before `git worktree add --detach <path> HEAD`.
	// If the command fails, mark that lease released before returning the error.
}
```

Use `os/exec` only behind `CommandRunner`; use a fake in tests. Release only the matching active lease, issuing `git worktree remove --force` through the same seam.

- [ ] **Step 4: Verify GREEN and commit**

Run: `go test ./internal/mission -run TestLeaseManager -count=1`

Expected: PASS.

```bash
git add internal/mission
git commit -m "feat: add mission workspace leases"
```

## Task 4: Scheduler and Local Worker

**Files:**
- Create: `internal/mission/worker.go`
- Create: `internal/mission/scheduler.go`
- Create: `internal/mission/scheduler_test.go`
- Create: `internal/mission/worker_test.go`

**Interfaces:**
- Produces `Worker`, `TaskContract`, `WorkerResult`, `Scheduler`, `NewScheduler`, and `RunOnce`.
- Consumes `Store` and `LeaseManager` from prior tasks.

- [ ] **Step 1: Write the failing ready-task dispatch test**

```go
func TestSchedulerRunsReadyTaskAndMovesToVerification(t *testing.T) {
	store := newTestStore(t)
	mission := newMission(t, store)
	ready := newQueuedTaskForMission(t, store, mission.ID)
	worker := &fakeWorker{result: WorkerResult{Summary: "done", Artifacts: []ArtifactInput{{Kind: "report", Content: []byte("done")}}}}
	scheduler := NewScheduler(store, newFakeLeases(t, store), []Worker{worker})
	report, err := scheduler.RunOnce(context.Background(), mission.ID)
	if err != nil || report.Dispatched != 1 || worker.calls != 1 { t.Fatalf("report=%+v calls=%d err=%v", report, worker.calls, err) }
	if got := mustTask(t, store, ready.ID); got.Status != TaskVerifying { t.Fatalf("status=%s", got.Status) }
}
```

- [ ] **Step 2: Verify RED**

Run: `go test ./internal/mission -run TestSchedulerRunsReadyTaskAndMovesToVerification -count=1`

Expected: FAIL because scheduler APIs are absent.

- [ ] **Step 3: Implement safe dispatch and adapter**

```go
type Worker interface {
	Name() string
	Run(context.Context, TaskContract) (WorkerResult, error)
}

func (s *Scheduler) RunOnce(ctx context.Context, missionID string) (DispatchReport, error) {
	// List queued tasks, acquire lease, record attempt, invoke one compatible worker.
	// A normal result writes artifacts and moves to verifying; cancellation becomes indeterminate.
}
```

Add a `LocalWorker` that depends on a small `LocalRunner` interface and adapts `subagent.Runner.Run`. It sends only contract, lease path, acceptance conditions, and authorized tool boundary in its prompt; it never treats natural-language completion as verification.

- [ ] **Step 4: Verify GREEN and commit**

Run: `go test ./internal/mission -run 'TestScheduler|TestLocalWorker' -count=1`

Expected: PASS.

```bash
git add internal/mission
git commit -m "feat: schedule local mission workers"
```

## Task 5: Independent Command Verifier

**Files:**
- Create: `internal/mission/verifier.go`
- Create: `internal/mission/verifier_test.go`
- Modify: `internal/mission/scheduler.go`

**Interfaces:**
- Produces `Verifier`, `CommandVerifier`, `NewCommandVerifier`, and `VerifyOnce`.

- [ ] **Step 1: Write the failing verifier test**

```go
func TestVerifierStoresOutputAndSucceedsVerifiedTask(t *testing.T) {
	store := newTestStore(t)
	task := newVerifyingTask(t, store, []string{"go test ./internal/mission"})
	scheduler := NewScheduler(store, newFakeLeases(t, store), nil).WithVerifier(NewCommandVerifier(fakeCommandRunner{output: "ok mission"}))
	if _, err := scheduler.VerifyOnce(context.Background(), task.MissionID); err != nil { t.Fatal(err) }
	if got := mustTask(t, store, task.ID); got.Status != TaskSucceeded { t.Fatalf("status=%s", got.Status) }
	evidence, _ := store.ListEvidence(context.Background(), task.ID)
	if len(evidence) != 1 || !evidence[0].Passed { t.Fatalf("evidence=%+v", evidence) }
}
```

- [ ] **Step 2: Verify RED**

Run: `go test ./internal/mission -run TestVerifierStoresOutputAndSucceedsVerifiedTask -count=1`

Expected: FAIL because the verifier is absent.

- [ ] **Step 3: Implement command verification**

```go
func (v *CommandVerifier) Verify(ctx context.Context, task Task, lease WorkspaceLease) (VerificationResult, error) {
	for _, command := range task.AcceptanceCommands {
		output, err := v.runner.Run(ctx, lease.Path, "bash", "-lc", command)
		// Persist evidence for every command before returning a failed result.
		if err != nil { return VerificationResult{Passed: false}, nil }
	}
	return VerificationResult{Passed: true}, nil
}
```

Pass means `verifying → succeeded`; any command failure means `verifying → failed`. Do not release evidence rows when a Task fails.

- [ ] **Step 4: Verify GREEN and commit**

Run: `go test ./internal/mission -run 'TestVerifier|TestScheduler.*Verif' -count=1`

Expected: PASS.

```bash
git add internal/mission
git commit -m "feat: verify mission work with evidence"
```

## Task 6: CLI and Loopback Dashboard

**Files:**
- Create: `internal/mission/http.go`
- Create: `internal/mission/http_test.go`
- Create: `cmd/harness9/mission.go`
- Create: `cmd/harness9/mission_test.go`
- Modify: `cmd/harness9/main.go`

**Interfaces:**
- Produces `NewHTTPHandler`, `Serve`, and `RunMissionCommand`.

- [ ] **Step 1: Write the failing API safety test**

```go
func TestDashboardListsMissionsAndRejectsWrites(t *testing.T) {
	store := newTestStore(t)
	mission := newMission(t, store)
	h := NewHTTPHandler(store)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/missions", nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), mission.ID) { t.Fatalf("%d %s", rr.Code, rr.Body.String()) }
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/missions", nil))
	if rr.Code != http.StatusMethodNotAllowed { t.Fatalf("status=%d", rr.Code) }
}
```

- [ ] **Step 2: Verify RED**

Run: `go test ./internal/mission ./cmd/harness9 -run 'TestDashboard|TestMissionCommand' -count=1`

Expected: FAIL because the HTTP handler and command do not exist.

- [ ] **Step 3: Implement explicit CLI writes and read-only browser UI**

```text
harness9 mission create --goal "add a formatter"
harness9 mission status <mission-id>
harness9 mission run <mission-id>
harness9 mission serve --addr 127.0.0.1:8765
```

Use embedded HTML/CSS and `GET /api/missions`; reject any non-loopback listen address. Mutations remain CLI-only until capability-token policy arrives in v1.2.

- [ ] **Step 4: Verify GREEN and commit**

Run: `go test ./internal/mission ./cmd/harness9 -count=1`

Expected: PASS.

```bash
git add internal/mission cmd/harness9
git commit -m "feat: add local mission dashboard"
```

## Task 7: AutoDev Entry Point and v1.1 Validation

**Files:**
- Modify: `skills/autodev/SKILL.md`
- Modify: `README.md`
- Create: `internal/mission/self_host_test.go`
- Create: `docs/agent-os/v1.1-mission-foundation-validation.md`

**Interfaces:**
- Consumes all Mission Foundation APIs from Tasks 1–6.

- [ ] **Step 1: Write the failing self-hosting flow test**

```go
func TestSelfHostingMissionCompletesOnlyAfterVerifierEvidence(t *testing.T) {
	store := newTestStore(t)
	mission := newMission(t, store)
	task := newQueuedTaskForMission(t, store, mission.ID)
	if err := runHermeticMission(t, store, task); err != nil { t.Fatal(err) }
	if got := mustTask(t, store, task.ID); got.Status != TaskSucceeded { t.Fatalf("status=%s", got.Status) }
	if evidence, _ := store.ListEvidence(context.Background(), task.ID); len(evidence) == 0 { t.Fatal("missing verifier evidence") }
}
```

- [ ] **Step 2: Verify RED**

Run: `go test ./internal/mission -run TestSelfHostingMissionCompletesOnlyAfterVerifierEvidence -count=1`

Expected: FAIL until scheduler and verifier are composed by the helper.

- [ ] **Step 3: Implement the composed flow and documentation**

Update `/autodev` so it creates a Mission, records an approved Task DAG before code edits, dispatches local workers, and requires evidence before success. Document the four CLI commands above and an end-to-end local self-hosting demo. Record actual build/test/vet/smoke evidence in the validation document.

- [ ] **Step 4: Run complete validation**

Run: `gofmt -w internal/mission/*.go cmd/harness9/mission*.go && go build ./... && go test ./... && go vet ./...`

Expected: all commands exit 0.

- [ ] **Step 5: Commit only relevant files**

```bash
git add internal/mission cmd/harness9 skills/autodev/SKILL.md README.md docs/agent-os docs/superpowers/plans/2026-07-23-mission-foundation.md
git commit -m "feat: complete mission foundation"
```

## Plan Self-Review

- **Spec coverage:** this plan implements v1.1: durable control plane, task graph, attempts, artifact/evidence records, leases, local worker, verifier, local management UI, and self-hosting entry. MCP Server/Streamable HTTP, capability tokens, and A2A federation remain intentionally scoped to v1.2/v1.3, rather than being simulated.
- **Placeholder scan:** no deferred-work marker or unspecified testing step remains.
- **Type consistency:** all later tasks consume only the `Store`, `WorkspaceLease`, `Worker`, `Scheduler`, and `Verifier` contracts defined in earlier tasks.
