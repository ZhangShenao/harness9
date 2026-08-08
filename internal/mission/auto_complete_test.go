package mission

import (
	"context"
	"testing"
)

func TestTryCompleteMissionNotAllSucceeded(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	m, _ := store.CreateMission(ctx, CreateMissionInput{Goal: "ship"})
	plan, _ := store.CreatePlan(ctx, m.ID, `[]`)
	store.ApprovePlan(ctx, plan.ID)
	store.DB().ExecContext(ctx, `UPDATE missions SET status = ? WHERE id = ?`, MissionRunning, m.ID)
	store.CreateTask(ctx, CreateTaskInput{MissionID: m.ID, Title: "a"})
	store.CreateTask(ctx, CreateTaskInput{MissionID: m.ID, Title: "b"})
	completed, err := store.TryCompleteMission(ctx, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if completed {
		t.Fatal("should not complete with pending tasks")
	}
}

func TestTryCompleteMissionAllSucceeded(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	m, _ := store.CreateMission(ctx, CreateMissionInput{Goal: "ship"})
	plan, _ := store.CreatePlan(ctx, m.ID, `[]`)
	store.ApprovePlan(ctx, plan.ID)
	store.DB().ExecContext(ctx, `UPDATE missions SET status = ? WHERE id = ?`, MissionRunning, m.ID)
	task1, _ := store.CreateTask(ctx, CreateTaskInput{MissionID: m.ID, Title: "a"})
	task2, _ := store.CreateTask(ctx, CreateTaskInput{MissionID: m.ID, Title: "b"})
	for _, task := range []Task{task1, task2} {
		store.DB().ExecContext(ctx, `UPDATE tasks SET status = ? WHERE id = ?`, TaskSucceeded, task.ID)
	}
	completed, err := store.TryCompleteMission(ctx, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !completed {
		t.Fatal("should complete with all tasks succeeded")
	}
	got, _ := store.GetMission(ctx, m.ID)
	if got.Status != MissionSucceeded {
		t.Fatalf("mission status = %q, want succeeded", got.Status)
	}
}

func TestGetMission(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	m, _ := store.CreateMission(ctx, CreateMissionInput{Goal: "test goal"})
	got, err := store.GetMission(ctx, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Goal != "test goal" {
		t.Fatalf("goal = %q", got.Goal)
	}
}

func TestTransitionMissionInvalid(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	m, _ := store.CreateMission(ctx, CreateMissionInput{Goal: "test"})
	_, err := store.TransitionMission(ctx, m.ID, MissionRunning)
	if err == nil {
		t.Fatal("expected error: draft -> running is invalid")
	}
}
