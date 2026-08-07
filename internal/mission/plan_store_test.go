package mission

import (
	"context"
	"testing"
)

func TestCreatePlanDraft(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	m, _ := store.CreateMission(ctx, CreateMissionInput{Goal: "ship feature"})
	plan, err := store.CreatePlan(ctx, m.ID, `[{"kind":"implementation","goal":"write code"}]`)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != PlanDraft || plan.Version != 1 {
		t.Fatalf("plan = %+v", plan)
	}
}

func TestApprovePlanCreatesVersion(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	m, _ := store.CreateMission(ctx, CreateMissionInput{Goal: "ship feature"})
	plan, _ := store.CreatePlan(ctx, m.ID, `[{"kind":"implementation","goal":"x"}]`)
	pv, err := store.ApprovePlan(ctx, plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pv.PlanID != plan.ID || pv.Version != 1 {
		t.Fatalf("plan version = %+v", pv)
	}
	got, err := store.GetPlan(ctx, plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != PlanApproved {
		t.Fatalf("plan status = %q, want approved", got.Status)
	}
	active, err := store.GetActivePlanVersion(ctx, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if active.ID != pv.ID {
		t.Fatalf("active version = %q, want %q", active.ID, pv.ID)
	}
}

func TestApproveSecondPlanSupersedesFirst(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	m, _ := store.CreateMission(ctx, CreateMissionInput{Goal: "ship feature"})
	p1, _ := store.CreatePlan(ctx, m.ID, `[]`)
	store.ApprovePlan(ctx, p1.ID)
	p2, _ := store.CreatePlan(ctx, m.ID, `[]`)
	pv2, _ := store.ApprovePlan(ctx, p2.ID)
	active, _ := store.GetActivePlanVersion(ctx, m.ID)
	if active.ID != pv2.ID {
		t.Fatalf("active = %q, want %q", active.ID, pv2.ID)
	}
	old, _ := store.GetPlan(ctx, p1.ID)
	if old.Status != PlanSuperseded {
		t.Fatalf("old plan status = %q, want superseded", old.Status)
	}
}

func TestCreateAndReviewChangeRequest(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	m, _ := store.CreateMission(ctx, CreateMissionInput{Goal: "ship feature"})
	cr, err := store.CreateChangeRequest(ctx, PlanChangeRequest{
		MissionID: m.ID, Reason: "need extra task", ProposedPlanJSON: `[]`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if cr.Status != ChangePending {
		t.Fatalf("status = %q, want pending", cr.Status)
	}
	reviewed, err := store.ReviewChangeRequest(ctx, cr.ID, ChangeApproved, "operator", "looks good")
	if err != nil {
		t.Fatal(err)
	}
	if reviewed.Status != ChangeApproved || reviewed.ReviewedBy != "operator" {
		t.Fatalf("reviewed = %+v", reviewed)
	}
}

func TestPolicyRoundTrip(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	m, _ := store.CreateMission(ctx, CreateMissionInput{Goal: "ship feature"})
	p := DefaultPolicy()
	p.MissionConcurrency = 3
	if err := store.SetPolicy(ctx, m.ID, p); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetPolicy(ctx, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.MissionConcurrency != 3 {
		t.Fatalf("concurrency = %d, want 3", got.MissionConcurrency)
	}
}
