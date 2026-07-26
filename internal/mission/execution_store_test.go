package mission

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

func TestAcquireLeaseRejectsSecondActiveLeaseForTask(t *testing.T) {
	store, task := newReadyTask(t)

	first, err := store.AcquireLease(
		context.Background(),
		task.ID,
		"wt/api",
		"branch/api",
		"sbx-1",
		time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == "" {
		t.Fatal("lease ID 不能为空")
	}
	if _, err := store.AcquireLease(
		context.Background(),
		task.ID,
		"wt/other",
		"branch/other",
		"sbx-2",
		time.Hour,
	); err == nil {
		t.Fatal("同一 Task 的活跃 Lease 必须冲突")
	}
}

func TestMarkInterruptedAttemptsIndeterminate(t *testing.T) {
	store, attempt := newRunningAttempt(t)

	count, err := store.MarkInterruptedAttemptsIndeterminate(context.Background())
	if err != nil || count != 1 {
		t.Fatalf("got count=%d err=%v", count, err)
	}
	got, err := store.GetAttempt(context.Background(), attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != AttemptIndeterminate {
		t.Fatalf("got %s", got.Status)
	}
	if got.LeaseID == "" {
		t.Fatal("recovered Attempt lost its Lease ID")
	}
	task, err := store.GetTask(context.Background(), attempt.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != TaskIndeterminate {
		t.Fatalf("Task status = %s, want %s", task.Status, TaskIndeterminate)
	}
	var leaseStatus string
	if err := store.db.QueryRowContext(context.Background(), `
		SELECT status FROM workspace_leases WHERE id = ?`, got.LeaseID,
	).Scan(&leaseStatus); err != nil {
		t.Fatal(err)
	}
	if leaseStatus != "expired" {
		t.Fatalf("recovered Lease status = %q, want expired", leaseStatus)
	}
	recoverable, err := store.ListRecoverableAttempts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(recoverable) != 1 || recoverable[0].ID != attempt.ID {
		t.Fatalf("recoverable Attempts = %+v", recoverable)
	}
}

func TestRecoveryMarksLegacyQueuedAttemptAndTaskIndeterminate(t *testing.T) {
	store, task := newReadyTask(t)
	attempt, err := store.StartAttempt(
		context.Background(),
		task.ID,
		"legacy-worker",
	)
	if err != nil {
		t.Fatal(err)
	}
	before, err := store.GetTask(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if before.Status != TaskQueued {
		t.Fatalf("legacy Task status = %s, want %s", before.Status, TaskQueued)
	}

	count, err := store.MarkInterruptedAttemptsIndeterminate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("recovered Attempt count = %d, want 1", count)
	}
	gotAttempt, err := store.GetAttempt(context.Background(), attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotAttempt.Status != AttemptIndeterminate {
		t.Fatalf("legacy Attempt status = %s, want %s", gotAttempt.Status, AttemptIndeterminate)
	}
	gotTask, err := store.GetTask(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotTask.Status != TaskIndeterminate {
		t.Fatalf("legacy Task status = %s, want %s", gotTask.Status, TaskIndeterminate)
	}
}

func TestNewStoreMigratesMissionEventsToAppendOnly(t *testing.T) {
	db := openSharedMemoryDB(t)
	store, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	mission, err := store.CreateMission(
		context.Background(),
		CreateMissionInput{Goal: "preserve audit events"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		DROP TRIGGER IF EXISTS prevent_mission_event_update;
		DROP TRIGGER IF EXISTS prevent_mission_event_delete;
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(db); err != nil {
		t.Fatal(err)
	}

	if _, err := db.ExecContext(context.Background(), `
		UPDATE mission_events
		SET type = 'tampered'
		WHERE mission_id = ?`, mission.ID,
	); err == nil {
		t.Fatal("mission Event update must be rejected after migration")
	}
	if _, err := db.ExecContext(context.Background(), `
		DELETE FROM mission_events
		WHERE mission_id = ?`, mission.ID,
	); err == nil {
		t.Fatal("mission Event delete must be rejected after migration")
	}
}

func TestTransitionAttemptRejectsTerminalTransition(t *testing.T) {
	store, attempt := newRunningAttempt(t)

	succeeded, err := store.TransitionAttempt(
		context.Background(),
		attempt.ID,
		AttemptSucceeded,
	)
	if err != nil {
		t.Fatal(err)
	}
	if succeeded.Status != AttemptSucceeded {
		t.Fatalf("status = %s, want %s", succeeded.Status, AttemptSucceeded)
	}
	if _, err := store.TransitionAttempt(
		context.Background(),
		attempt.ID,
		AttemptFailed,
	); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("TransitionAttempt error = %v, want ErrInvalidTransition", err)
	}
}

func TestStartAttemptRejectsExpiredLease(t *testing.T) {
	store, task := newReadyTask(t)
	now := time.Date(2026, time.July, 26, 10, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }
	if _, err := store.AcquireLease(
		context.Background(),
		task.ID,
		"wt/expired",
		"branch/expired",
		"sbx-expired",
		time.Minute,
	); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)

	if _, err := store.StartAttempt(
		context.Background(),
		task.ID,
		"test-worker",
	); !errors.Is(err, ErrConflict) {
		t.Fatalf("StartAttempt error = %v, want ErrConflict", err)
	}
}

func TestAddArtifactIsDigestIdempotentAndRetainsBlob(t *testing.T) {
	store, attempt := newRunningAttempt(t)
	task, err := store.GetTask(context.Background(), attempt.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	input := CreateArtifactInput{
		MissionID: task.MissionID,
		TaskID:    task.ID,
		AttemptID: attempt.ID,
		Kind:      "report",
		Content:   []byte{0x00, 0xff, 0x41, 0x00},
	}

	first, err := store.AddArtifact(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.AddArtifact(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID {
		t.Fatalf("duplicate Artifact ID = %q, want %q", second.ID, first.ID)
	}
	if !bytes.Equal(second.Content, input.Content) {
		t.Fatalf("Artifact content = %v, want %v", second.Content, input.Content)
	}

	var count int
	if err := store.db.QueryRowContext(context.Background(), `
		SELECT COUNT(*) FROM artifacts WHERE attempt_id = ?`, attempt.ID,
	).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("Artifact count = %d, want 1", count)
	}
	if count := eventTypeCount(t, store, task.MissionID, "artifact.created"); count != 1 {
		t.Fatalf("artifact.created event count = %d, want 1", count)
	}
}

func TestAddEvidenceSeparatesProducerAndVerifierAttempts(t *testing.T) {
	store, producer := newRunningAttempt(t)
	task, err := store.GetTask(context.Background(), producer.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := store.StartAttempt(context.Background(), task.ID, "verifier")
	if err != nil {
		t.Fatal(err)
	}
	input := CreateEvidenceInput{
		MissionID:         task.MissionID,
		TaskID:            task.ID,
		AttemptID:         producer.ID,
		VerifierAttemptID: verifier.ID,
		Kind:              "go_test",
		Content:           []byte("ok\tgithub.com/harness9/internal/mission"),
		Passed:            true,
	}

	evidence, err := store.AddEvidence(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.AttemptID != producer.ID ||
		evidence.VerifierAttemptID != verifier.ID {
		t.Fatalf("Evidence Attempts = %+v", evidence)
	}
	if count := eventTypeCount(t, store, task.MissionID, "evidence.created"); count != 1 {
		t.Fatalf("evidence.created event count = %d, want 1", count)
	}
	input.VerifierAttemptID = producer.ID
	input.Content = []byte("collision")
	if _, err := store.AddEvidence(
		context.Background(),
		input,
	); !errors.Is(err, ErrConflict) {
		t.Fatalf("AddEvidence collision error = %v, want ErrConflict", err)
	}
}

func TestListEventsOrdersByCreationThenIDAndSupportsCursor(t *testing.T) {
	store, task := newReadyTask(t)
	now := time.Date(2026, time.July, 26, 10, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }
	if _, err := store.AcquireLease(
		context.Background(),
		task.ID,
		"wt/events",
		"branch/events",
		"sbx-events",
		time.Hour,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.StartAttempt(
		context.Background(),
		task.ID,
		"test-worker",
	); err != nil {
		t.Fatal(err)
	}

	events, err := store.ListEvents(
		context.Background(),
		task.MissionID,
		"",
		100,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) < 3 {
		t.Fatalf("event count = %d, want at least 3", len(events))
	}
	for i := 1; i < len(events); i++ {
		if events[i].CreatedAt.Before(events[i-1].CreatedAt) ||
			(events[i].CreatedAt.Equal(events[i-1].CreatedAt) &&
				events[i].ID <= events[i-1].ID) {
			t.Fatalf("events are not ordered at %d: %+v", i, events)
		}
	}
	after, err := store.ListEvents(
		context.Background(),
		task.MissionID,
		events[0].ID,
		100,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(events)-1 || after[0].ID != events[1].ID {
		t.Fatalf("events after cursor = %+v, want IDs after %s", after, events[0].ID)
	}
	fromBeginning, err := store.ListEvents(
		context.Background(),
		task.MissionID,
		int64(0),
		100,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(fromBeginning) != len(events) {
		t.Fatalf("numeric zero cursor returned %d events, want %d", len(fromBeginning), len(events))
	}
}

func TestReleaseLeaseIsTerminalAndAllowsReplacement(t *testing.T) {
	store, task := newReadyTask(t)
	lease, err := store.AcquireLease(
		context.Background(),
		task.ID,
		"wt/first",
		"branch/first",
		"sbx-first",
		time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}

	released, err := store.ReleaseLease(context.Background(), lease.ID)
	if err != nil {
		t.Fatal(err)
	}
	if released.Status != "released" || released.ReleasedAt == nil {
		t.Fatalf("released Lease = %+v", released)
	}
	if released.Branch != lease.Branch || released.SandboxID != lease.SandboxID {
		t.Fatalf("released Lease metadata = %+v, want %+v", released, lease)
	}
	if count := eventTypeCount(t, store, task.MissionID, "lease.released"); count != 1 {
		t.Fatalf("lease.released event count = %d, want 1", count)
	}
	if _, err := store.AcquireLease(
		context.Background(),
		task.ID,
		"wt/replacement",
		"branch/replacement",
		"sbx-replacement",
		time.Hour,
	); err != nil {
		t.Fatalf("Acquire replacement Lease: %v", err)
	}
}

func TestReleaseExpiredLeasePersistsTerminalStatus(t *testing.T) {
	store, task := newReadyTask(t)
	now := time.Date(2026, time.July, 26, 10, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }
	lease, err := store.AcquireLease(
		context.Background(),
		task.ID,
		"wt/expired-release",
		"branch/expired-release",
		"sbx-expired-release",
		time.Minute,
	)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)

	released, err := store.ReleaseLease(context.Background(), lease.ID)
	if err != nil {
		t.Fatal(err)
	}
	if released.Status != "expired" || released.ReleasedAt == nil {
		t.Fatalf("expired Lease = %+v", released)
	}
	if count := eventTypeCount(t, store, task.MissionID, "lease.released"); count != 1 {
		t.Fatalf("lease.released event count = %d, want 1", count)
	}
}

func TestRecoveryDoesNotAffectTerminalAttemptsOrCreateRetry(t *testing.T) {
	store, attempt := newRunningAttempt(t)
	if _, err := store.TransitionAttempt(
		context.Background(),
		attempt.ID,
		AttemptSucceeded,
	); err != nil {
		t.Fatal(err)
	}

	count, err := store.MarkInterruptedAttemptsIndeterminate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("recovered Attempt count = %d, want 0", count)
	}
	got, err := store.GetAttempt(context.Background(), attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != AttemptSucceeded {
		t.Fatalf("terminal Attempt status = %s, want %s", got.Status, AttemptSucceeded)
	}
	var attemptCount int
	if err := store.db.QueryRowContext(context.Background(), `
		SELECT COUNT(*) FROM task_attempts WHERE task_id = ?`, attempt.TaskID,
	).Scan(&attemptCount); err != nil {
		t.Fatal(err)
	}
	if attemptCount != 1 {
		t.Fatalf("Attempt count after recovery = %d, want 1", attemptCount)
	}
}

func newReadyTask(t *testing.T) (*Store, Task) {
	t.Helper()
	store := newTestStore(t)
	ctx := context.Background()
	mission, err := store.CreateMission(ctx, CreateMissionInput{Goal: "test execution persistence"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, CreateTaskInput{
		MissionID: mission.ID,
		Title:     "execute persisted task",
	})
	if err != nil {
		t.Fatal(err)
	}
	return store, task
}

func newRunningAttempt(t *testing.T) (*Store, TaskAttempt) {
	t.Helper()
	store, task := newReadyTask(t)
	if _, err := store.AcquireLease(
		context.Background(),
		task.ID,
		"wt/recovery",
		"branch/recovery",
		"sbx-recovery",
		time.Hour,
	); err != nil {
		t.Fatal(err)
	}
	attempt, err := store.StartAttempt(context.Background(), task.ID, "test-worker")
	if err != nil {
		t.Fatal(err)
	}
	return store, attempt
}

func eventTypeCount(t *testing.T, store *Store, missionID, eventType string) int {
	t.Helper()
	var count int
	if err := store.db.QueryRowContext(context.Background(), `
		SELECT COUNT(*)
		FROM mission_events
		WHERE mission_id = ? AND type = ?`,
		missionID,
		eventType,
	).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}
