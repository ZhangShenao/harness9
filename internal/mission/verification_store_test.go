package mission

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestGetLatestLeaseReturnsMostRecentLease(t *testing.T) {
	store, task := newReadyTask(t)
	if _, err := store.AcquireLease(context.Background(), task.ID, "wt/a", "branch/a", "sbx-a", time.Hour); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetLatestLease(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("GetLatestLease: %v", err)
	}
	if got.Path != "wt/a" || got.Branch != "branch/a" {
		t.Fatalf("lease = %+v, want path=wt/a branch=branch/a", got)
	}
}

func TestGetLatestLeaseRejectsUnknownTask(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.GetLatestLease(context.Background(), "does-not-exist"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetLatestLease on unknown task: err = %v, want ErrNotFound", err)
	}
}

func TestGetLatestAttemptReturnsMostRecentAttempt(t *testing.T) {
	store, attempt := newRunningAttempt(t)
	got, err := store.GetLatestAttempt(context.Background(), attempt.TaskID)
	if err != nil {
		t.Fatalf("GetLatestAttempt: %v", err)
	}
	if got.ID != attempt.ID {
		t.Fatalf("attempt.ID = %s, want %s", got.ID, attempt.ID)
	}
}

func TestGetLatestAttemptRejectsUnknownTask(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.GetLatestAttempt(context.Background(), "does-not-exist"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetLatestAttempt on unknown task: err = %v, want ErrNotFound", err)
	}
}
