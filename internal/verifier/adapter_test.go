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
	_, verifyTask := newVerifiableTarget(t, store, repoRoot)
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
