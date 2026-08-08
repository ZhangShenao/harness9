package coordinator

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/harness9/internal/mission"
	_ "modernc.org/sqlite"
)

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

func TestDecomposeGoal(t *testing.T) {
	store := newTestStore(t)
	coord := NewCoordinator(store)
	ctx := context.Background()

	m, plan, err := coord.DecomposeGoal(ctx, "implement feature X")
	if err != nil {
		t.Fatal(err)
	}
	if m.Status != mission.MissionPlanning {
		t.Fatalf("mission status = %q, want planning", m.Status)
	}
	if plan.Status != mission.PlanDraft {
		t.Fatalf("plan status = %q, want draft", plan.Status)
	}
}

func TestCreateTasksFromPlan(t *testing.T) {
	store := newTestStore(t)
	coord := NewCoordinator(store)
	ctx := context.Background()

	m, plan, _ := coord.DecomposeGoal(ctx, "implement feature X")
	if err := coord.CreateTaskFromPlan(ctx, plan.ID); err != nil {
		t.Fatal(err)
	}
	tasks, _ := store.ListTasks(ctx, m.ID)
	if len(tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(tasks))
	}
	if tasks[0].ContractKind != mission.ContractImplementation {
		t.Fatalf("contract kind = %q, want implementation", tasks[0].ContractKind)
	}
}

func TestMonitor(t *testing.T) {
	store := newTestStore(t)
	coord := NewCoordinator(store)
	ctx := context.Background()

	m, _, _ := coord.DecomposeGoal(ctx, "implement feature X")
	summary, err := coord.Monitor(ctx, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if summary == "" {
		t.Fatal("summary should not be empty")
	}
}
