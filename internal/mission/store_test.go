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

func TestStoreEvidenceIsContentAddressedAndAppendOnly(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	mission, err := store.CreateMission(ctx, CreateMissionInput{Goal: "ship a feature"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, CreateTaskInput{MissionID: mission.ID, Title: "verify feature"})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := store.StartAttempt(ctx, task.ID, "local")
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := store.AddEvidence(ctx, CreateEvidenceInput{
		MissionID: mission.ID,
		TaskID:    task.ID,
		AttemptID: attempt.ID,
		Kind:      "go_test",
		Content:   []byte("ok\tgithub.com/harness9/internal/mission"),
		Passed:    true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if evidence.SHA256 == "" {
		t.Fatal("evidence SHA256 is empty")
	}
	if evidence.AttemptID != attempt.ID {
		t.Fatalf("attempt ID = %q, want %q", evidence.AttemptID, attempt.ID)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE evidence SET content = ? WHERE id = ?`, []byte("tampered"), evidence.ID); err == nil {
		t.Fatal("expected immutable evidence update to fail")
	}
	if _, err := store.AddEvidence(ctx, CreateEvidenceInput{
		MissionID: mission.ID,
		TaskID:    task.ID,
		AttemptID: attempt.ID,
		Kind:      "go_test",
		Content:   []byte("ok\tgithub.com/harness9/internal/mission"),
		Passed:    true,
	}); err != nil {
		t.Fatalf("adding duplicate evidence: %v", err)
	}
	got, err := store.ListEvidence(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("evidence count = %d, want 1", len(got))
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
