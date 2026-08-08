package scheduler

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/harness9/internal/mission"
	_ "modernc.org/sqlite"
)

type mockDispatcher struct {
	called bool
	result Result
	delay  time.Duration
}

func (m *mockDispatcher) Dispatch(ctx context.Context, task mission.Task, attempt mission.TaskAttempt) (Result, error) {
	m.called = true
	if m.delay > 0 {
		time.Sleep(m.delay)
	}
	return m.result, nil
}

func newTestStore(t *testing.T) *mission.Store {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "test.db"))
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

func TestRoutingDispatcherRoutesByContractKind(t *testing.T) {
	impl := &mockDispatcher{result: Result{Status: "succeeded"}}
	rd := NewRoutingDispatcher()
	rd.Register(mission.ContractImplementation, impl)
	task := mission.Task{ContractKind: mission.ContractImplementation}
	result, err := rd.Dispatch(context.Background(), task, mission.TaskAttempt{})
	if err != nil {
		t.Fatal(err)
	}
	if !impl.called || result.Status != "succeeded" {
		t.Fatalf("dispatch failed: called=%v status=%q", impl.called, result.Status)
	}
}

func TestRoutingDispatcherUnregisteredKind(t *testing.T) {
	rd := NewRoutingDispatcher()
	_, err := rd.Dispatch(context.Background(), mission.Task{ContractKind: "unknown"}, mission.TaskAttempt{})
	if err == nil {
		t.Fatal("expected error for unregistered kind")
	}
}

func setupMissionWithTask(t *testing.T, store *mission.Store, ctx context.Context) (mission.Mission, mission.Task) {
	t.Helper()
	m, _ := store.CreateMission(ctx, mission.CreateMissionInput{Goal: "ship"})
	plan, _ := store.CreatePlan(ctx, m.ID, `[]`)
	store.ApprovePlan(ctx, plan.ID)
	store.DB().ExecContext(ctx, `UPDATE missions SET status = ? WHERE id = ?`, mission.MissionReady, m.ID)
	task, _ := store.CreateTask(ctx, mission.CreateTaskInput{MissionID: m.ID, Title: "impl"})
	store.DB().ExecContext(ctx, `UPDATE tasks SET contract_kind = ? WHERE id = ?`, mission.ContractImplementation, task.ID)
	return m, task
}

func TestSchedulerDispatchesQueuedTask(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	_, task := setupMissionWithTask(t, store, ctx)

	mock := &mockDispatcher{result: Result{Status: "succeeded"}}
	rd := NewRoutingDispatcher()
	rd.Register(mission.ContractImplementation, mock)

	sched := NewScheduler(SchedulerConfig{Store: store, Dispatchers: rd, GlobalConcurrency: 2})
	sched.Tick(ctx)
	time.Sleep(100 * time.Millisecond)

	updated, _ := store.GetTask(ctx, task.ID)
	if updated.Status != mission.TaskSucceeded {
		t.Fatalf("task status = %q, want succeeded", updated.Status)
	}
}

func TestSchedulerRespectsConcurrencyLimit(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	m, _ := setupMissionWithTask(t, store, ctx)
	for i := 0; i < 2; i++ {
		task, _ := store.CreateTask(ctx, mission.CreateTaskInput{MissionID: m.ID, Title: "impl"})
		store.DB().ExecContext(ctx, `UPDATE tasks SET contract_kind = ? WHERE id = ?`, mission.ContractImplementation, task.ID)
	}

	block := make(chan struct{})
	rd := NewRoutingDispatcher()
	rd.Register(mission.ContractImplementation, &blockingDispatcher{block: block})

	sched := NewScheduler(SchedulerConfig{Store: store, Dispatchers: rd, GlobalConcurrency: 1})
	sched.Tick(ctx)

	counts, _ := store.ActiveTaskCounts(ctx)
	if counts["__global__"] != 1 {
		t.Fatalf("active = %d, want 1", counts["__global__"])
	}
	close(block)
	time.Sleep(100 * time.Millisecond)
}

type blockingDispatcher struct {
	block chan struct{}
}

func (b *blockingDispatcher) Dispatch(ctx context.Context, task mission.Task, attempt mission.TaskAttempt) (Result, error) {
	<-b.block
	return Result{Status: "succeeded"}, nil
}

func TestReconcileMarksInterruptedAttempt(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	m, _ := store.CreateMission(ctx, mission.CreateMissionInput{Goal: "ship"})
	plan, _ := store.CreatePlan(ctx, m.ID, `[]`)
	store.ApprovePlan(ctx, plan.ID)
	store.DB().ExecContext(ctx, `UPDATE missions SET status = ? WHERE id = ?`, mission.MissionRunning, m.ID)
	task, _ := store.CreateTask(ctx, mission.CreateTaskInput{MissionID: m.ID, Title: "impl"})
	store.DB().ExecContext(ctx, `UPDATE tasks SET status = ? WHERE id = ?`, mission.TaskRunning, task.ID)
	attempt, _ := store.StartAttempt(ctx, task.ID, "worker")

	rd := NewRoutingDispatcher()
	sched := NewScheduler(SchedulerConfig{Store: store, Dispatchers: rd, GlobalConcurrency: 2})
	if err := sched.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	updated, _ := store.GetTask(ctx, task.ID)
	if updated.Status != mission.TaskIndeterminate {
		t.Fatalf("status = %q, want indeterminate", updated.Status)
	}
	finished, _ := store.GetLatestAttempt(ctx, task.ID)
	if finished.ID != attempt.ID {
		t.Fatalf("attempt mismatch")
	}
	if finished.Status != "indeterminate" {
		t.Fatalf("attempt status = %q, want indeterminate", finished.Status)
	}
}

func TestIntegrationSingleWorkerMission(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	_, task := setupMissionWithTask(t, store, ctx)

	mock := &mockDispatcher{result: Result{Status: "succeeded"}}
	rd := NewRoutingDispatcher()
	rd.Register(mission.ContractImplementation, mock)

	sched := NewScheduler(SchedulerConfig{Store: store, Dispatchers: rd, GlobalConcurrency: 2})
	sched.Tick(ctx)
	time.Sleep(100 * time.Millisecond)

	updated, _ := store.GetTask(ctx, task.ID)
	if updated.Status != mission.TaskSucceeded {
		t.Fatalf("task status = %q, want succeeded", updated.Status)
	}
}
