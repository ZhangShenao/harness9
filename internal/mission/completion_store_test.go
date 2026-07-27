package mission

import (
	"context"
	"errors"
	"testing"
)

func TestMarkMissionNeedsAttentionTransitionsRunningMissionIdempotently(t *testing.T) {
	store, mission := newStoreWithMission(t)
	plan, err := store.CreateDraftPlan(context.Background(), mission.ID, samplePlanInput(), "coordinator")
	if err != nil {
		t.Fatal(err)
	}
	if err := markPlanApprovedForTest(context.Background(), store, mission.ID, plan.Version); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkMissionRunning(context.Background(), mission.ID); err != nil {
		t.Fatal(err)
	}

	got, err := store.MarkMissionNeedsAttention(context.Background(), mission.ID, "integration merge conflict")
	if err != nil {
		t.Fatalf("MarkMissionNeedsAttention: %v", err)
	}
	if got.Status != MissionNeedsAttention {
		t.Fatalf("status = %s, want needs_attention", got.Status)
	}

	again, err := store.MarkMissionNeedsAttention(context.Background(), mission.ID, "integration merge conflict")
	if err != nil {
		t.Fatalf("MarkMissionNeedsAttention (repeat): %v", err)
	}
	if again.Status != MissionNeedsAttention {
		t.Fatalf("status on repeat = %s, want needs_attention", again.Status)
	}
}

func TestMarkMissionNeedsAttentionRejectsMissionNotYetRunning(t *testing.T) {
	store, mission := newStoreWithMission(t)
	if _, err := store.MarkMissionNeedsAttention(context.Background(), mission.ID, "too early"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("MarkMissionNeedsAttention on draft mission: err = %v, want ErrInvalidTransition", err)
	}
}
