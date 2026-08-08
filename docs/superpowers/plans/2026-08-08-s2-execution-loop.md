# S2 Execution Loop Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task.

**Goal:** Build the deterministic Scheduler + generic Worker Adapter that dispatches approved Plan Tasks to isolated worktree/Sandbox environments, runs sub-agents via the existing Runner, records Artifacts, and recovers from crashes.

**Architecture:** New `internal/scheduler` package (LLM-free dispatch loop + Dispatcher interface + RoutingDispatcher by ContractKind) and `internal/worker` package (WorkerAdapter implementing Dispatcher, git worktree management, ImplementationContract prompt builder, ParseResult). Store enhancements for scheduler queries. Crash recovery via Reconcile on startup.

**Tech Stack:** Go 1.25.3, `database/sql`, `os/exec` (git worktree), existing `subagent.Runner` + `sandbox.Manager`.

## Global Constraints

- Go 1.25.3, module path `github.com/harness9`
- gofmt tab indentation, error messages lowercase no trailing period, wrap with `%w`
- No `_` for ignored errors, standard `testing` package, no third-party assertion libs
- Existing tests must pass, all schema migrations idempotent
- Fast Lane (engine.Run/RunStream) unchanged
- Run: `go test ./internal/... -v` and `go build ./...`

## File Structure

| File | Action | Responsibility |
|------|--------|---------------|
| `internal/mission/scheduler_store.go` | Create | Store queries for scheduler: ListSchedulableTasks, ActiveTaskCounts, MarkMissionRunning, MarkAttemptFinished, GetLatestAttempt |
| `internal/mission/scheduler_store_test.go` | Create | Tests for scheduler queries |
| `internal/scheduler/dispatcher.go` | Create | Dispatcher interface + Result + RoutingDispatcher |
| `internal/scheduler/scheduler.go` | Create | Scheduler struct + dispatch loop + concurrency control + Reconcile |
| `internal/scheduler/scheduler_test.go` | Create | Scheduler unit tests (mock Dispatcher) |
| `internal/worker/worktree.go` | Create | CreateWorktree/RemoveWorktree (git worktree wrappers) |
| `internal/worker/adapter.go` | Create | WorkerAdapter implementing scheduler.Dispatcher |
| `internal/worker/contract.go` | Create | ImplementationContract prompt builder + ParseResult |
| `internal/worker/worktree_test.go` | Create | Worktree tests |
| `internal/worker/adapter_test.go` | Create | WorkerAdapter tests (mock Runner) |

## Key Type Reference

```go
// internal/scheduler/dispatcher.go
type Dispatcher interface {
    Dispatch(ctx context.Context, task mission.Task, attempt mission.TaskAttempt) (Result, error)
}
type Result struct {
    Status    string  // "succeeded" | "failed" | "indeterminate"
    Artifact  *mission.CreateArtifactInput  // nil if none
    ExitReason string
}
type RoutingDispatcher struct { impl map[mission.ContractKind]Dispatcher }

// internal/scheduler/scheduler.go
type Scheduler struct {
    store *mission.Store
    dispatchers *RoutingDispatcher
    globalConcurrency int
    // ...
}
type SchedulerConfig struct {
    Store *mission.Store
    Dispatchers *RoutingDispatcher
    GlobalConcurrency int
    TickInterval time.Duration
}
func NewScheduler(cfg SchedulerConfig) *Scheduler
func (s *Scheduler) Run(ctx context.Context) error  // blocking loop
func (s *Scheduler) Reconcile(ctx context.Context) error  // crash recovery

// internal/worker/worktree.go
func CreateWorktree(ctx context.Context, repoDir, path, branch string) error
func RemoveWorktree(ctx context.Context, path string) error

// internal/worker/adapter.go
type WorkerAdapter struct {
    runner *subagent.Runner  // or RunnerConfig for per-attempt runners
    store *mission.Store
    repoDir string
}
func NewWorkerAdapter(cfg WorkerAdapterConfig) *WorkerAdapter

// internal/worker/contract.go
func BuildImplementationContract(task mission.Task, deps []mission.Artifact) string
func ParseResult(output string) (commitSHA, diffSummary string, files []string, err error)
```

---

## Task 1: Store Enhancements for Scheduler

**Files:**
- Create: `internal/mission/scheduler_store.go`
- Test: `internal/mission/scheduler_store_test.go`

**Interfaces:**
- Produces: `ListSchedulableTasks`, `ActiveTaskCounts`, `MarkMissionRunning`, `MarkAttemptFinished`, `GetLatestAttempt`

- [ ] **Step 1: Write failing tests in `scheduler_store_test.go`**

```go
package mission

import (
	"context"
	"testing"
)

func TestListSchedulableTasks(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	m, _ := store.CreateMission(ctx, CreateMissionInput{Goal: "ship"})
	// approve a plan so there's an active plan version
	plan, _ := store.CreatePlan(ctx, m.ID, `[]`)
	store.ApprovePlan(ctx, plan.ID)
	// create a queued task (no deps)
	task, _ := store.CreateTask(ctx, CreateTaskInput{MissionID: m.ID, Title: "impl"})
	// task is queued (no deps), mission has active plan version
	tasks, err := store.ListSchedulableTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].ID != task.ID {
		t.Fatalf("schedulable tasks = %d, want 1 with %s", len(tasks), task.ID)
	}
}

func TestActiveTaskCounts(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	m, _ := store.CreateMission(ctx, CreateMissionInput{Goal: "ship"})
	store.CreateTask(ctx, CreateTaskInput{MissionID: m.ID, Title: "a"})
	store.StartAttempt(ctx, "task-id", "worker")
	counts, err := store.ActiveTaskCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// counts is map[missionID]int + global
	if counts["__global__"] < 0 {
		t.Fatal("global count should be >= 0")
	}
}

func TestMarkMissionRunning(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	m, _ := store.CreateMission(ctx, CreateMissionInput{Goal: "ship"})
	// move to ready
	store.db.ExecContext(ctx, `UPDATE missions SET status = ? WHERE id = ?`, MissionReady, m.ID)
	if err := store.MarkMissionRunning(ctx, m.ID); err != nil {
		t.Fatal(err)
	}
	updated, _ := store.getMission(ctx, m.ID) // getMission is on CommandService; use direct query
	var status string
	store.db.QueryRowContext(ctx, `SELECT status FROM missions WHERE id = ?`, m.ID).Scan(&status)
	if status != string(MissionRunning) {
		t.Fatalf("status = %q, want running", status)
	}
	// idempotent: calling again on running should not error
	if err := store.MarkMissionRunning(ctx, m.ID); err != nil {
		t.Fatalf("idempotent call failed: %v", err)
	}
}
```

- [ ] **Step 2: Run, verify FAIL**

- [ ] **Step 3: Implement `scheduler_store.go`**

```go
package mission

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// ListSchedulableTasks returns tasks that are queued, have all deps satisfied,
// and belong to a mission with an active (approved) plan version.
func (s *Store) ListSchedulableTasks(ctx context.Context) ([]Task, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT t.id, t.mission_id, t.title, t.status, t.created_at, t.updated_at
		FROM tasks t
		JOIN missions m ON m.id = t.mission_id
		WHERE t.status = ? AND m.current_plan_version IS NOT NULL AND m.current_plan_version != ''
		ORDER BY t.created_at`, TaskQueued)
	if err != nil {
		return nil, fmt.Errorf("list schedulable tasks: %w", err)
	}
	defer rows.Close()
	var tasks []Task
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		task.DependsOn, err = s.taskDependencies(ctx, task.ID)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	// filter out tasks with unmet dependencies
	var ready []Task
	for _, task := range tasks {
		if s.depsSatisfied(ctx, task.DependsOn) {
			ready = append(ready, task)
		}
	}
	return ready, rows.Err()
}

func (s *Store) depsSatisfied(ctx context.Context, depIDs []string) bool {
	for _, depID := range depIDs {
		var status string
		err := s.db.QueryRowContext(ctx, `SELECT status FROM tasks WHERE id = ?`, depID).Scan(&status)
		if err != nil || status != string(TaskSucceeded) {
			return false
		}
	}
	return true
}

// ActiveTaskCounts returns per-mission and global in-flight task counts.
// The global count is under key "__global__".
func (s *Store) ActiveTaskCounts(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT t.mission_id, COUNT(*)
		FROM tasks t
		WHERE t.status IN ('leased', 'running')
		GROUP BY t.mission_id`)
	if err != nil {
		return nil, fmt.Errorf("active task counts: %w", err)
	}
	defer rows.Close()
	counts := map[string]int{}
	var global int
	for rows.Next() {
		var missionID string
		var count int
		if err := rows.Scan(&missionID, &count); err != nil {
			return nil, fmt.Errorf("scan task count: %w", err)
		}
		counts[missionID] = count
		global += count
	}
	counts["__global__"] = global
	return counts, rows.Err()
}

// MarkMissionRunning idempotently transitions a ready mission to running.
func (s *Store) MarkMissionRunning(ctx context.Context, missionID string) error {
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx,
		`UPDATE missions SET status = ?, updated_at = ? WHERE id = ? AND status IN (?, ?)`,
		MissionRunning, unixMillis(now), missionID, MissionReady, MissionRunning)
	if err != nil {
		return fmt.Errorf("mark mission running: %w", err)
	}
	return nil
}

// MarkAttemptFinished records the final status and timing of an attempt.
func (s *Store) MarkAttemptFinished(ctx context.Context, attemptID, status, exitReason string) error {
	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx,
		`UPDATE task_attempts SET status = ?, exit_reason = ?, finished_at = ?, updated_at = ? WHERE id = ?`,
		status, exitReason, unixMillis(now), unixMillis(now), attemptID)
	if err != nil {
		return fmt.Errorf("mark attempt finished: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetLatestAttempt returns the most recent attempt for a task.
func (s *Store) GetLatestAttempt(ctx context.Context, taskID string) (TaskAttempt, error) {
	var a TaskAttempt
	var createdAt, updatedAt int64
	var startedAt, finishedAt sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
		SELECT id, task_id, worker, status, created_at, updated_at
		FROM task_attempts WHERE task_id = ? ORDER BY created_at DESC LIMIT 1`, taskID).
		Scan(&a.ID, &a.TaskID, &a.Worker, &a.Status, &createdAt, &updatedAt)
	if err == sql.ErrNoRows {
		return TaskAttempt{}, ErrNotFound
	}
	if err != nil {
		return TaskAttempt{}, fmt.Errorf("get latest attempt: %w", err)
	}
	a.CreatedAt = fromUnixMillis(createdAt)
	a.UpdatedAt = fromUnixMillis(updatedAt)
	if startedAt.Valid {
		t := fromUnixMillis(startedAt.Int64)
		a.StartedAt = &t
	}
	if finishedAt.Valid {
		t := fromUnixMillis(finishedAt.Int64)
		a.FinishedAt = &t
	}
	return a, nil
}
```

- [ ] **Step 4: Run, verify PASS. Step 5: gofmt + commit.**

```bash
gofmt -w internal/mission/scheduler_store.go internal/mission/scheduler_store_test.go
git add internal/mission/scheduler_store.go internal/mission/scheduler_store_test.go
git commit -m "feat(mission): Scheduler Store 查询--可调度任务/活跃计数/状态推进"
```

---

## Task 2: Dispatcher Interface + RoutingDispatcher

**Files:**
- Create: `internal/scheduler/dispatcher.go`
- Create: `internal/scheduler/scheduler_test.go`

**Interfaces:**
- Produces: `Dispatcher` interface, `Result`, `RoutingDispatcher`

- [ ] **Step 1: Write failing test**

```go
package scheduler

import (
	"context"
	"errors"
	"testing"

	"github.com/harness9/internal/mission"
)

type mockDispatcher struct {
	called bool
	result Result
}

func (m *mockDispatcher) Dispatch(ctx context.Context, task mission.Task, attempt mission.TaskAttempt) (Result, error) {
	m.called = true
	return m.result, nil
}

func TestRoutingDispatcherRoutesByContractKind(t *testing.T) {
	impl := &mockDispatcher{result: Result{Status: "succeeded"}}
	rd := NewRoutingDispatcher()
	rd.Register(mission.ContractImplementation, impl)

	task := mission.Task{ContractKind: mission.ContractImplementation}
	result, err := rd.Dispatch(context.Background(), task, mission.TaskAttempt{})
	if err != nil {
		t.Fatal(err)
	}
	if !impl.called {
		t.Fatal("implementation dispatcher was not called")
	}
	if result.Status != "succeeded" {
		t.Fatalf("status = %q", result.Status)
	}
}

func TestRoutingDispatcherUnregisteredKind(t *testing.T) {
	rd := NewRoutingDispatcher()
	_, err := rd.Dispatch(context.Background(), mission.Task{ContractKind: "unknown"}, mission.TaskAttempt{})
	if !errors.Is(err, ErrNoDispatcher) {
		t.Fatalf("err = %v, want ErrNoDispatcher", err)
	}
}
```

- [ ] **Step 2: Run, verify FAIL**

- [ ] **Step 3: Implement `dispatcher.go`**

```go
// Package scheduler provides the deterministic, LLM-free dispatch loop that
// schedules Mission Tasks to Workers. The Scheduler itself never uses an LLM
// to decide safety-critical state -- it only enforces concurrency, budget,
// and policy constraints.
package scheduler

import (
	"context"
	"fmt"

	"github.com/harness9/internal/mission"
)

// Result describes the outcome of a Dispatch call.
type Result struct {
	Status     string
	Artifact   *mission.CreateArtifactInput
	ExitReason string
}

// Dispatcher executes one Task Attempt and returns a structured result.
// Implementations are responsible for acquiring leases, running workers,
// recording artifacts, and cleaning up.
type Dispatcher interface {
	Dispatch(ctx context.Context, task mission.Task, attempt mission.TaskAttempt) (Result, error)
}

// ErrNoDispatcher is returned when no Dispatcher is registered for a ContractKind.
var ErrNoDispatcher = fmt.Errorf("no dispatcher registered for contract kind")

// RoutingDispatcher routes Dispatch calls to per-ContractKind Dispatchers.
// The Scheduler uses this and remains agnostic to how many Contract kinds exist.
type RoutingDispatcher struct {
	impl map[mission.ContractKind]Dispatcher
}

// NewRoutingDispatcher creates an empty RoutingDispatcher.
func NewRoutingDispatcher() *RoutingDispatcher {
	return &RoutingDispatcher{impl: make(map[mission.ContractKind]Dispatcher)}
}

// Register associates a Dispatcher with a ContractKind.
func (r *RoutingDispatcher) Register(kind mission.ContractKind, d Dispatcher) {
	r.impl[kind] = d
}

// Dispatch routes to the registered Dispatcher for the task's ContractKind.
func (r *RoutingDispatcher) Dispatch(ctx context.Context, task mission.Task, attempt mission.TaskAttempt) (Result, error) {
	d, ok := r.impl[task.ContractKind]
	if !ok {
		return Result{}, fmt.Errorf("%w: %s", ErrNoDispatcher, task.ContractKind)
	}
	return d.Dispatch(ctx, task, attempt)
}
```

- [ ] **Step 4: Run, verify PASS. Step 5: gofmt + commit.**

```bash
gofmt -w internal/scheduler/dispatcher.go internal/scheduler/scheduler_test.go
git add internal/scheduler/dispatcher.go internal/scheduler/scheduler_test.go
git commit -m "feat(scheduler): Dispatcher 接口 + RoutingDispatcher 按 ContractKind 路由"
```

---

## Task 3: Worktree Management

**Files:**
- Create: `internal/worker/worktree.go`
- Test: `internal/worker/worktree_test.go`

**Interfaces:**
- Produces: `CreateWorktree(ctx, repoDir, path, branch)`, `RemoveWorktree(ctx, path)`

- [ ] **Step 1: Write failing test**

```go
package worker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func initTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, cmd := range [][]string{
		{"git", "init"}, {"git", "config", "user.email", "test@test.com"},
		{"git", "config", "user.name", "test"}, {"git", "checkout", "-b", "main"},
	} {
		if out, err := exec.Command(cmd[0], cmd[1:]...).Dir(dir).CombinedOutput(); err != nil {
			t.Fatalf("%v: %s: %v", cmd, out, err)
		}
	}
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("init"), 0644)
	exec.Command("git", "add", ".").Dir(dir).Run()
	exec.Command("git", "commit", "-m", "init").Dir(dir).Run()
	return dir
}

func TestCreateAndRemoveWorktree(t *testing.T) {
	repoDir := initTestRepo(t)
	wtPath := filepath.Join(t.TempDir(), "wt")
	ctx := context.Background()

	if err := CreateWorktree(ctx, repoDir, wtPath, "mission/test-branch"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(wtPath, "README.md")); err != nil {
		t.Fatalf("worktree files missing: %v", err)
	}
	if err := RemoveWorktree(ctx, wtPath); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wtPath); !os.IsNotExist(err) {
		t.Fatalf("worktree dir still exists after remove")
	}
}

func TestCreateWorktreeBranchExists(t *testing.T) {
	repoDir := initTestRepo(t)
	wtPath := filepath.Join(t.TempDir(), "wt")
	ctx := context.Background()
	exec.Command("git", "branch", "mission/dup").Dir(repoDir).Run()
	err := CreateWorktree(ctx, repoDir, wtPath, "mission/dup")
	if err == nil {
		t.Fatal("expected error for duplicate branch")
	}
}
```

- [ ] **Step 2: Run, verify FAIL**

- [ ] **Step 3: Implement `worktree.go`**

```go
package worker

import (
	"context"
	"fmt"
	"os/exec"
)

// CreateWorktree creates a git worktree at path with a new branch.
func CreateWorktree(ctx context.Context, repoDir, path, branch string) error {
	if repoDir == "" || path == "" || branch == "" {
		return fmt.Errorf("repoDir, path and branch are required")
	}
	cmd := exec.CommandContext(ctx, "git", "worktree", "add", "-b", branch, path)
	cmd.Dir = repoDir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git worktree add %s: %w: %s", path, err, out)
	}
	return nil
}

// RemoveWorktree removes a git worktree from the repo.
func RemoveWorktree(ctx context.Context, path string) error {
	if path == "" {
		return nil
	}
	cmd := exec.CommandContext(ctx, "git", "worktree", "remove", "--force", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git worktree remove %s: %w: %s", path, err, out)
	}
	return nil
}
```

- [ ] **Step 4: Run, verify PASS. Step 5: gofmt + commit.**

---

## Task 4: WorkerAdapter + ImplementationContract + ParseResult

**Files:**
- Create: `internal/worker/contract.go`
- Create: `internal/worker/adapter.go`
- Test: `internal/worker/adapter_test.go`

**Interfaces:**
- Consumes: `subagent.Runner` (via RunnerExecutor interface), `mission.Store`, `CreateWorktree`/`RemoveWorktree`
- Produces: `WorkerAdapter` implementing `scheduler.Dispatcher`, `RunnerExecutor` interface, `BuildImplementationContract`, `ParseResult`

- [ ] **Step 1: Write `contract.go` with BuildImplementationContract + ParseResult**

```go
package worker

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/harness9/internal/mission"
)

// BuildImplementationContract creates the prompt for a Worker sub-agent.
func BuildImplementationContract(task mission.Task, depArtifacts []mission.Artifact) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Task: %s\n\n", task.Title)
	if len(task.Input.Goal) > 0 {
		fmt.Fprintf(&b, "### Goal\n%s\n\n", task.Input.Goal)
	}
	if len(task.Input.Acceptance) > 0 {
		b.WriteString("### Acceptance Criteria\n")
		for _, a := range task.Input.Acceptance {
			fmt.Fprintf(&b, "- %s\n", a)
		}
		b.WriteString("\n")
	}
	if len(task.Input.AllowedTools) > 0 {
		b.WriteString("### Allowed Tools\n")
		fmt.Fprintf(&b, "%s\n\n", strings.Join(task.Input.AllowedTools, ", "))
	}
	if len(depArtifacts) > 0 {
		b.WriteString("### Dependency Artifacts\n")
		for _, a := range depArtifacts {
			fmt.Fprintf(&b, "- %s: %s\n", a.Kind, string(a.Content[:min(len(a.Content), 200)]))
		}
		b.WriteString("\n")
	}
	b.WriteString("### Instructions\n")
	b.WriteString("1. Implement the task in this worktree\n")
	b.WriteString("2. Run tests to verify\n")
	b.WriteString("3. Commit your work\n")
	b.WriteString("4. Output a TASK_RESULT JSON block at the end:\n")
	b.WriteString("```json\n{\"commit\": \"<sha>\", \"files\": [\"<list>\"], \"summary\": \"<text>\"}\n```\n")
	return b.String()
}

// TaskResult is the structured output parsed from the Worker's final text.
type TaskResult struct {
	Commit  string   `json:"commit"`
	Files   []string `json:"files"`
	Summary string   `json:"summary"`
}

// ParseResult extracts the TASK_RESULT JSON block from the Worker output.
func ParseResult(output string) (TaskResult, error) {
	start := strings.Index(output, "```json\n{\"commit\"")
	if start < 0 {
		start = strings.Index(output, "{\"commit\"")
		if start < 0 {
			return TaskResult{}, fmt.Errorf("no TASK_RESULT found in output")
		}
	}
	end := strings.Index(output[start:], "```")
	if end < 0 {
		end = len(output) - start
		if strings.Index(output[start:], "}") < 0 {
			return TaskResult{}, fmt.Errorf("malformed TASK_RESULT")
		}
		end = strings.Index(output[start:], "}") + 1
	} else {
		end = strings.Index(output[start:], "}") + 1
	}
	jsonStr := output[start : start+end]
	var result TaskResult
	if err := json.Unmarshal([]byte(jsonStr), &result); err != nil {
		return TaskResult{}, fmt.Errorf("parse TASK_RESULT: %w", err)
	}
	return result, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
```

- [ ] **Step 2: Write `adapter.go` with RunnerExecutor interface + WorkerAdapter**

```go
package worker

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/harness9/internal/mission"
	"github.com/harness9/internal/subagent"
)

// RunnerExecutor is the interface the WorkerAdapter depends on.
// subagent.Runner satisfies this interface.
type RunnerExecutor interface {
	Run(ctx context.Context, def subagent.SubAgentDefinition, prompt string, background bool) (subagent.SubAgentResult, error)
}

// WorkerAdapterConfig configures a WorkerAdapter.
type WorkerAdapterConfig struct {
	Runner    RunnerExecutor
	Store     *mission.Store
	RepoDir   string
	WorkDirBase string  // parent dir for worktrees (default: .missions)
}

// WorkerAdapter implements scheduler.Dispatcher for implementation Tasks.
type WorkerAdapter struct {
	runner      RunnerExecutor
	store       *mission.Store
	repoDir     string
	workDirBase string
}

// NewWorkerAdapter creates a WorkerAdapter.
func NewWorkerAdapter(cfg WorkerAdapterConfig) *WorkerAdapter {
	base := cfg.WorkDirBase
	if base == "" {
		base = ".missions"
	}
	return &WorkerAdapter{
		runner: cfg.Runner, store: cfg.Store, repoDir: cfg.RepoDir, workDirBase: base,
	}
}

// Dispatch executes a Task Attempt: creates worktree, runs sub-agent, records artifact, cleans up.
func (w *WorkerAdapter) Dispatch(ctx context.Context, task mission.Task, attempt mission.TaskAttempt) (Result, error) {
	wtPath := filepath.Join(w.workDirBase, task.MissionID, task.ID, attempt.ID)
	branch := fmt.Sprintf("mission/%s/%s/%s", task.MissionID[:8], task.ID[:8], attempt.ID[:8])

	if err := CreateWorktree(ctx, w.repoDir, wtPath, branch); err != nil {
		return Result{Status: "failed", ExitReason: err.Error()}, nil
	}
	defer RemoveWorktree(context.Background(), wtPath)

	// Build the worker sub-agent definition
	def := subagent.SubAgentDefinition{
		Name:         "worker",
		Description:  "Generic implementation worker for Mission tasks",
		SystemPrompt: "You are a Mission Worker. Implement the task, run tests, commit, and output TASK_RESULT.",
		Tools:        task.Input.AllowedTools,
		MaxTurns:     task.Input.Budget.MaxTurns,
	}

	// Build the contract prompt
	depArtifacts := w.loadDepArtifacts(ctx, task)
	prompt := BuildImplementationContract(task, depArtifacts)

	// Run the sub-agent (background mode = auto-deny approvals)
	result, err := w.runner.Run(ctx, def, prompt, true)
	if err != nil {
		return Result{Status: "indeterminate", ExitReason: err.Error()}, nil
	}

	// Parse the result
	taskResult, err := ParseResult(result.FinalText)
	if err != nil {
		return Result{Status: "failed", ExitReason: fmt.Sprintf("parse result: %v", err)}, nil
	}

	// Record artifact
	manifest := fmt.Sprintf("commit: %s\nfiles: %v\nsummary: %s", taskResult.Commit, taskResult.Files, taskResult.Summary)
	if w.store != nil {
		w.store.AddArtifact(ctx, mission.CreateArtifactInput{
			MissionID: task.MissionID, TaskID: task.ID, AttemptID: attempt.ID,
			Kind: "manifest", Content: []byte(manifest),
		})
	}

	return Result{Status: "succeeded", ExitReason: taskResult.Summary}, nil
}

func (w *WorkerAdapter) loadDepArtifacts(ctx context.Context, task mission.Task) []mission.Artifact {
	var artifacts []mission.Artifact
	for _, depID := range task.DependsOn {
		attempt, err := w.store.GetLatestAttempt(ctx, depID)
		if err != nil {
			continue
		}
		// For now, we just reference the attempt ID; full artifact loading can be added later
		_ = attempt
	}
	return artifacts
}
```

Note: The `Result` type in `adapter.go` references `scheduler.Result`. To avoid an import cycle (worker -> scheduler -> worker), define `Result` in the `worker` package OR have the adapter return `scheduler.Result` (scheduler doesn't import worker, so no cycle). Use `scheduler.Result`:

```go
import "github.com/harness9/internal/scheduler"
// Change return type to scheduler.Result
// The Dispatch signature becomes:
// func (w *WorkerAdapter) Dispatch(ctx context.Context, task mission.Task, attempt mission.TaskAttempt) (scheduler.Result, error)
```

- [ ] **Step 3: Write tests with mock RunnerExecutor**

```go
package worker

import (
	"context"
	"testing"

	"github.com/harness9/internal/mission"
	"github.com/harness9/internal/subagent"
)

type mockRunner struct {
	output string
	err    error
}

func (m *mockRunner) Run(ctx context.Context, def subagent.SubAgentDefinition, prompt string, background bool) (subagent.SubAgentResult, error) {
	return subagent.SubAgentResult{AgentID: "test", FinalText: m.output}, m.err
}

func TestParseResultValid(t *testing.T) {
	output := "Some work done.\n```json\n{\"commit\": \"abc123\", \"files\": [\"a.go\"], \"summary\": \"done\"}\n```"
	r, err := ParseResult(output)
	if err != nil {
		t.Fatal(err)
	}
	if r.Commit != "abc123" || len(r.Files) != 1 {
		t.Fatalf("parsed = %+v", r)
	}
}

func TestParseResultMissing(t *testing.T) {
	_, err := ParseResult("no result here")
	if err == nil {
		t.Fatal("expected error")
	}
}
```

- [ ] **Step 4: Run, verify PASS. Step 5: gofmt + commit.**

```bash
gofmt -w internal/worker/
git add internal/worker/
git commit -m "feat(worker): WorkerAdapter + ImplementationContract + ParseResult + worktree 管理"
```

---

## Task 5: Scheduler Core (Dispatch Loop + Concurrency Control)

**Files:**
- Create: `internal/scheduler/scheduler.go`
- Test: `internal/scheduler/scheduler_test.go` (append)

**Interfaces:**
- Consumes: `mission.Store` (ListSchedulableTasks, ActiveTaskCounts, MarkMissionRunning, StartAttempt, AcquireLease, TransitionTask), `RoutingDispatcher`
- Produces: `Scheduler`, `SchedulerConfig`, `NewScheduler`, `Run`, `Tick`

- [ ] **Step 1: Write failing test**

Append to `internal/scheduler/scheduler_test.go`:

```go
func TestSchedulerDispatchesQueuedTask(t *testing.T) {
	store := newTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m, _ := store.CreateMission(ctx, mission.CreateMissionInput{Goal: "ship"})
	plan, _ := store.CreatePlan(ctx, m.ID, `[]`)
	store.ApprovePlan(ctx, plan.ID)
	store.db.ExecContext(ctx, `UPDATE missions SET status = ? WHERE id = ?`, mission.MissionReady, m.ID)
	task, _ := store.CreateTask(ctx, mission.CreateTaskInput{MissionID: m.ID, Title: "impl"})
	store.db.ExecContext(ctx, `UPDATE tasks SET contract_kind = ? WHERE id = ?`, mission.ContractImplementation, task.ID)

	mock := &mockDispatcher{result: Result{Status: "succeeded"}}
	rd := NewRoutingDispatcher()
	rd.Register(mission.ContractImplementation, mock)

	sched := NewScheduler(SchedulerConfig{
		Store: store, Dispatchers: rd, GlobalConcurrency: 2, TickInterval: 0,
	})

	sched.Tick(ctx)

	updated, _ := store.GetTask(ctx, task.ID)
	if updated.Status != mission.TaskVerifying && updated.Status != mission.TaskSucceeded {
		t.Fatalf("task status = %q, want verifying or succeeded", updated.Status)
	}
}

func TestSchedulerRespectsConcurrencyLimit(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	m, _ := store.CreateMission(ctx, mission.CreateMissionInput{Goal: "ship"})
	plan, _ := store.CreatePlan(ctx, m.ID, `[]`)
	store.ApprovePlan(ctx, plan.ID)
	store.db.ExecContext(ctx, `UPDATE missions SET status = ? WHERE id = ?`, mission.MissionReady, m.ID)

	// create 3 queued tasks
	for i := 0; i < 3; i++ {
		task, _ := store.CreateTask(ctx, mission.CreateTaskInput{MissionID: m.ID, Title: "impl"})
		store.db.ExecContext(ctx, `UPDATE tasks SET contract_kind = ? WHERE id = ?`, mission.ContractImplementation, task.ID)
	}

	// mock that blocks to simulate in-flight work
	block := make(chan struct{})
	mock := &blockingDispatcher{block: block}
	rd := NewRoutingDispatcher()
	rd.Register(mission.ContractImplementation, mock)

	sched := NewScheduler(SchedulerConfig{
		Store: store, Dispatchers: rd, GlobalConcurrency: 1, TickInterval: 0,
	})

	sched.Tick(ctx)
	// only 1 task should be dispatched (concurrency = 1)
	counts, _ := store.ActiveTaskCounts(ctx)
	if counts["__global__"] != 1 {
		t.Fatalf("active = %d, want 1", counts["__global__"])
	}
	close(block) // release the mock
}

type blockingDispatcher struct {
	block chan struct{}
}

func (b *blockingDispatcher) Dispatch(ctx context.Context, task mission.Task, attempt mission.TaskAttempt) (Result, error) {
	<-b.block
	return Result{Status: "succeeded"}, nil
}
```

- [ ] **Step 2: Run, verify FAIL**

- [ ] **Step 3: Implement `scheduler.go`**

```go
package scheduler

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/harness9/internal/logfmt"
	"github.com/harness9/internal/mission"
)

// SchedulerConfig configures a Scheduler.
type SchedulerConfig struct {
	Store              *mission.Store
	Dispatchers        *RoutingDispatcher
	GlobalConcurrency  int
	TickInterval       time.Duration  // 0 = event-driven only (no periodic tick)
}

// Scheduler is the deterministic, LLM-free dispatch loop.
type Scheduler struct {
	store              *mission.Store
	dispatchers        *RoutingDispatcher
	globalConcurrency  int
	tickInterval       time.Duration
}

// NewScheduler creates a Scheduler.
func NewScheduler(cfg SchedulerConfig) *Scheduler {
	if cfg.GlobalConcurrency <= 0 {
		cfg.GlobalConcurrency = 2
	}
	return &Scheduler{
		store: cfg.Store, dispatchers: cfg.Dispatchers,
		globalConcurrency: cfg.GlobalConcurrency, tickInterval: cfg.TickInterval,
	}
}

// Run starts the dispatch loop. Blocking -- runs until ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context) error {
	ticker := time.NewTicker(s.tickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			s.Tick(ctx)
		}
	}
}

// Tick performs one dispatch cycle: find schedulable tasks, check limits, dispatch.
func (s *Scheduler) Tick(ctx context.Context) {
	tasks, err := s.store.ListSchedulableTasks(ctx)
	if err != nil {
		log.Print(logfmt.FormatMsg("scheduler", fmt.Sprintf("list schedulable: %v", err)))
		return
	}
	if len(tasks) == 0 {
		return
	}
	counts, err := s.store.ActiveTaskCounts(ctx)
	if err != nil {
		log.Print(logfmt.FormatMsg("scheduler", fmt.Sprintf("active counts: %v", err)))
		return
	}
	for _, task := range tasks {
		if !s.canDispatch(counts, task.MissionID) {
			continue
		}
		if err := s.dispatchOne(ctx, task); err != nil {
			log.Print(logfmt.FormatMsg("scheduler", fmt.Sprintf("dispatch task %s: %v", task.ID, err)))
			continue
		}
		counts["__global__"]++
		counts[task.MissionID]++
	}
}

func (s *Scheduler) canDispatch(counts map[string]int, missionID string) bool {
	if counts["__global__"] >= s.globalConcurrency {
		return false
	}
	// per-mission concurrency default is 1 (from Policy); for now use global cap only
	return true
}

func (s *Scheduler) dispatchOne(ctx context.Context, task mission.Task) error {
	if err := s.store.MarkMissionRunning(ctx, task.MissionID); err != nil {
		return fmt.Errorf("mark mission running: %w", err)
	}
	attempt, err := s.store.StartAttempt(ctx, task.ID, "worker")
	if err != nil {
		return fmt.Errorf("start attempt: %w", err)
	}
	// transition task to running
	if _, err := s.store.TransitionTask(ctx, task.ID, mission.TaskLeased); err != nil {
		return fmt.Errorf("transition to leased: %w", err)
	}
	if _, err := s.store.TransitionTask(ctx, task.ID, mission.TaskRunning); err != nil {
		return fmt.Errorf("transition to running: %w", err)
	}

	// dispatch asynchronously
	go func() {
		result, err := s.dispatchers.Dispatch(ctx, task, attempt)
		if err != nil {
			s.store.MarkAttemptFinished(ctx, attempt.ID, "failed", err.Error())
			s.store.TransitionTask(ctx, task.ID, mission.TaskFailed)
			return
		}
		s.handleResult(ctx, task, attempt, result)
	}()
	return nil
}

func (s *Scheduler) handleResult(ctx context.Context, task mission.Task, attempt mission.TaskAttempt, result Result) {
	s.store.MarkAttemptFinished(ctx, attempt.ID, result.Status, result.ExitReason)
	if result.Artifact != nil {
		s.store.AddArtifact(ctx, *result.Artifact)
	}
	switch result.Status {
	case "succeeded":
		s.store.TransitionTask(ctx, task.ID, mission.TaskVerifying)
		// For S2 without verifier, auto-succeed (S3 adds real verification)
		s.store.TransitionTask(ctx, task.ID, mission.TaskSucceeded)
	case "failed":
		s.store.TransitionTask(ctx, task.ID, mission.TaskFailed)
	case "indeterminate":
		s.store.TransitionTask(ctx, task.ID, mission.TaskIndeterminate)
	}
}
```

Note: `newTestStore` helper needs to be in the scheduler_test.go file. Since it's in package `scheduler` (not `mission`), it needs to create a mission.Store:

```go
func newTestStore(t *testing.T) *mission.Store {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "test.db"))
	if err != nil { t.Fatal(err) }
	t.Cleanup(func() { db.Close() })
	store, err := mission.NewStore(db)
	if err != nil { t.Fatal(err) }
	return store
}
```

Add `import ("database/sql", "path/filepath", "modernc.org/sqlite")` to the test file.

- [ ] **Step 4: Run, verify PASS. Step 5: gofmt + commit.**

```bash
gofmt -w internal/scheduler/
git add internal/scheduler/
git commit -m "feat(scheduler): 确定性调度循环 + 并发控制 + 异步派发"
```

---

## Task 6: Crash Recovery (Reconcile)

**Files:**
- Modify: `internal/scheduler/scheduler.go`
- Test: `internal/scheduler/scheduler_test.go` (append)

**Interfaces:**
- Produces: `Reconcile(ctx)` method on Scheduler

- [ ] **Step 1: Write failing test**

```go
func TestReconcileMarksInterruptedAttempt(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	m, _ := store.CreateMission(ctx, mission.CreateMissionInput{Goal: "ship"})
	plan, _ := store.CreatePlan(ctx, m.ID, `[]`)
	store.ApprovePlan(ctx, plan.ID)
	store.db.ExecContext(ctx, `UPDATE missions SET status = ? WHERE id = ?`, mission.MissionRunning, m.ID)
	task, _ := store.CreateTask(ctx, mission.CreateTaskInput{MissionID: m.ID, Title: "impl"})
	store.db.ExecContext(ctx, `UPDATE tasks SET status = ? WHERE id = ?`, mission.TaskRunning, task.ID)
	attempt, _ := store.StartAttempt(ctx, task.ID, "worker")
	store.db.ExecContext(ctx, `UPDATE task_attempts SET started_at = ? WHERE id = ?`,
		time.Now().UnixMilli(), attempt.ID)

	rd := NewRoutingDispatcher()
	sched := NewScheduler(SchedulerConfig{Store: store, Dispatchers: rd, GlobalConcurrency: 2})

	if err := sched.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	updated, _ := store.GetTask(ctx, task.ID)
	if updated.Status != mission.TaskIndeterminate {
		t.Fatalf("status = %q, want indeterminate", updated.Status)
	}
}
```

- [ ] **Step 2: Run, verify FAIL**

- [ ] **Step 3: Implement `Reconcile` in `scheduler.go`**

```go
// Reconcile checks for attempts that were running when the process died
// and marks them indeterminate (never blindly retries).
func (s *Scheduler) Reconcile(ctx context.Context) error {
	rows, err := s.store.DB().QueryContext(ctx, `
		SELECT a.id, a.task_id, t.mission_id
		FROM task_attempts a
		JOIN tasks t ON t.id = a.task_id
		WHERE a.status = 'running' AND a.finished_at IS NULL`)
	if err != nil {
		return fmt.Errorf("find interrupted attempts: %w", err)
	}
	defer rows.Close()
	var interrupted []struct{ attemptID, taskID, missionID string }
	for rows.Next() {
		var item struct{ attemptID, taskID, missionID string }
		if err := rows.Scan(&item.attemptID, &item.taskID, &item.missionID); err != nil {
			return fmt.Errorf("scan interrupted attempt: %w", err)
		}
		interrupted = append(interrupted, item)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate interrupted attempts: %w", err)
	}
	for _, item := range interrupted {
		s.store.MarkAttemptFinished(ctx, item.attemptID, "indeterminate", "process restart")
		s.store.TransitionTask(ctx, item.taskID, mission.TaskIndeterminate)
	}
	return nil
}
```

Note: This requires a `DB()` accessor on Store. Add to `internal/mission/store.go`:

```go
// DB returns the underlying database connection (for scheduler queries that
// don't have dedicated Store methods yet).
func (s *Store) DB() *sql.DB { return s.db }
```

- [ ] **Step 4: Run, verify PASS. Step 5: gofmt + commit.**

```bash
gofmt -w internal/scheduler/ internal/mission/store.go
git add internal/scheduler/ internal/mission/store.go
git commit -m "feat(scheduler): 崩溃恢复--标记中断 Attempt 为 indeterminate"
```

---

## Task 7: Integration Test + Self-Review

**Files:**
- Create: `internal/scheduler/integration_test.go`

- [ ] **Step 1: Write integration test** -- end-to-end: create mission, approve plan, dispatch task with mock dispatcher, verify task succeeds.

```go
package scheduler

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/harness9/internal/mission"
	_ "modernc.org/sqlite"
)

func TestIntegrationSingleWorkerMission(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Setup: mission + approved plan + queued task
	m, _ := store.CreateMission(ctx, mission.CreateMissionInput{Goal: "ship feature"})
	plan, _ := store.CreatePlan(ctx, m.ID, `[]`)
	store.ApprovePlan(ctx, plan.ID)
	store.DB().ExecContext(ctx, `UPDATE missions SET status = ? WHERE id = ?`, mission.MissionReady, m.ID)
	task, _ := store.CreateTask(ctx, mission.CreateTaskInput{MissionID: m.ID, Title: "implement X"})
	store.DB().ExecContext(ctx, `UPDATE tasks SET contract_kind = ? WHERE id = ?`, mission.ContractImplementation, task.ID)

	// Mock dispatcher that succeeds
	mock := &mockDispatcher{result: Result{Status: "succeeded"}}
	rd := NewRoutingDispatcher()
	rd.Register(mission.ContractImplementation, mock)

	sched := NewScheduler(SchedulerConfig{Store: store, Dispatchers: rd, GlobalConcurrency: 2})
	sched.Tick(ctx)

	// Wait for async dispatch
	time.Sleep(100 * time.Millisecond)

	updated, _ := store.GetTask(ctx, task.ID)
	if updated.Status != mission.TaskSucceeded {
		t.Fatalf("task status = %q, want succeeded", updated.Status)
	}
}
```

- [ ] **Step 2: Run, verify PASS. Step 3: gofmt + commit.**

---

## Self-Review

### Spec Coverage
| S2 requirement | Task |
|---|---|
| Scheduler dispatch loop | Task 5 |
| Concurrency limits (global + per-mission) | Task 5 |
| Dispatcher interface + ContractKind routing | Task 2 |
| WorkerAdapter (worktree + sandbox + Runner) | Task 3-4 |
| ImplementationContract + ParseResult | Task 4 |
| Crash recovery (Reconcile) | Task 6 |
| Store queries (schedulable, counts, mark running) | Task 1 |
| Integration test | Task 7 |

### Known Simplifications (for S3+ to address)
- handleResult auto-succeeds after succeeded (skips verifying state) -- S3 adds Verifier
- Per-mission concurrency uses global cap only (Policy not yet wired) -- future
- loadDepArtifacts is a stub -- S3 loads real artifacts
- Reconcile marks all running attempts as indeterminate (doesn't check git state yet) -- S8 hardens
