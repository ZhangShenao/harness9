package mission

import (
	"context"
	"testing"
)

func TestListSchedulableTasksEmpty(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	tasks, err := store.ListSchedulableTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 0 {
		t.Fatalf("expected 0 tasks, got %d", len(tasks))
	}
}

func TestListSchedulableTasksWithActivePlan(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	m, _ := store.CreateMission(ctx, CreateMissionInput{Goal: "ship"})
	plan, _ := store.CreatePlan(ctx, m.ID, `[]`)
	store.ApprovePlan(ctx, plan.ID)
	task, _ := store.CreateTask(ctx, CreateTaskInput{MissionID: m.ID, Title: "impl"})
	tasks, err := store.ListSchedulableTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].ID != task.ID {
		t.Fatalf("schedulable = %d tasks, want 1 with %s", len(tasks), task.ID)
	}
}

func TestListSchedulableTasksNoActivePlan(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	m, _ := store.CreateMission(ctx, CreateMissionInput{Goal: "ship"})
	store.CreateTask(ctx, CreateTaskInput{MissionID: m.ID, Title: "impl"})
	tasks, _ := store.ListSchedulableTasks(ctx)
	if len(tasks) != 0 {
		t.Fatalf("expected 0 without active plan, got %d", len(tasks))
	}
}

func TestListSchedulableTasksFiltersBlockedDeps(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	m, _ := store.CreateMission(ctx, CreateMissionInput{Goal: "ship"})
	plan, _ := store.CreatePlan(ctx, m.ID, `[]`)
	store.ApprovePlan(ctx, plan.ID)
	first, _ := store.CreateTask(ctx, CreateTaskInput{MissionID: m.ID, Title: "first"})
	second, _ := store.CreateTask(ctx, CreateTaskInput{MissionID: m.ID, Title: "second", DependsOn: []string{first.ID}})
	if second.Status != TaskBlocked {
		t.Fatalf("second should be blocked, got %q", second.Status)
	}
	tasks, _ := store.ListSchedulableTasks(ctx)
	if len(tasks) != 1 || tasks[0].ID != first.ID {
		t.Fatalf("expected only first task schedulable, got %d", len(tasks))
	}
}

func TestActiveTaskCounts(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	m, _ := store.CreateMission(ctx, CreateMissionInput{Goal: "ship"})
	task, _ := store.CreateTask(ctx, CreateTaskInput{MissionID: m.ID, Title: "impl"})
	store.StartAttempt(ctx, task.ID, "worker")
	store.db.ExecContext(ctx, `UPDATE tasks SET status = ? WHERE id = ?`, TaskRunning, task.ID)
	counts, err := store.ActiveTaskCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts["__global__"] != 1 {
		t.Fatalf("global = %d, want 1", counts["__global__"])
	}
	if counts[m.ID] != 1 {
		t.Fatalf("mission = %d, want 1", counts[m.ID])
	}
}

func TestMarkMissionRunningIdempotent(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	m, _ := store.CreateMission(ctx, CreateMissionInput{Goal: "ship"})
	store.db.ExecContext(ctx, `UPDATE missions SET status = ? WHERE id = ?`, MissionReady, m.ID)
	if err := store.MarkMissionRunning(ctx, m.ID); err != nil {
		t.Fatal(err)
	}
	var status string
	store.db.QueryRowContext(ctx, `SELECT status FROM missions WHERE id = ?`, m.ID).Scan(&status)
	if status != string(MissionRunning) {
		t.Fatalf("status = %q, want running", status)
	}
	if err := store.MarkMissionRunning(ctx, m.ID); err != nil {
		t.Fatalf("idempotent call failed: %v", err)
	}
}

func TestMarkAttemptFinished(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	m, _ := store.CreateMission(ctx, CreateMissionInput{Goal: "ship"})
	task, _ := store.CreateTask(ctx, CreateTaskInput{MissionID: m.ID, Title: "impl"})
	attempt, _ := store.StartAttempt(ctx, task.ID, "worker")
	if err := store.MarkAttemptFinished(ctx, attempt.ID, "succeeded", "done"); err != nil {
		t.Fatal(err)
	}
	var status, exitReason string
	store.db.QueryRowContext(ctx, `SELECT status, exit_reason FROM task_attempts WHERE id = ?`, attempt.ID).Scan(&status, &exitReason)
	if status != "succeeded" || exitReason != "done" {
		t.Fatalf("status=%q exitReason=%q", status, exitReason)
	}
}

func TestGetLatestAttempt(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	m, _ := store.CreateMission(ctx, CreateMissionInput{Goal: "ship"})
	task, _ := store.CreateTask(ctx, CreateTaskInput{MissionID: m.ID, Title: "impl"})
	first, _ := store.StartAttempt(ctx, task.ID, "worker")
	second, _ := store.StartAttempt(ctx, task.ID, "worker")
	got, err := store.GetLatestAttempt(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != second.ID {
		t.Fatalf("latest = %s, want %s (first was %s)", got.ID, second.ID, first.ID)
	}
}

func TestGetLatestAttemptNotFound(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	_, err := store.GetLatestAttempt(ctx, "nonexistent")
	if err != ErrNotFound {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}
