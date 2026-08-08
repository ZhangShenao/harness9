package integration

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

func TestIntegrationNoDeps(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	m, _ := store.CreateMission(ctx, mission.CreateMissionInput{Goal: "ship"})
	task, _ := store.CreateTask(ctx, mission.CreateTaskInput{MissionID: m.ID, Title: "integrate"})
	attempt, _ := store.StartAttempt(ctx, task.ID, "integration")

	adapter := NewAdapter(store, t.TempDir())
	result, err := adapter.Dispatch(ctx, task, attempt)
	if err != nil {
		t.Fatal(err)
	}
	// With no deps, merge passes; test may pass or fail in empty dir
	_ = result
	evidence, _ := store.ListEvidence(ctx, task.ID)
	if len(evidence) < 1 {
		t.Fatalf("evidence count = %d, want >= 1", len(evidence))
	}
}
