package worker

import (
	"context"
	"database/sql"
	"os/exec"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/harness9/internal/mission"
	"github.com/harness9/internal/subagent"
)

// newTestStore creates a fresh in-memory-backed mission.Store for tests.
func newTestStore(t *testing.T) *mission.Store {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "mission.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store, err := mission.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// newQueuedTask creates an approved Mission with one root Task (no
// dependencies), returning it in mission.TaskQueued status, ready to Dispatch.
func newQueuedTask(t *testing.T, store *mission.Store) mission.Task {
	t.Helper()
	ctx := context.Background()
	m, err := store.CreateMission(ctx, mission.CreateMissionInput{Goal: "ship a feature"})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := store.CreateDraftPlan(ctx, m.ID, mission.PlanInput{Tasks: []mission.TaskInput{
		{ClientID: "task-a", Position: 1, Title: "Task A", Contract: "implement A and make tests pass"},
	}}, "coordinator")
	if err != nil {
		t.Fatal(err)
	}
	svc := mission.NewCommandService(store)
	if _, err := svc.ApprovePlan(ctx, mission.ApprovePlanCommand{
		MissionID: m.ID, Version: plan.Version, Actor: "user:zsa", Reason: "looks good", IdempotencyKey: "approve-1",
	}); err != nil {
		t.Fatal(err)
	}
	tasks, err := store.ListSchedulableTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range tasks {
		if task.MissionID == m.ID {
			return task
		}
	}
	t.Fatal("newQueuedTask: no schedulable task found after approving the plan")
	return mission.Task{}
}

// noopExecutor never actually gets called by this task's tests (Dispatch's
// synchronous half returns before the goroutine reaches the executor), but is
// needed to construct an Adapter.
type noopExecutor struct{}

func (noopExecutor) Execute(ctx context.Context, workDir, prompt string) (subagent.SubAgentResult, error) {
	return subagent.SubAgentResult{}, nil
}

func TestDispatchCreatesWorktreeLeaseAndAttempt(t *testing.T) {
	repoRoot := newTestRepo(t)
	store := newTestStore(t)
	task := newQueuedTask(t, store)
	adapter := NewAdapter(store, repoRoot, noopExecutor{}, context.Background())

	if err := adapter.Dispatch(context.Background(), task); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	path, branch := worktreeFor(repoRoot, task)
	if _, err := runGitErr(path, "rev-parse", "--is-inside-work-tree"); err != nil {
		t.Fatalf("worktree at %s was not created: %v", path, err)
	}
	got := runGit(t, path, "branch", "--show-current")
	if got != branch {
		t.Fatalf("branch = %q, want %q", got, branch)
	}

	updated, err := store.GetTask(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != mission.TaskRunning {
		t.Fatalf("task status = %s, want running (AcquireLease+StartAttempt should have advanced it)", updated.Status)
	}
}

func TestDispatchRollsBackWorktreeWhenLeaseAcquisitionFails(t *testing.T) {
	repoRoot := newTestRepo(t)
	store := newTestStore(t)
	task := newQueuedTask(t, store)
	// Force AcquireLease to fail: transition the task to a status from which
	// no lease can legally be acquired (queued/leased only). CreateWorktree
	// will still succeed first, since the path is fresh.
	if _, err := store.TransitionTask(context.Background(), task.ID, mission.TaskFailed); err != nil {
		t.Fatalf("pre-transition task to failed: %v", err)
	}
	adapter := NewAdapter(store, repoRoot, noopExecutor{}, context.Background())

	err := adapter.Dispatch(context.Background(), task)
	if err == nil {
		t.Fatal("Dispatch error = nil, want an error since the task cannot acquire a lease")
	}

	path, _ := worktreeFor(repoRoot, task)
	if _, statErr := runGitErr(path, "rev-parse", "--is-inside-work-tree"); statErr == nil {
		t.Fatalf("worktree at %s still exists after a failed Dispatch, want it rolled back", path)
	}
}

// runGitErr runs a git command in dir and returns an error without failing
// the test, for asserting that a path is (or isn't) a valid git worktree.
func runGitErr(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}
