package worker

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/harness9/internal/mission"
	"github.com/harness9/internal/subagent"
)

// newTestStore creates a fresh in-memory-backed mission.Store for tests. The
// _pragma=busy_timeout DSN parameter makes concurrent writers block-and-retry
// instead of immediately failing with SQLITE_BUSY ("database is locked"):
// Task 4's Adapter.run genuinely executes on a background goroutine
// concurrently with the test goroutine's polling reads, unlike Task 3's
// no-op run, so this package is the first in this codebase to put real
// concurrent load on a mission.Store. (A single-connection pool was also
// tried and rejected: mission.Store.ListSchedulableTasks holds an open
// *sql.Rows while issuing a nested per-row query, which self-deadlocks with
// SetMaxOpenConns(1).)
func newTestStore(t *testing.T) *mission.Store {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "mission.db") + "?_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
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

// noopExecutor never actually gets called by the two rollback tests below
// (Dispatch's synchronous half returns an error before the goroutine reaches
// the executor in either case), but is needed to construct an Adapter.
type noopExecutor struct{}

func (noopExecutor) Execute(ctx context.Context, workDir, prompt string) (subagent.SubAgentResult, error) {
	return subagent.SubAgentResult{}, nil
}

// blockingExecutor blocks in Execute until release is closed. Used only by
// TestDispatchCreatesWorktreeLeaseAndAttempt, which asserts on Dispatch's
// synchronous bookkeeping alone: now that Task 4's a.run actually calls the
// executor, a noopExecutor's empty SubAgentResult fails ParseResult and races
// the Task past running to failed before the test observes the running
// state (the background completion routinely outruns the test's own
// post-Dispatch git subprocess calls). Parking the goroutine here removes
// that race so the test observes exactly the synchronous state Dispatch
// itself is responsible for.
type blockingExecutor struct {
	release chan struct{}
}

func (b *blockingExecutor) Execute(ctx context.Context, workDir, prompt string) (subagent.SubAgentResult, error) {
	<-b.release
	return subagent.SubAgentResult{}, nil
}

func TestDispatchCreatesWorktreeLeaseAndAttempt(t *testing.T) {
	repoRoot := newTestRepo(t)
	store := newTestStore(t)
	task := newQueuedTask(t, store)
	adapter := NewAdapter(store, repoRoot, &blockingExecutor{release: make(chan struct{})}, context.Background())

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

// TestDispatchRevertsTaskToQueuedWhenStartAttemptFails exercises the rollback
// branch that runs after AcquireLease has already advanced the Task from
// queued to leased but StartAttempt subsequently fails. Regression test for a
// bug where the rollback released the lease and removed the worktree but
// never called TransitionTask, permanently stranding the Task in
// mission.TaskLeased (with no active lease and no attempt row) where
// ListSchedulableTasks would never pick it up again. A near-zero lease TTL
// deterministically makes StartAttempt observe an already-expired lease.
func TestDispatchRevertsTaskToQueuedWhenStartAttemptFails(t *testing.T) {
	repoRoot := newTestRepo(t)
	store := newTestStore(t)
	task := newQueuedTask(t, store)
	adapter := &Adapter{
		store:      store,
		repoRoot:   repoRoot,
		leaseTTL:   time.Nanosecond,
		executor:   noopExecutor{},
		baseCtx:    context.Background(),
		workerName: "worker-adapter",
	}

	err := adapter.Dispatch(context.Background(), task)
	if err == nil {
		t.Fatal("Dispatch error = nil, want an error since the lease is already expired by the time StartAttempt runs")
	}

	updated, getErr := store.GetTask(context.Background(), task.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if updated.Status != mission.TaskQueued {
		t.Fatalf("task status = %s, want queued (Dispatch must revert the Task to queued when StartAttempt fails, per scheduler.Dispatcher's contract)", updated.Status)
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

// fakeExecutor simulates the implementation Task Contract by performing real
// git operations in the given workDir — writing a file and committing it (or
// not, for the failure case) — so the full Adapter pipeline (including
// captureDiff's real `git show`) is exercised end to end without a real LLM.
type fakeExecutor struct {
	succeed bool
	reason  string
}

func (f *fakeExecutor) Execute(ctx context.Context, workDir, prompt string) (subagent.SubAgentResult, error) {
	if !f.succeed {
		return subagent.SubAgentResult{
			FinalText: "TASK_RESULT: FAILED\nREASON: " + f.reason,
		}, nil
	}
	if err := os.WriteFile(filepath.Join(workDir, "output.txt"), []byte("done\n"), 0o644); err != nil {
		return subagent.SubAgentResult{}, err
	}
	runGitInDir(workDir, "add", "-A")
	runGitInDir(workDir, "commit", "-q", "-m", "feat: fake implementation")
	sha := runGitInDir(workDir, "rev-parse", "HEAD")
	return subagent.SubAgentResult{
		FinalText: "did the work\n\nTASK_RESULT: SUCCESS\nCOMMIT: " + sha,
	}, nil
}

// runGitInDir runs git in dir and returns trimmed stdout, panicking on error
// (test-only helper — a git failure here means the test fixture itself is
// broken, not the code under test).
func runGitInDir(dir string, args ...string) string {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		panic(fmt.Sprintf("git %v in %s: %v\n%s", args, dir, err, out))
	}
	return trimTrailingNewline(string(out))
}

// waitForTaskStatus polls the store until task reaches one of the wanted
// statuses or the timeout elapses, failing the test on timeout. Dispatch's
// completion runs in a background goroutine, so tests must poll rather than
// assert immediately after Dispatch returns.
func waitForTaskStatus(t *testing.T, store *mission.Store, taskID string, want mission.TaskStatus, timeout time.Duration) mission.Task {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		task, err := store.GetTask(context.Background(), taskID)
		if err != nil {
			t.Fatal(err)
		}
		if task.Status == want {
			return task
		}
		if time.Now().After(deadline) {
			t.Fatalf("task %s status = %s after %s, want %s", taskID, task.Status, timeout, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestDispatchSucceedsRecordsArtifactAndEvidenceAndReachesVerifying(t *testing.T) {
	repoRoot := newTestRepo(t)
	store := newTestStore(t)
	task := newQueuedTask(t, store)
	adapter := NewAdapter(store, repoRoot, &fakeExecutor{succeed: true}, context.Background())

	if err := adapter.Dispatch(context.Background(), task); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	waitForTaskStatus(t, store, task.ID, mission.TaskVerifying, time.Second)

	evidence, err := store.ListEvidence(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence) != 1 || !evidence[0].Passed {
		t.Fatalf("evidence = %+v, want exactly one passing record", evidence)
	}
}

func TestDispatchFailureRecordsEvidenceAndReachesFailed(t *testing.T) {
	repoRoot := newTestRepo(t)
	store := newTestStore(t)
	task := newQueuedTask(t, store)
	adapter := NewAdapter(store, repoRoot, &fakeExecutor{succeed: false, reason: "tests kept failing"}, context.Background())

	if err := adapter.Dispatch(context.Background(), task); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	waitForTaskStatus(t, store, task.ID, mission.TaskFailed, time.Second)

	evidence, err := store.ListEvidence(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence) != 1 || evidence[0].Passed {
		t.Fatalf("evidence = %+v, want exactly one failing record", evidence)
	}
}
