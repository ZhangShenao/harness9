package verifier

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/harness9/internal/mission"
)

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

// newVerifiableTarget builds an approved Mission with one implementation Task
// ("impl") and one verification Task ("verify") depending on it, then
// simulates a completed Worker Adapter run on "impl" (a real Lease + Attempt
// + a real commit in a real worktree, transitioned to verifying) so a
// verifier.Adapter has something real to check out and re-verify.
func newVerifiableTarget(t *testing.T, store *mission.Store, repoRoot string) (target, verifyTask mission.Task) {
	t.Helper()
	ctx := context.Background()
	m, err := store.CreateMission(ctx, mission.CreateMissionInput{Goal: "ship a feature"})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := store.CreateDraftPlan(ctx, m.ID, mission.PlanInput{Tasks: []mission.TaskInput{
		{ClientID: "impl", Position: 1, Title: "Implement", Contract: "implement the thing", ContractKind: mission.ContractImplementation},
		{ClientID: "verify", Position: 2, Title: "Verify", Contract: "verify the implementation", ContractKind: mission.ContractVerification, Dependencies: []string{"impl"}},
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
	tasks, err := store.ListTasks(ctx, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range tasks {
		switch task.ClientID {
		case "impl":
			target = task
		case "verify":
			verifyTask = task
		}
	}
	if target.ID == "" || verifyTask.ID == "" {
		t.Fatal("newVerifiableTarget: expected both impl and verify tasks to exist")
	}

	implPath := filepath.Join(repoRoot, ".harness9", "missions", target.ID)
	implBranch := "mission/" + target.ID
	cmd := exec.Command("git", "worktree", "add", implPath, "-b", implBranch)
	cmd.Dir = repoRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git worktree add %s: %v\n%s", implPath, err, out)
	}
	if err := os.WriteFile(filepath.Join(implPath, "feature.go"), []byte("package main\n\n// Feature reports a fixed greeting for the fixture module.\nfunc Feature() string { return \"done\" }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, implPath, "add", "-A")
	runGit(t, implPath, "commit", "-q", "-m", "feat: implementation work")

	if _, err := store.AcquireLease(ctx, target.ID, implPath, implBranch, "", time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := store.StartAttempt(ctx, target.ID, "worker-adapter"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionTask(ctx, target.ID, mission.TaskVerifying); err != nil {
		t.Fatal(err)
	}
	verifyTask, err = store.GetTask(ctx, verifyTask.ID)
	if err != nil {
		t.Fatal(err)
	}
	return target, verifyTask
}

func TestDispatchCreatesDetachedWorktreeLeaseAndAttempt(t *testing.T) {
	repoRoot := newTestRepo(t)
	store := newTestStore(t)
	target, verifyTask := newVerifiableTarget(t, store, repoRoot)
	adapter := NewAdapter(store, repoRoot, context.Background())

	if err := adapter.Dispatch(context.Background(), verifyTask); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	path, _ := verifyWorktreeFor(repoRoot, verifyTask)
	if _, err := os.Stat(filepath.Join(path, "feature.go")); err != nil {
		t.Fatalf("verifier worktree missing the implementer's commit: %v", err)
	}

	updated, err := store.GetTask(context.Background(), verifyTask.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != mission.TaskRunning {
		t.Fatalf("verify task status = %s, want running", updated.Status)
	}

	// Dispatch's synchronous bookkeeping above is this test's actual subject,
	// but Dispatch also launches a.run on a background goroutine that now
	// performs a real go build/vet/test against the worktree (Task 5),
	// instead of Task 4's no-op. Returning before that goroutine finishes
	// would let it keep running after t.Cleanup closes this test's store's
	// DB, so wait for the verification Task it drives to reach a terminal
	// status first. newVerifiableTarget's fixture is a valid, passing Go
	// module and nothing in this test breaks it, so both the target and the
	// verifier converge on succeeded.
	waitForTaskStatus(t, store, target.ID, mission.TaskSucceeded, 30*time.Second)
	waitForTaskStatus(t, store, verifyTask.ID, mission.TaskSucceeded, time.Second)
}

func TestDispatchRejectsTaskWithoutExactlyOneDependency(t *testing.T) {
	store := newTestStore(t)
	repoRoot := newTestRepo(t)
	adapter := NewAdapter(store, repoRoot, context.Background())

	err := adapter.Dispatch(context.Background(), mission.Task{ID: "orphan-verifier", ContractKind: mission.ContractVerification})
	if err == nil {
		t.Fatal("Dispatch on a verification task with zero dependencies = nil error, want an error")
	}
}

func TestDispatchRollsBackWorktreeWhenLeaseAcquisitionFails(t *testing.T) {
	repoRoot := newTestRepo(t)
	store := newTestStore(t)
	_, verifyTask := newVerifiableTarget(t, store, repoRoot)
	// Force AcquireLease to fail: transition the verify task to a status from
	// which no lease can legally be acquired. CreateDetachedWorktree still
	// succeeds first, since the path is fresh.
	if _, err := store.TransitionTask(context.Background(), verifyTask.ID, mission.TaskFailed); err != nil {
		t.Fatalf("pre-transition verify task to failed: %v", err)
	}
	adapter := NewAdapter(store, repoRoot, context.Background())

	err := adapter.Dispatch(context.Background(), verifyTask)
	if err == nil {
		t.Fatal("Dispatch error = nil, want an error since the task cannot acquire a lease")
	}

	path, _ := verifyWorktreeFor(repoRoot, verifyTask)
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("worktree at %s still exists after a failed Dispatch, want it rolled back", path)
	}
}

// TestDispatchRequeuesTaskWhenStartAttemptFails exercises the rollback branch
// that runs after AcquireLease has already advanced the verification Task
// from queued to leased but StartAttempt subsequently fails. Regression test
// for the same class of bug internal/worker.Adapter guards against (see
// worker's TestDispatchRevertsTaskToQueuedWhenStartAttemptFails): a rollback
// that releases the lease and removes the worktree but never calls
// TransitionTask would permanently strand the Task in mission.TaskLeased
// (with no active lease and no attempt row), where ListSchedulableTasks would
// never pick it up again. A near-zero lease TTL deterministically makes
// StartAttempt observe an already-expired lease for the verification Task's
// own lease (acquired via this Adapter's leaseTTL) — not the target's lease,
// which newVerifiableTarget's fixture already acquired separately with a
// full one-hour TTL and which StartAttempt never inspects.
func TestDispatchRequeuesTaskWhenStartAttemptFails(t *testing.T) {
	repoRoot := newTestRepo(t)
	store := newTestStore(t)
	_, verifyTask := newVerifiableTarget(t, store, repoRoot)
	adapter := &Adapter{
		store:    store,
		repoRoot: repoRoot,
		leaseTTL: time.Nanosecond,
		baseCtx:  context.Background(),
	}

	err := adapter.Dispatch(context.Background(), verifyTask)
	if err == nil {
		t.Fatal("Dispatch error = nil, want an error since the lease is already expired by the time StartAttempt runs")
	}

	updated, getErr := store.GetTask(context.Background(), verifyTask.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if updated.Status != mission.TaskQueued {
		t.Fatalf("verify task status = %s, want queued (Dispatch must revert the Task to queued when StartAttempt fails, per scheduler.Dispatcher's contract)", updated.Status)
	}

	path, _ := verifyWorktreeFor(repoRoot, verifyTask)
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("worktree at %s still exists after a failed Dispatch, want it rolled back", path)
	}
}

func TestDispatchVerifiesSuccessfullyAndAdvancesTargetToSucceeded(t *testing.T) {
	repoRoot := newTestRepo(t)
	store := newTestStore(t)
	target, verifyTask := newVerifiableTarget(t, store, repoRoot)
	adapter := NewAdapter(store, repoRoot, context.Background())

	if err := adapter.Dispatch(context.Background(), verifyTask); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	waitForTaskStatus(t, store, target.ID, mission.TaskSucceeded, 30*time.Second)
	waitForTaskStatus(t, store, verifyTask.ID, mission.TaskSucceeded, time.Second)

	evidence, err := store.ListEvidence(context.Background(), target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence) != 1 || !evidence[0].Passed {
		t.Fatalf("evidence = %+v, want exactly one passing record", evidence)
	}
	targetAttempt, err := store.GetLatestAttempt(context.Background(), target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if evidence[0].AttemptID != targetAttempt.ID {
		t.Fatalf("evidence.AttemptID = %s, want the target's own attempt %s", evidence[0].AttemptID, targetAttempt.ID)
	}
	if evidence[0].VerifierAttemptID == "" || evidence[0].VerifierAttemptID == evidence[0].AttemptID {
		t.Fatalf("evidence.VerifierAttemptID = %q, want a distinct non-empty verifier attempt ID", evidence[0].VerifierAttemptID)
	}
}

func TestDispatchVerificationFailureFailsTargetButVerifierSucceeds(t *testing.T) {
	repoRoot := newTestRepo(t)
	store := newTestStore(t)
	target, verifyTask := newVerifiableTarget(t, store, repoRoot)
	// Break the implementation worktree's build so independent verification
	// genuinely fails (rather than faking a failure path).
	implLease, err := store.GetLatestLease(context.Background(), target.ID)
	if err != nil {
		t.Fatal(err)
	}
	implPath := implLease.Path
	if err := os.WriteFile(filepath.Join(implPath, "broken.go"), []byte("package main\nfunc broken( {\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, implPath, "add", "-A")
	runGit(t, implPath, "commit", "-q", "-m", "feat: introduce a build break")

	adapter := NewAdapter(store, repoRoot, context.Background())
	if err := adapter.Dispatch(context.Background(), verifyTask); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	waitForTaskStatus(t, store, target.ID, mission.TaskFailed, 30*time.Second)
	waitForTaskStatus(t, store, verifyTask.ID, mission.TaskSucceeded, time.Second)

	evidence, err := store.ListEvidence(context.Background(), target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence) != 1 || evidence[0].Passed {
		t.Fatalf("evidence = %+v, want exactly one failing record", evidence)
	}
}

// waitForTaskStatus polls the store until task reaches one of the wanted
// statuses or the timeout elapses, failing the test on timeout. Dispatch's
// completion runs in a background goroutine, so tests must poll rather than
// assert immediately after Dispatch returns. A generous timeout is used for
// the two tests above since they run a real `go build`/`go vet`/`go test`
// against the small synthetic Go module newTestRepo creates.
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
		time.Sleep(20 * time.Millisecond)
	}
}
