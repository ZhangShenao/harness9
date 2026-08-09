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

func TestPauseAndResumeMission(t *testing.T) {
	store := newTestStore(t)
	cs := NewCommandService(store)
	ctx := context.Background()
	m, _ := store.CreateMission(ctx, CreateMissionInput{Goal: "ship feature"})
	// move to running first
	store.db.ExecContext(ctx, `UPDATE missions SET status = ? WHERE id = ?`, MissionRunning, m.ID)

	pauseRes := cs.Execute(ctx, Command{
		Kind: CmdPauseMission, Actor: "operator", Target: m.ID,
		IdempotencyKey: "pause-1", Reason: "investigating",
	})
	if !pauseRes.Applied {
		t.Fatalf("pause not applied: %v", pauseRes.Error)
	}
	// pause maps to needs_attention
	updated, _ := cs.getMission(ctx, m.ID)
	if updated.Status != MissionNeedsAttention {
		t.Fatalf("status = %q, want needs_attention", updated.Status)
	}

	resumeRes := cs.Execute(ctx, Command{
		Kind: CmdResumeMission, Actor: "operator", Target: m.ID,
		IdempotencyKey: "resume-1", Reason: "resolved",
	})
	if !resumeRes.Applied {
		t.Fatalf("resume not applied: %v", resumeRes.Error)
	}
	updated, _ = cs.getMission(ctx, m.ID)
	if updated.Status != MissionRunning {
		t.Fatalf("status = %q, want running", updated.Status)
	}
}

func TestCancelMissionFromRunning(t *testing.T) {
	store := newTestStore(t)
	cs := NewCommandService(store)
	ctx := context.Background()
	m, _ := store.CreateMission(ctx, CreateMissionInput{Goal: "ship feature"})
	store.db.ExecContext(ctx, `UPDATE missions SET status = ? WHERE id = ?`, MissionRunning, m.ID)
	res := cs.Execute(ctx, Command{
		Kind: CmdCancelMission, Actor: "operator", Target: m.ID,
		IdempotencyKey: "cancel-1",
	})
	if !res.Applied {
		t.Fatalf("cancel not applied: %v", res.Error)
	}
	updated, _ := cs.getMission(ctx, m.ID)
	if updated.Status != MissionCancelled {
		t.Fatalf("status = %q, want cancelled", updated.Status)
	}
}
