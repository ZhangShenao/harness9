package mission

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestStoreCompletingDependencyQueuesBlockedTask(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	mission, err := store.CreateMission(ctx, CreateMissionInput{Goal: "ship a feature"})
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.CreateTask(ctx, CreateTaskInput{MissionID: mission.ID, Title: "write specification"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.CreateTask(ctx, CreateTaskInput{
		MissionID: mission.ID,
		Title:     "implement feature",
		DependsOn: []string{first.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.Status != TaskBlocked {
		t.Fatalf("initial status = %q, want %q", second.Status, TaskBlocked)
	}

	for _, status := range []TaskStatus{TaskLeased, TaskRunning, TaskVerifying, TaskSucceeded} {
		if _, err := store.TransitionTask(ctx, first.ID, status); err != nil {
			t.Fatalf("transition to %q: %v", status, err)
		}
	}

	got, err := store.GetTask(ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != TaskQueued {
		t.Fatalf("dependent status = %q, want %q", got.Status, TaskQueued)
	}
}

func TestStoreRejectsDirectTaskSuccess(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	mission, err := store.CreateMission(ctx, CreateMissionInput{Goal: "ship a feature"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, CreateTaskInput{MissionID: mission.ID, Title: "implement feature"})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := store.TransitionTask(ctx, task.ID, TaskSucceeded); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("TransitionTask error = %v, want ErrInvalidTransition", err)
	}
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "mission.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	return store
}
