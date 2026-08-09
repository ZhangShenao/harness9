package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/harness9/internal/mission"
)

// TestE2EMultiTaskMission tests the full Mission lifecycle:
// create -> plan -> approve -> dispatch (2 tasks with dep) -> auto-complete.
func TestE2EMultiTaskMission(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Setup mission with approved plan
	m, _ := store.CreateMission(ctx, mission.CreateMissionInput{Goal: "ship feature"})
	plan, _ := store.CreatePlan(ctx, m.ID, `[]`)
	store.ApprovePlan(ctx, plan.ID)
	store.DB().ExecContext(ctx, `UPDATE missions SET status = ? WHERE id = ?`, mission.MissionReady, m.ID)

	// Create two tasks: impl1 (no deps) + impl2 (depends on impl1)
	task1, _ := store.CreateTask(ctx, mission.CreateTaskInput{MissionID: m.ID, Title: "implement A"})
	store.DB().ExecContext(ctx, `UPDATE tasks SET contract_kind = ? WHERE id = ?`, mission.ContractImplementation, task1.ID)
	task2, _ := store.CreateTask(ctx, mission.CreateTaskInput{MissionID: m.ID, Title: "implement B", DependsOn: []string{task1.ID}})
	store.DB().ExecContext(ctx, `UPDATE tasks SET contract_kind = ? WHERE id = ?`, mission.ContractImplementation, task2.ID)

	// Mock dispatcher that always succeeds
	mock := &mockDispatcher{result: Result{Status: "succeeded"}}
	rd := NewRoutingDispatcher()
	rd.Register(mission.ContractImplementation, mock)

	sched := NewScheduler(SchedulerConfig{Store: store, Dispatchers: rd, GlobalConcurrency: 2})

	// First tick: only task1 should dispatch (task2 is blocked)
	sched.Tick(ctx)
	time.Sleep(100 * time.Millisecond)

	updated1, _ := store.GetTask(ctx, task1.ID)
	if updated1.Status != mission.TaskSucceeded {
		t.Fatalf("task1 status = %q, want succeeded", updated1.Status)
	}

	// task2 should now be queued (dependency satisfied)
	updated2, _ := store.GetTask(ctx, task2.ID)
	if updated2.Status != mission.TaskQueued {
		t.Fatalf("task2 status = %q, want queued after dep satisfied", updated2.Status)
	}

	// Second tick: dispatch task2
	sched.Tick(ctx)
	time.Sleep(100 * time.Millisecond)

	updated2, _ = store.GetTask(ctx, task2.ID)
	if updated2.Status != mission.TaskSucceeded {
		t.Fatalf("task2 status = %q, want succeeded", updated2.Status)
	}

	// Mission should auto-complete
	completedMission, _ := store.GetMission(ctx, m.ID)
	if completedMission.Status != mission.MissionSucceeded {
		t.Fatalf("mission status = %q, want succeeded", completedMission.Status)
	}
}

// TestE2ECrashRecovery tests that interrupted attempts are marked indeterminate.
func TestE2ECrashRecovery(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	m, _ := store.CreateMission(ctx, mission.CreateMissionInput{Goal: "ship"})
	plan, _ := store.CreatePlan(ctx, m.ID, `[]`)
	store.ApprovePlan(ctx, plan.ID)
	store.DB().ExecContext(ctx, `UPDATE missions SET status = ? WHERE id = ?`, mission.MissionRunning, m.ID)
	task, _ := store.CreateTask(ctx, mission.CreateTaskInput{MissionID: m.ID, Title: "impl"})
	store.DB().ExecContext(ctx, `UPDATE tasks SET status = ? WHERE id = ?`, mission.TaskRunning, task.ID)
	store.StartAttempt(ctx, task.ID, "worker")

	// Simulate crash: process restarts, Reconcile finds interrupted attempt
	rd := NewRoutingDispatcher()
	sched := NewScheduler(SchedulerConfig{Store: store, Dispatchers: rd, GlobalConcurrency: 2})
	if err := sched.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	// Task should be indeterminate (never blindly retry)
	updated, _ := store.GetTask(ctx, task.ID)
	if updated.Status != mission.TaskIndeterminate {
		t.Fatalf("task status = %q, want indeterminate after crash", updated.Status)
	}
}

// TestE2EFailedTaskDoesNotCompleteMission tests that a failed task
// prevents Mission auto-completion.
func TestE2EFailedTaskDoesNotCompleteMission(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	m, _ := store.CreateMission(ctx, mission.CreateMissionInput{Goal: "ship"})
	plan, _ := store.CreatePlan(ctx, m.ID, `[]`)
	store.ApprovePlan(ctx, plan.ID)
	store.DB().ExecContext(ctx, `UPDATE missions SET status = ? WHERE id = ?`, mission.MissionReady, m.ID)
	task, _ := store.CreateTask(ctx, mission.CreateTaskInput{MissionID: m.ID, Title: "impl"})
	store.DB().ExecContext(ctx, `UPDATE tasks SET contract_kind = ? WHERE id = ?`, mission.ContractImplementation, task.ID)

	mock := &mockDispatcher{result: Result{Status: "failed", ExitReason: "build error"}}
	rd := NewRoutingDispatcher()
	rd.Register(mission.ContractImplementation, mock)

	sched := NewScheduler(SchedulerConfig{Store: store, Dispatchers: rd, GlobalConcurrency: 2})
	sched.Tick(ctx)
	time.Sleep(100 * time.Millisecond)

	updated, _ := store.GetTask(ctx, task.ID)
	if updated.Status != mission.TaskFailed {
		t.Fatalf("task status = %q, want failed", updated.Status)
	}
	m2, _ := store.GetMission(ctx, m.ID)
	if m2.Status == mission.MissionSucceeded {
		t.Fatal("mission should not succeed with failed task")
	}
}
