package mission

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestAcquireLeaseExclusive(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	m, _ := store.CreateMission(ctx, CreateMissionInput{Goal: "ship"})
	task, _ := store.CreateTask(ctx, CreateTaskInput{MissionID: m.ID, Title: "impl"})
	lease, err := store.AcquireLease(ctx, task.ID, "/tmp/wt", "mission/branch", "sandbox-1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Status != LeaseActive {
		t.Fatalf("status = %q, want active", lease.Status)
	}
	if lease.Branch != "mission/branch" {
		t.Fatalf("branch = %q, want %q", lease.Branch, "mission/branch")
	}
	if lease.SandboxID != "sandbox-1" {
		t.Fatalf("sandbox_id = %q, want %q", lease.SandboxID, "sandbox-1")
	}
	if lease.ExpiresAt.IsZero() {
		t.Fatal("expires_at is zero")
	}
	if _, err := store.AcquireLease(ctx, task.ID, "/tmp/wt2", "branch2", "sandbox-2", time.Hour); err == nil {
		t.Fatal("expected duplicate lease to fail")
	}
}

func TestGetActiveLeaseRoundTrip(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	m, _ := store.CreateMission(ctx, CreateMissionInput{Goal: "ship"})
	task, _ := store.CreateTask(ctx, CreateTaskInput{MissionID: m.ID, Title: "impl"})
	original, _ := store.AcquireLease(ctx, task.ID, "/tmp/wt", "mission/branch", "sandbox-1", time.Hour)

	got, err := store.GetActiveLease(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != original.ID {
		t.Fatalf("id = %q, want %q", got.ID, original.ID)
	}
	if got.Path != "/tmp/wt" {
		t.Fatalf("path = %q, want %q", got.Path, "/tmp/wt")
	}
	if got.Branch != "mission/branch" {
		t.Fatalf("branch = %q, want %q", got.Branch, "mission/branch")
	}
	if got.SandboxID != "sandbox-1" {
		t.Fatalf("sandbox_id = %q, want %q", got.SandboxID, "sandbox-1")
	}
	if got.Status != LeaseActive {
		t.Fatalf("status = %q, want %q", got.Status, LeaseActive)
	}
}

func TestGetActiveLeaseNotFound(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	m, _ := store.CreateMission(ctx, CreateMissionInput{Goal: "ship"})
	task, _ := store.CreateTask(ctx, CreateTaskInput{MissionID: m.ID, Title: "impl"})
	_, err := store.GetActiveLease(ctx, task.ID)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestReleaseLease(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	m, _ := store.CreateMission(ctx, CreateMissionInput{Goal: "ship"})
	task, _ := store.CreateTask(ctx, CreateTaskInput{MissionID: m.ID, Title: "impl"})
	lease, _ := store.AcquireLease(ctx, task.ID, "/p", "b", "s", time.Hour)
	if err := store.ReleaseLease(ctx, lease.ID); err != nil {
		t.Fatal(err)
	}
	active, err := store.GetActiveLease(ctx, task.ID)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("after release, GetActiveLease err = %v, want ErrNotFound", err)
	}
	_ = active
}

func TestReleaseLeaseNotFound(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	if err := store.ReleaseLease(ctx, "nonexistent"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestReleaseLeaseIdempotentGuard(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	m, _ := store.CreateMission(ctx, CreateMissionInput{Goal: "ship"})
	task, _ := store.CreateTask(ctx, CreateTaskInput{MissionID: m.ID, Title: "impl"})
	lease, _ := store.AcquireLease(ctx, task.ID, "/p", "b", "s", time.Hour)
	if err := store.ReleaseLease(ctx, lease.ID); err != nil {
		t.Fatal(err)
	}
	// Releasing again must fail because status is no longer active.
	if err := store.ReleaseLease(ctx, lease.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second release err = %v, want ErrNotFound", err)
	}
}

func TestAcquireLeaseAfterRelease(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	m, _ := store.CreateMission(ctx, CreateMissionInput{Goal: "ship"})
	task, _ := store.CreateTask(ctx, CreateTaskInput{MissionID: m.ID, Title: "impl"})
	lease, _ := store.AcquireLease(ctx, task.ID, "/p", "b", "s", time.Hour)
	_ = store.ReleaseLease(ctx, lease.ID)
	if _, err := store.AcquireLease(ctx, task.ID, "/p2", "b2", "s2", time.Hour); err != nil {
		t.Fatalf("re-acquire after release: %v", err)
	}
}

func TestExpireLeases(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	m, _ := store.CreateMission(ctx, CreateMissionInput{Goal: "ship"})
	task, _ := store.CreateTask(ctx, CreateTaskInput{MissionID: m.ID, Title: "impl"})
	// TTL of 1 nanosecond so it is already expired by the time ExpireLeases runs.
	lease, _ := store.AcquireLease(ctx, task.ID, "/p", "b", "s", time.Nanosecond)
	time.Sleep(2 * time.Millisecond)

	count, err := store.ExpireLeases(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expired count = %d, want 1", count)
	}
	_, err = store.GetActiveLease(ctx, task.ID)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("after expire, GetActiveLease err = %v, want ErrNotFound", err)
	}
	_ = lease
}

func TestExpireLeasesSkipsActive(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	m, _ := store.CreateMission(ctx, CreateMissionInput{Goal: "ship"})
	task, _ := store.CreateTask(ctx, CreateTaskInput{MissionID: m.ID, Title: "impl"})
	_, _ = store.AcquireLease(ctx, task.ID, "/p", "b", "s", time.Hour)

	count, err := store.ExpireLeases(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("expired count = %d, want 0", count)
	}
	if _, err := store.GetActiveLease(ctx, task.ID); err != nil {
		t.Fatalf("active lease should still exist: %v", err)
	}
}

func TestAddAndListAuditEvents(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	m, _ := store.CreateMission(ctx, CreateMissionInput{Goal: "ship"})
	_, err := store.AddAuditEvent(ctx, AuditEvent{
		MissionID: m.ID, CommandKind: "approve_plan", Actor: "operator",
		Result: "applied", IdempotencyKey: "key-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	events, err := store.ListAuditEvents(ctx, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
}

func TestListAuditEventsChronological(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	m, _ := store.CreateMission(ctx, CreateMissionInput{Goal: "ship"})
	for i := 0; i < 3; i++ {
		_, err := store.AddAuditEvent(ctx, AuditEvent{
			MissionID: m.ID, CommandKind: "approve_plan", Actor: "operator", Result: "applied",
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	events, err := store.ListAuditEvents(ctx, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("events = %d, want 3", len(events))
	}
	for i := 1; i < len(events); i++ {
		if events[i].CreatedAt.Before(events[i-1].CreatedAt) {
			t.Fatalf("event %d before event %d", i, i-1)
		}
	}
}

func TestAddAuditEventValidation(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	m, _ := store.CreateMission(ctx, CreateMissionInput{Goal: "ship"})
	cases := []struct {
		name  string
		event AuditEvent
	}{
		{"missing mission id", AuditEvent{CommandKind: "approve_plan", Actor: "operator", Result: "applied"}},
		{"missing command kind", AuditEvent{MissionID: m.ID, Actor: "operator", Result: "applied"}},
		{"missing actor", AuditEvent{MissionID: m.ID, CommandKind: "approve_plan", Result: "applied"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := store.AddAuditEvent(ctx, c.event); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestFindAuditEventByIdempotencyKey(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	m, _ := store.CreateMission(ctx, CreateMissionInput{Goal: "ship"})
	_, _ = store.AddAuditEvent(ctx, AuditEvent{
		MissionID: m.ID, CommandKind: "approve_plan", Actor: "operator",
		Result: "applied", IdempotencyKey: "key-1",
	})
	found, ok, err := store.FindAuditEventByIdempotencyKey(ctx, m.ID, "key-1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected to find event by idempotency key")
	}
	if found.IdempotencyKey != "key-1" {
		t.Fatalf("idempotency key = %q, want %q", found.IdempotencyKey, "key-1")
	}
}

func TestFindAuditEventByIdempotencyKeyNotFound(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	m, _ := store.CreateMission(ctx, CreateMissionInput{Goal: "ship"})
	_, ok, err := store.FindAuditEventByIdempotencyKey(ctx, m.ID, "missing-key")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected not found, got ok=true")
	}
}

func TestListAuditEventsEmpty(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	m, _ := store.CreateMission(ctx, CreateMissionInput{Goal: "ship"})
	events, err := store.ListAuditEvents(ctx, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if events != nil && len(events) != 0 {
		t.Fatalf("events = %v, want empty", events)
	}
}
