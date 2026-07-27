package mission

import (
	"context"
	"errors"
	"testing"
)

func TestListSchedulableTasksReturnsQueuedRootTaskForApprovedPlan(t *testing.T) {
	store, mission := newStoreWithMission(t)
	plan, err := store.CreateDraftPlan(context.Background(), mission.ID, samplePlanInput(), "coordinator")
	if err != nil {
		t.Fatal(err)
	}
	if err := markPlanApprovedForTest(context.Background(), store, mission.ID, plan.Version); err != nil {
		t.Fatal(err)
	}

	tasks, err := store.ListSchedulableTasks(context.Background())
	if err != nil {
		t.Fatalf("ListSchedulableTasks: %v", err)
	}
	if len(tasks) != 1 || tasks[0].ClientID != "spec" || tasks[0].Status != TaskQueued {
		t.Fatalf("schedulable tasks = %+v, want only the queued root task \"spec\"", tasks)
	}
}

func TestListSchedulableTasksExcludesSupersededPlanVersion(t *testing.T) {
	store, mission := newApprovedRunningMission(t)
	v1Tasks, err := store.ListSchedulableTasks(context.Background())
	if err != nil {
		t.Fatalf("ListSchedulableTasks (v1): %v", err)
	}
	if len(v1Tasks) != 1 {
		t.Fatalf("v1 schedulable tasks = %+v, want exactly the root \"spec\" task", v1Tasks)
	}
	v1SpecTaskID := v1Tasks[0].ID

	request, err := store.createPlanChangeRequest(
		context.Background(), mission.ID, mission.CurrentPlanVersion, revisedPlanInput(), "missing docs", "coordinator",
	)
	if err != nil {
		t.Fatal(err)
	}
	svc := NewCommandService(store)
	if _, err := svc.ResolvePlanChange(context.Background(), ResolvePlanChangeCommand{
		MissionID:       mission.ID,
		ChangeRequestID: request.ID,
		Plan:            revisedPlanInput(),
		Approve:         true,
		Actor:           "user:zsa",
		Reason:          "approve v2",
		IdempotencyKey:  "resolve-v2",
	}); err != nil {
		t.Fatal(err)
	}

	v2Tasks, err := store.ListSchedulableTasks(context.Background())
	if err != nil {
		t.Fatalf("ListSchedulableTasks (v2): %v", err)
	}
	for _, task := range v2Tasks {
		if task.ID == v1SpecTaskID {
			t.Fatalf("schedulable tasks after the plan change still include the superseded v1 task: %+v", task)
		}
	}
	foundV2Root := false
	for _, task := range v2Tasks {
		if task.ClientID == "docs" {
			foundV2Root = true
		}
	}
	if !foundV2Root {
		t.Fatalf("v2 schedulable tasks = %+v, want the new version's \"docs\" root task", v2Tasks)
	}
}

func TestActiveTaskCountsGroupsLeasedAndRunningByMission(t *testing.T) {
	store, attempt := newRunningAttempt(t)
	task, err := store.GetTask(context.Background(), attempt.TaskID)
	if err != nil {
		t.Fatal(err)
	}

	perMission, total, err := store.ActiveTaskCounts(context.Background())
	if err != nil {
		t.Fatalf("ActiveTaskCounts: %v", err)
	}
	if perMission[task.MissionID] != 1 || total != 1 {
		t.Fatalf("ActiveTaskCounts = %+v/%d, want 1/1 for mission %s", perMission, total, task.MissionID)
	}
}

func TestMarkMissionRunningTransitionsReadyToRunningIdempotently(t *testing.T) {
	store, mission := newStoreWithMission(t)
	plan, err := store.CreateDraftPlan(context.Background(), mission.ID, samplePlanInput(), "coordinator")
	if err != nil {
		t.Fatal(err)
	}
	if err := markPlanApprovedForTest(context.Background(), store, mission.ID, plan.Version); err != nil {
		t.Fatal(err)
	}

	got, err := store.MarkMissionRunning(context.Background(), mission.ID)
	if err != nil {
		t.Fatalf("MarkMissionRunning: %v", err)
	}
	if got.Status != MissionRunning {
		t.Fatalf("Status = %s, want running", got.Status)
	}

	again, err := store.MarkMissionRunning(context.Background(), mission.ID)
	if err != nil {
		t.Fatalf("MarkMissionRunning (repeat call): %v", err)
	}
	if again.Status != MissionRunning {
		t.Fatalf("Status = %s, want running on repeat call", again.Status)
	}
}

func TestMarkMissionRunningRejectsMissionNotReady(t *testing.T) {
	store, mission := newStoreWithMission(t)
	if _, err := store.MarkMissionRunning(context.Background(), mission.ID); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("MarkMissionRunning on draft mission: err = %v, want ErrInvalidTransition", err)
	}
}

func TestListSchedulableTasksIncludesContractKind(t *testing.T) {
	store, mission := newStoreWithMission(t)
	plan, err := store.CreateDraftPlan(context.Background(), mission.ID, PlanInput{Tasks: []TaskInput{
		{ClientID: "a", Position: 1, Title: "A", Contract: "do A", ContractKind: ContractVerification},
	}}, "coordinator")
	if err != nil {
		t.Fatal(err)
	}
	if err := markPlanApprovedForTest(context.Background(), store, mission.ID, plan.Version); err != nil {
		t.Fatal(err)
	}
	tasks, err := store.ListSchedulableTasks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].ContractKind != ContractVerification {
		t.Fatalf("schedulable tasks = %+v, want one task with ContractKind %q", tasks, ContractVerification)
	}
}
