package integration

import (
	"context"
	"database/sql"
	"os"
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

func TestDispatchCreatesIntegrationWorktreeLeaseAndAttempt(t *testing.T) {
	repoRoot := newTestRepo(t)
	store := newTestStore(t)
	ctx := context.Background()
	m, err := store.CreateMission(ctx, mission.CreateMissionInput{Goal: "ship a feature"})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := store.CreateDraftPlan(ctx, m.ID, mission.PlanInput{Tasks: []mission.TaskInput{
		{ClientID: "a", Position: 1, Title: "A", Contract: "do A", ContractKind: mission.ContractImplementation},
		{ClientID: "b", Position: 2, Title: "B", Contract: "do B", ContractKind: mission.ContractImplementation},
		{ClientID: "integrate", Position: 3, Title: "Integrate", Contract: "merge A and B", ContractKind: mission.ContractIntegration, Dependencies: []string{"a", "b"}},
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
	if _, err := store.MarkMissionRunning(ctx, m.ID); err != nil {
		t.Fatal(err)
	}
	tasks, err := store.ListTasks(ctx, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	var taskA, taskB, integrateTask mission.Task
	for _, task := range tasks {
		switch task.ClientID {
		case "a":
			taskA = task
		case "b":
			taskB = task
		case "integrate":
			integrateTask = task
		}
	}

	completeImplementation(t, store, repoRoot, taskA, "feature_a.go", "package main\n\nfunc FeatureA() string { return \"a\" }\n")
	completeImplementation(t, store, repoRoot, taskB, "feature_b.go", "package main\n\nfunc FeatureB() string { return \"b\" }\n")

	integrateTask, err = store.GetTask(ctx, integrateTask.ID)
	if err != nil {
		t.Fatal(err)
	}
	if integrateTask.Status != mission.TaskQueued {
		t.Fatalf("integrate task status = %s, want queued once both dependencies succeeded", integrateTask.Status)
	}

	adapter := NewAdapter(store, repoRoot, context.Background())
	if err := adapter.Dispatch(context.Background(), integrateTask); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	path, _ := integrationWorktreeFor(repoRoot, integrateTask)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("integration worktree was not created: %v", err)
	}
	updated, err := store.GetTask(context.Background(), integrateTask.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != mission.TaskRunning {
		t.Fatalf("integrate task status = %s, want running", updated.Status)
	}

	// Dispatch's synchronous bookkeeping above is this test's actual subject,
	// but Dispatch also launches a.run on a background goroutine that now
	// performs a real merge and go build/vet/test against the worktree
	// (Task 3), instead of Task 2's no-op. Returning before that goroutine
	// finishes would let it keep running after t.Cleanup closes this test's
	// store's DB, so wait for the integrate Task to reach a terminal status
	// first.
	waitForTaskStatus(t, store, integrateTask.ID, mission.TaskSucceeded, 30*time.Second)
}

func TestDispatchRejectsTaskWithNoDependencies(t *testing.T) {
	store := newTestStore(t)
	repoRoot := newTestRepo(t)
	adapter := NewAdapter(store, repoRoot, context.Background())

	err := adapter.Dispatch(context.Background(), mission.Task{ID: "orphan-integration", ContractKind: mission.ContractIntegration})
	if err == nil {
		t.Fatal("Dispatch on an integration task with zero dependencies = nil error, want an error")
	}
}

func TestDispatchMergesAndVerifiesSuccessfullyCompletingTheMission(t *testing.T) {
	repoRoot := newTestRepo(t)
	store := newTestStore(t)
	ctx := context.Background()
	m, err := store.CreateMission(ctx, mission.CreateMissionInput{Goal: "ship a feature"})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := store.CreateDraftPlan(ctx, m.ID, mission.PlanInput{Tasks: []mission.TaskInput{
		{ClientID: "a", Position: 1, Title: "A", Contract: "do A", ContractKind: mission.ContractImplementation},
		{ClientID: "b", Position: 2, Title: "B", Contract: "do B", ContractKind: mission.ContractImplementation},
		{ClientID: "integrate", Position: 3, Title: "Integrate", Contract: "merge A and B", ContractKind: mission.ContractIntegration, Dependencies: []string{"a", "b"}},
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
	if _, err := store.MarkMissionRunning(ctx, m.ID); err != nil {
		t.Fatal(err)
	}
	tasks, err := store.ListTasks(ctx, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	var taskA, taskB, integrateTask mission.Task
	for _, task := range tasks {
		switch task.ClientID {
		case "a":
			taskA = task
		case "b":
			taskB = task
		case "integrate":
			integrateTask = task
		}
	}
	completeImplementation(t, store, repoRoot, taskA, "feature_a.go", "package main\n\nfunc FeatureA() string { return \"a\" }\n")
	completeImplementation(t, store, repoRoot, taskB, "feature_b.go", "package main\n\nfunc FeatureB() string { return \"b\" }\n")
	integrateTask, err = store.GetTask(ctx, integrateTask.ID)
	if err != nil {
		t.Fatal(err)
	}

	adapter := NewAdapter(store, repoRoot, context.Background())
	if err := adapter.Dispatch(ctx, integrateTask); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	waitForTaskStatus(t, store, integrateTask.ID, mission.TaskSucceeded, 30*time.Second)
	waitForMissionStatus(t, store, m.ID, mission.MissionSucceeded, time.Second)

	evidence, err := store.ListEvidence(context.Background(), integrateTask.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence) != 1 || !evidence[0].Passed {
		t.Fatalf("evidence = %+v, want exactly one passing record", evidence)
	}
	path, _ := integrationWorktreeFor(repoRoot, integrateTask)
	if _, err := os.Stat(filepath.Join(path, "feature_a.go")); err != nil {
		t.Fatalf("merged worktree missing feature_a.go: %v", err)
	}
	if _, err := os.Stat(filepath.Join(path, "feature_b.go")); err != nil {
		t.Fatalf("merged worktree missing feature_b.go: %v", err)
	}
}

func TestDispatchEscalatesMissionOnMergeConflict(t *testing.T) {
	repoRoot := newTestRepo(t)
	store := newTestStore(t)
	ctx := context.Background()
	m, err := store.CreateMission(ctx, mission.CreateMissionInput{Goal: "ship a feature"})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := store.CreateDraftPlan(ctx, m.ID, mission.PlanInput{Tasks: []mission.TaskInput{
		{ClientID: "a", Position: 1, Title: "A", Contract: "do A", ContractKind: mission.ContractImplementation},
		{ClientID: "b", Position: 2, Title: "B", Contract: "do B", ContractKind: mission.ContractImplementation},
		{ClientID: "integrate", Position: 3, Title: "Integrate", Contract: "merge A and B", ContractKind: mission.ContractIntegration, Dependencies: []string{"a", "b"}},
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
	if _, err := store.MarkMissionRunning(ctx, m.ID); err != nil {
		t.Fatal(err)
	}
	tasks, err := store.ListTasks(ctx, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	var taskA, taskB, integrateTask mission.Task
	for _, task := range tasks {
		switch task.ClientID {
		case "a":
			taskA = task
		case "b":
			taskB = task
		case "integrate":
			integrateTask = task
		}
	}
	// Both A and B change the exact same line of main.go, guaranteeing a
	// genuine merge conflict when Integration tries to combine them.
	completeImplementation(t, store, repoRoot, taskA, "main.go", "package main\n\nfunc main() { println(\"from a\") }\n")
	completeImplementation(t, store, repoRoot, taskB, "main.go", "package main\n\nfunc main() { println(\"from b\") }\n")
	integrateTask, err = store.GetTask(ctx, integrateTask.ID)
	if err != nil {
		t.Fatal(err)
	}

	adapter := NewAdapter(store, repoRoot, context.Background())
	if err := adapter.Dispatch(ctx, integrateTask); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	waitForTaskStatus(t, store, integrateTask.ID, mission.TaskFailed, 30*time.Second)
	waitForMissionStatus(t, store, m.ID, mission.MissionNeedsAttention, time.Second)

	evidence, err := store.ListEvidence(context.Background(), integrateTask.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence) != 1 || evidence[0].Passed {
		t.Fatalf("evidence = %+v, want exactly one failing record", evidence)
	}
}

// completeImplementation simulates a completed Worker Adapter run on task: a
// real worktree, a real commit, and a full Running->Verifying->Succeeded
// transition — reused by both this task's tests and Task 2's.
func completeImplementation(t *testing.T, store *mission.Store, repoRoot string, task mission.Task, filename, content string) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(repoRoot, ".harness9", "missions", task.ID)
	branch := "mission/" + task.ID
	runGit(t, repoRoot, "worktree", "add", path, "-b", branch)
	if err := os.WriteFile(filepath.Join(path, filename), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, path, "add", "-A")
	runGit(t, path, "commit", "-q", "-m", "feat: implement "+task.ClientID)
	if _, err := store.AcquireLease(ctx, task.ID, path, branch, "", time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := store.StartAttempt(ctx, task.ID, "worker-adapter"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionTask(ctx, task.ID, mission.TaskVerifying); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionTask(ctx, task.ID, mission.TaskSucceeded); err != nil {
		t.Fatal(err)
	}
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
		time.Sleep(20 * time.Millisecond)
	}
}

// waitForMissionStatus mirrors waitForTaskStatus for Mission-level status.
func waitForMissionStatus(t *testing.T, store *mission.Store, missionID string, want mission.MissionStatus, timeout time.Duration) mission.Mission {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		m, err := store.GetMission(context.Background(), missionID)
		if err != nil {
			t.Fatal(err)
		}
		if m.Status == want {
			return m
		}
		if time.Now().After(deadline) {
			t.Fatalf("mission %s status = %s after %s, want %s", missionID, m.Status, timeout, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
