package mission

import (
	"context"
	"testing"
)

func TestSubmitAndApprovePlanViaCommand(t *testing.T) {
	store := newTestStore(t)
	cs := NewCommandService(store)
	ctx := context.Background()

	m, _ := store.CreateMission(ctx, CreateMissionInput{Goal: "ship feature"})

	submitRes := cs.Execute(ctx, Command{
		Kind: CmdSubmitPlanDraft, Actor: "coordinator", Target: m.ID,
		IdempotencyKey: "submit-1", Payload: []byte(`[{"kind":"implementation","goal":"x"}]`),
	})
	if !submitRes.Applied {
		t.Fatalf("submit not applied: %v", submitRes.Error)
	}

	plan, err := store.GetPlan(ctx, submitRes.Event.Target)
	if err != nil {
		t.Fatal(err)
	}

	approveRes := cs.Execute(ctx, Command{
		Kind: CmdApprovePlan, Actor: "operator", Target: plan.ID,
		IdempotencyKey: "approve-1", Reason: "looks good",
	})
	if !approveRes.Applied {
		t.Fatalf("approve not applied: %v", approveRes.Error)
	}
}

func TestCommandIdempotency(t *testing.T) {
	store := newTestStore(t)
	cs := NewCommandService(store)
	ctx := context.Background()
	m, _ := store.CreateMission(ctx, CreateMissionInput{Goal: "ship feature"})

	cmd := Command{
		Kind: CmdSubmitPlanDraft, Actor: "coordinator", Target: m.ID,
		IdempotencyKey: "dup-key", Payload: []byte(`[]`),
	}
	first := cs.Execute(ctx, cmd)
	if !first.Applied {
		t.Fatal("first should apply")
	}
	second := cs.Execute(ctx, cmd)
	if second.Applied {
		t.Fatal("second should not apply (idempotent)")
	}
	if second.Event.ID != first.Event.ID {
		t.Fatal("should return same audit event")
	}
}

func TestRejectPlanViaCommand(t *testing.T) {
	store := newTestStore(t)
	cs := NewCommandService(store)
	ctx := context.Background()
	m, _ := store.CreateMission(ctx, CreateMissionInput{Goal: "ship feature"})
	submitRes := cs.Execute(ctx, Command{
		Kind: CmdSubmitPlanDraft, Actor: "coordinator", Target: m.ID,
		IdempotencyKey: "s1", Payload: []byte(`[]`),
	})
	plan, _ := store.GetPlan(ctx, submitRes.Event.Target)
	rejectRes := cs.Execute(ctx, Command{
		Kind: CmdRejectPlan, Actor: "operator", Target: plan.ID,
		IdempotencyKey: "r1", Reason: "missing tests",
	})
	if !rejectRes.Applied {
		t.Fatalf("reject not applied: %v", rejectRes.Error)
	}
}
