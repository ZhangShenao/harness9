package dataset

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/harness9/internal/evals"
	"github.com/harness9/internal/mission"
	_ "modernc.org/sqlite"
)

func newMissionStore(t *testing.T) *mission.Store {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "mission.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store, err := mission.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// TestMissionFoundationPlanVersioning verifies that a Plan goes through
// draft -> approved -> superseded lifecycle correctly, and that only
// the active PlanVersion is schedulable.
func TestMissionFoundationPlanVersioning(t *testing.T) {
	evals.SetupHermeticEnv(t)
	store := newMissionStore(t)
	ctx := context.Background()
	cs := mission.NewCommandService(store)

	m, _ := store.CreateMission(ctx, mission.CreateMissionInput{Goal: "ship feature"})

	// Submit and approve plan v1
	r1 := cs.Execute(ctx, mission.Command{
		Kind: mission.CmdSubmitPlanDraft, Actor: "coordinator", Target: m.ID,
		IdempotencyKey: "submit-1", Payload: []byte(`[{"kind":"implementation","goal":"x"}]`),
	})
	if !r1.Applied {
		t.Fatal("submit not applied")
	}
	plan1, _ := store.GetPlan(ctx, r1.Event.Target)
	r2 := cs.Execute(ctx, mission.Command{
		Kind: mission.CmdApprovePlan, Actor: "operator", Target: plan1.ID,
		IdempotencyKey: "approve-1",
	})
	if !r2.Applied {
		t.Fatal("approve not applied")
	}
	pv1, _ := store.GetActivePlanVersion(ctx, m.ID)
	if pv1.Version != 1 {
		t.Fatalf("v1 version = %d, want 1", pv1.Version)
	}

	// Submit and approve plan v2 -> should supersede v1
	r3 := cs.Execute(ctx, mission.Command{
		Kind: mission.CmdSubmitPlanDraft, Actor: "coordinator", Target: m.ID,
		IdempotencyKey: "submit-2", Payload: []byte(`[]`),
	})
	plan2, _ := store.GetPlan(ctx, r3.Event.Target)
	cs.Execute(ctx, mission.Command{
		Kind: mission.CmdApprovePlan, Actor: "operator", Target: plan2.ID,
		IdempotencyKey: "approve-2",
	})
	pv2, _ := store.GetActivePlanVersion(ctx, m.ID)
	if pv2.Version != 2 {
		t.Fatalf("v2 version = %d, want 2", pv2.Version)
	}
	oldPlan, _ := store.GetPlan(ctx, plan1.ID)
	if oldPlan.Status != mission.PlanSuperseded {
		t.Fatalf("old plan status = %q, want superseded", oldPlan.Status)
	}
}

// TestMissionFoundationCommandIdempotency verifies that duplicate
// commands with the same IdempotencyKey do not double-apply.
func TestMissionFoundationCommandIdempotency(t *testing.T) {
	evals.SetupHermeticEnv(t)
	store := newMissionStore(t)
	ctx := context.Background()
	cs := mission.NewCommandService(store)

	m, _ := store.CreateMission(ctx, mission.CreateMissionInput{Goal: "ship"})

	cmd := mission.Command{
		Kind: mission.CmdSubmitPlanDraft, Actor: "coordinator", Target: m.ID,
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

// TestMissionFoundationChangeRequestGating verifies that unapproved
// PlanChangeRequests do not affect schedulable state.
func TestMissionFoundationChangeRequestGating(t *testing.T) {
	evals.SetupHermeticEnv(t)
	store := newMissionStore(t)
	ctx := context.Background()

	m, _ := store.CreateMission(ctx, mission.CreateMissionInput{Goal: "ship"})
	plan, _ := store.CreatePlan(ctx, m.ID, `[]`)
	store.ApprovePlan(ctx, plan.ID)
	store.CreateTask(ctx, mission.CreateTaskInput{MissionID: m.ID, Title: "impl"})

	// Create a pending change request
	cr, err := store.CreateChangeRequest(ctx, mission.PlanChangeRequest{
		MissionID: m.ID, Reason: "need extra task", ProposedPlanJSON: `[]`,
	})
	if err != nil {
		t.Fatalf("create change request: %v", err)
	}
	if cr.Status != mission.ChangePending {
		t.Fatalf("CR status = %q, want pending", cr.Status)
	}

	// The pending CR should not affect schedulable tasks
	tasks, _ := store.ListSchedulableTasks(ctx)
	if len(tasks) != 1 {
		t.Fatalf("schedulable tasks = %d, want 1 (CR should not affect)", len(tasks))
	}

	// Reject the CR
	pending, _ := store.ListPendingChangeRequests(ctx, m.ID)
	if len(pending) != 1 {
		t.Fatalf("pending CRs = %d, want 1", len(pending))
	}
}
