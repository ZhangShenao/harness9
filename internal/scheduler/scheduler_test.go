package scheduler

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/harness9/internal/mission"
)

// fakeDispatcher simulates a Worker Adapter by performing the same Store
// transitions a real Dispatcher must perform: acquire a lease and start an
// Attempt. This exercises real concurrency accounting without needing git
// worktrees, Sandbox containers, or a real sub-agent.
type fakeDispatcher struct {
	store    *mission.Store
	failTask map[string]bool
}

func (f *fakeDispatcher) Dispatch(ctx context.Context, task mission.Task) error {
	if f.failTask[task.ID] {
		return errors.New("simulated dispatch failure")
	}
	if _, err := f.store.AcquireLease(ctx, task.ID, "/tmp/fake-"+task.ID, "fake-branch", "fake-sandbox", time.Hour); err != nil {
		return err
	}
	if _, err := f.store.StartAttempt(ctx, task.ID, "fake-worker"); err != nil {
		return err
	}
	return nil
}

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

func approvedMissionWithTwoRootTasks(t *testing.T, store *mission.Store, policyJSON string) mission.Mission {
	t.Helper()
	ctx := context.Background()
	m, err := store.CreateMission(ctx, mission.CreateMissionInput{Goal: "ship feature", PolicyJSON: policyJSON})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := store.CreateDraftPlan(ctx, m.ID, mission.PlanInput{Tasks: []mission.TaskInput{
		{ClientID: "a", Position: 1, Title: "Task A", Contract: "A done"},
		{ClientID: "b", Position: 2, Title: "Task B", Contract: "B done"},
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
	got, err := store.GetMission(ctx, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestTickRespectsPerMissionConcurrencyLimit(t *testing.T) {
	store := newTestStore(t)
	m := approvedMissionWithTwoRootTasks(t, store, `{"max_concurrent_tasks":1}`)
	dispatcher := &fakeDispatcher{store: store, failTask: map[string]bool{}}
	s := NewScheduler(store, dispatcher, WithMaxGlobalConcurrency(10))

	dispatched, err := s.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if dispatched != 1 {
		t.Fatalf("dispatched = %d, want 1 (mission cap is 1)", dispatched)
	}

	dispatched, err = s.Tick(context.Background())
	if err != nil {
		t.Fatalf("second Tick: %v", err)
	}
	if dispatched != 0 {
		t.Fatalf("second tick dispatched = %d, want 0 (mission still at cap)", dispatched)
	}

	got, err := store.GetMission(context.Background(), m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != mission.MissionRunning {
		t.Fatalf("mission status = %s, want running", got.Status)
	}
}

func TestTickRespectsGlobalConcurrencyAcrossMissions(t *testing.T) {
	store := newTestStore(t)
	approvedMissionWithTwoRootTasks(t, store, `{"max_concurrent_tasks":2}`)
	approvedMissionWithTwoRootTasks(t, store, `{"max_concurrent_tasks":2}`)
	dispatcher := &fakeDispatcher{store: store, failTask: map[string]bool{}}
	s := NewScheduler(store, dispatcher, WithMaxGlobalConcurrency(1))

	dispatched, err := s.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if dispatched != 1 {
		t.Fatalf("dispatched = %d, want 1 (global cap is 1)", dispatched)
	}
}

func TestTickLeavesFailedTaskQueuedAndContinuesOthers(t *testing.T) {
	store := newTestStore(t)
	approvedMissionWithTwoRootTasks(t, store, `{}`)
	tasks, err := store.ListSchedulableTasks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 2 {
		t.Fatalf("schedulable tasks = %d, want 2", len(tasks))
	}
	failing := tasks[0].ID
	dispatcher := &fakeDispatcher{store: store, failTask: map[string]bool{failing: true}}
	s := NewScheduler(store, dispatcher, WithMaxGlobalConcurrency(10))

	dispatched, err := s.Tick(context.Background())
	if err == nil {
		t.Fatal("Tick error = nil, want an error for the failing task")
	}
	if dispatched != 1 {
		t.Fatalf("dispatched = %d, want 1 (the non-failing task)", dispatched)
	}

	failedTask, err := store.GetTask(context.Background(), failing)
	if err != nil {
		t.Fatal(err)
	}
	if failedTask.Status != mission.TaskQueued {
		t.Fatalf("failed task status = %s, want queued so it retries next tick", failedTask.Status)
	}
}
