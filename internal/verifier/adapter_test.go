package verifier

import (
	"context"
	"testing"

	"github.com/harness9/internal/mission"
)

func TestVerifierProducesEvidence(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	m, _ := store.CreateMission(ctx, mission.CreateMissionInput{Goal: "ship"})
	task, _ := store.CreateTask(ctx, mission.CreateTaskInput{MissionID: m.ID, Title: "verify"})
	attempt, _ := store.StartAttempt(ctx, task.ID, "verifier")

	checks := []Check{
		{"echo1", []string{"echo", "hello"}},
		{"echo2", []string{"echo", "world"}},
	}
	adapter := NewAdapterWithChecks(store, ".", checks)
	result, err := adapter.Dispatch(ctx, task, attempt)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "succeeded" {
		t.Fatalf("status = %q, want succeeded", result.Status)
	}
	evidence, _ := store.ListEvidence(ctx, task.ID)
	if len(evidence) != 2 {
		t.Fatalf("evidence count = %d, want 2", len(evidence))
	}
	for _, e := range evidence {
		if !e.Passed {
			t.Fatalf("evidence %s did not pass", e.Kind)
		}
	}
}

func TestVerifierFailedCheck(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	m, _ := store.CreateMission(ctx, mission.CreateMissionInput{Goal: "ship"})
	task, _ := store.CreateTask(ctx, mission.CreateTaskInput{MissionID: m.ID, Title: "verify"})
	attempt, _ := store.StartAttempt(ctx, task.ID, "verifier")

	checks := []Check{
		{"pass", []string{"echo", "ok"}},
		{"fail", []string{"false"}}, // always exits non-zero
	}
	adapter := NewAdapterWithChecks(store, ".", checks)
	result, _ := adapter.Dispatch(ctx, task, attempt)
	if result.Status != "failed" {
		t.Fatalf("status = %q, want failed", result.Status)
	}
	evidence, _ := store.ListEvidence(ctx, task.ID)
	if len(evidence) != 2 {
		t.Fatalf("evidence count = %d, want 2", len(evidence))
	}
	foundFail := false
	for _, e := range evidence {
		if e.Kind == "fail" && !e.Passed {
			foundFail = true
		}
	}
	if !foundFail {
		t.Fatal("expected failing evidence")
	}
}

func newTestStore(t *testing.T) *mission.Store {
	t.Helper()
	return newTestStoreImpl(t)
}
