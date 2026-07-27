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

	completeImplementation := func(task mission.Task, filename, content string) {
		t.Helper()
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
	completeImplementation(taskA, "feature_a.go", "package main\n\nfunc FeatureA() string { return \"a\" }\n")
	completeImplementation(taskB, "feature_b.go", "package main\n\nfunc FeatureB() string { return \"b\" }\n")

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
