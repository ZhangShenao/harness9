package mission

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestApprovePlanIsIdempotentAndQueuesRootTasks(t *testing.T) {
	svc, store, mission, draft := newDraftMissionService(t)
	cmd := ApprovePlanCommand{
		MissionID:      mission.ID,
		Version:        draft.Version,
		Actor:          "user:zsa",
		Reason:         "计划已审阅",
		IdempotencyKey: "approve-v1",
	}

	first, err := svc.ApprovePlan(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.ApprovePlan(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID {
		t.Fatalf("duplicate approval returned Plan %q, want %q", second.ID, first.ID)
	}

	tasks, err := store.ListTasks(context.Background(), mission.ID)
	if err != nil {
		t.Fatal(err)
	}
	statusByClientID := make(map[string]TaskStatus, len(tasks))
	for _, task := range tasks {
		statusByClientID[task.ClientID] = task.Status
	}
	if statusByClientID["spec"] != TaskQueued ||
		statusByClientID["code"] != TaskBlocked {
		t.Fatalf("root/dependent Task states = %v", statusByClientID)
	}

	events, err := store.ListEvents(context.Background(), mission.ID, nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	if got := countEventsOfType(events, "plan.approved"); got != 1 {
		t.Fatalf("plan.approved Event count = %d, want 1", got)
	}
}

func TestResolveApprovedChangeCreatesNewVersion(t *testing.T) {
	svc, store, mission, request := newPendingChangeService(t)

	cmd := ResolvePlanChangeCommand{
		MissionID:       mission.ID,
		ChangeRequestID: request.ID,
		Approve:         true,
		Actor:           "user:zsa",
		Reason:          "补齐文档",
		IdempotencyKey:  "change-7",
	}
	plan, err := svc.ResolvePlanChange(
		context.Background(),
		cmd,
	)
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := svc.ResolvePlanChange(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	if duplicate.ID != plan.ID {
		t.Fatalf("duplicate resolution returned Plan %q, want %q", duplicate.ID, plan.ID)
	}
	if plan.Version != request.BaseVersion+1 {
		t.Fatalf("approved Plan version = %d, want %d", plan.Version, request.BaseVersion+1)
	}
	if plan.Status != PlanApproved || plan.ApprovedAt == nil {
		t.Fatalf("approved replacement Plan = %+v", plan)
	}

	old, err := store.GetPlan(context.Background(), mission.ID, request.BaseVersion)
	if err != nil {
		t.Fatal(err)
	}
	if old.Status != PlanApproved {
		t.Fatalf("old Plan status = %s, want %s", old.Status, PlanApproved)
	}
	gotMission, err := store.GetMission(context.Background(), mission.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotMission.CurrentPlanVersion != plan.Version {
		t.Fatalf("current Plan version = %d, want %d", gotMission.CurrentPlanVersion, plan.Version)
	}
	tasks, err := store.ListTasks(context.Background(), mission.ID)
	if err != nil {
		t.Fatal(err)
	}
	statusByClientID := make(map[string]TaskStatus)
	for _, task := range tasks {
		if task.ID != "" && task.ClientID != "" && task.Position > 0 &&
			task.Status != TaskSucceeded {
			statusByClientID[task.ClientID] = task.Status
		}
	}
	if statusByClientID["spec"] != TaskQueued ||
		statusByClientID["docs"] != TaskQueued ||
		statusByClientID["code"] != TaskBlocked {
		t.Fatalf("replacement root/dependent Task states = %v", statusByClientID)
	}
	resolved, err := store.GetPlanChangeRequest(context.Background(), request.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Status != ChangeRequestApproved ||
		resolved.ResolutionReason != "补齐文档" ||
		resolved.ResolvedAt == nil {
		t.Fatalf("approved Change Request = %+v", resolved)
	}
	events, err := store.ListEvents(context.Background(), mission.ID, nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	if got := countEventsOfType(events, "plan_change.approved"); got != 1 {
		t.Fatalf("plan_change.approved Event count = %d, want 1", got)
	}
	if got := countEventsOfType(events, "plan.version_activated"); got != 1 {
		t.Fatalf("plan.version_activated Event count = %d, want 1", got)
	}
}

func TestSubmitPlanChangeIsIdempotentAndKeepsActiveGraph(t *testing.T) {
	store, mission := newApprovedRunningMission(t)
	svc := NewCommandService(store)
	cmd := SubmitPlanChangeCommand{
		MissionID:      mission.ID,
		BaseVersion:    mission.CurrentPlanVersion,
		ProposedPlan:   revisedPlanInput(),
		Actor:          "coordinator",
		Reason:         "补齐迁移文档",
		IdempotencyKey: "submit-7",
	}

	first, err := svc.SubmitPlanChange(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.SubmitPlanChange(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID || first.Status != ChangeRequestPending {
		t.Fatalf("duplicate submit results = %+v, %+v", first, second)
	}
	gotMission, err := store.GetMission(context.Background(), mission.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotMission.CurrentPlanVersion != mission.CurrentPlanVersion {
		t.Fatalf(
			"SubmitPlanChange switched current version to %d, want %d",
			gotMission.CurrentPlanVersion,
			mission.CurrentPlanVersion,
		)
	}
	events, err := store.ListEvents(context.Background(), mission.ID, nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	if got := countEventsOfType(events, "plan_change.requested"); got != 1 {
		t.Fatalf("plan_change.requested Event count = %d, want 1", got)
	}
}

func TestResolveRejectedChangeIsIdempotentAndKeepsBaseVersion(t *testing.T) {
	svc, store, mission, request := newPendingChangeService(t)
	cmd := ResolvePlanChangeCommand{
		MissionID:       mission.ID,
		ChangeRequestID: request.ID,
		Approve:         false,
		Actor:           "user:zsa",
		Reason:          "暂不扩展范围",
		IdempotencyKey:  "reject-7",
	}

	first, err := svc.ResolvePlanChange(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.ResolvePlanChange(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID || first.Version != request.BaseVersion {
		t.Fatalf("duplicate rejection results = %+v, %+v", first, second)
	}
	resolved, err := store.GetPlanChangeRequest(context.Background(), request.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Status != ChangeRequestRejected ||
		resolved.ResolutionReason != "暂不扩展范围" ||
		resolved.ResolvedAt == nil {
		t.Fatalf("rejected Change Request = %+v", resolved)
	}
	versions, err := store.ListPlanVersions(context.Background(), mission.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 1 || versions[0].Version != request.BaseVersion {
		t.Fatalf("Plan versions after rejection = %+v", versions)
	}
	events, err := store.ListEvents(context.Background(), mission.ID, nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	if got := countEventsOfType(events, "plan_change.rejected"); got != 1 {
		t.Fatalf("plan_change.rejected Event count = %d, want 1", got)
	}
}

func TestPauseAndCancelMissionAreAuditedAndIdempotent(t *testing.T) {
	t.Run("pause", func(t *testing.T) {
		store, mission := newApprovedRunningMission(t)
		svc := NewCommandService(store)
		cmd := PauseMissionCommand{
			MissionID:      mission.ID,
			Actor:          "user:zsa",
			Reason:         "等待人工确认",
			IdempotencyKey: "pause-1",
		}
		first, err := svc.PauseMission(context.Background(), cmd)
		if err != nil {
			t.Fatal(err)
		}
		second, err := svc.PauseMission(context.Background(), cmd)
		if err != nil {
			t.Fatal(err)
		}
		if first.Status != MissionNeedsAttention || second.ID != first.ID {
			t.Fatalf("pause results = %+v, %+v", first, second)
		}
		assertCommandEvent(
			t,
			store,
			mission.ID,
			"mission.paused",
			"user:zsa",
			"等待人工确认",
		)
	})

	t.Run("cancel", func(t *testing.T) {
		store, mission := newStoreWithMission(t)
		svc := NewCommandService(store)
		cmd := CancelMissionCommand{
			MissionID:      mission.ID,
			Actor:          "user:zsa",
			Reason:         "目标已撤销",
			IdempotencyKey: "cancel-1",
		}
		first, err := svc.CancelMission(context.Background(), cmd)
		if err != nil {
			t.Fatal(err)
		}
		second, err := svc.CancelMission(context.Background(), cmd)
		if err != nil {
			t.Fatal(err)
		}
		if first.Status != MissionCancelled || second.ID != first.ID {
			t.Fatalf("cancel results = %+v, %+v", first, second)
		}
		assertCommandEvent(
			t,
			store,
			mission.ID,
			"mission.cancelled",
			"user:zsa",
			"目标已撤销",
		)
	})
}

func TestCommandStatusGatesRejectIllegalMissionStates(t *testing.T) {
	allStatuses := []MissionStatus{
		MissionDraft,
		MissionPlanning,
		MissionAwaitingPlanApproval,
		MissionReady,
		MissionRunning,
		MissionVerifying,
		MissionSucceeded,
		MissionFailed,
		MissionNeedsAttention,
		MissionCancelled,
	}
	tests := []struct {
		name    string
		allowed func(MissionStatus) bool
		run     func(*testing.T, MissionStatus) error
	}{
		{
			name: "approve",
			allowed: func(status MissionStatus) bool {
				return status == MissionAwaitingPlanApproval
			},
			run: func(t *testing.T, status MissionStatus) error {
				svc, store, mission, draft := newDraftMissionService(t)
				setMissionStatusForCommandTest(t, store, mission.ID, status)
				_, err := svc.ApprovePlan(context.Background(), ApprovePlanCommand{
					MissionID:      mission.ID,
					Version:        draft.Version,
					Actor:          "user:zsa",
					Reason:         "approve",
					IdempotencyKey: "approve-" + string(status),
				})
				return err
			},
		},
		{
			name: "submit",
			allowed: func(status MissionStatus) bool {
				return status == MissionRunning || status == MissionVerifying
			},
			run: func(t *testing.T, status MissionStatus) error {
				store, mission := newApprovedRunningMission(t)
				setMissionStatusForCommandTest(t, store, mission.ID, status)
				_, err := NewCommandService(store).SubmitPlanChange(
					context.Background(),
					SubmitPlanChangeCommand{
						MissionID:      mission.ID,
						BaseVersion:    mission.CurrentPlanVersion,
						ProposedPlan:   revisedPlanInput(),
						Actor:          "coordinator",
						Reason:         "change",
						IdempotencyKey: "submit-" + string(status),
					},
				)
				return err
			},
		},
		{
			name: "resolve",
			allowed: func(status MissionStatus) bool {
				return status == MissionRunning || status == MissionVerifying
			},
			run: func(t *testing.T, status MissionStatus) error {
				svc, store, mission, request := newPendingChangeService(t)
				setMissionStatusForCommandTest(t, store, mission.ID, status)
				_, err := svc.ResolvePlanChange(
					context.Background(),
					ResolvePlanChangeCommand{
						MissionID:       mission.ID,
						ChangeRequestID: request.ID,
						Approve:         true,
						Actor:           "user:zsa",
						Reason:          "resolve",
						IdempotencyKey:  "resolve-" + string(status),
					},
				)
				return err
			},
		},
		{
			name: "pause",
			allowed: func(status MissionStatus) bool {
				return status.CanTransitionTo(MissionNeedsAttention)
			},
			run: func(t *testing.T, status MissionStatus) error {
				store, mission := newApprovedRunningMission(t)
				setMissionStatusForCommandTest(t, store, mission.ID, status)
				_, err := NewCommandService(store).PauseMission(
					context.Background(),
					PauseMissionCommand{
						MissionID:      mission.ID,
						Actor:          "user:zsa",
						Reason:         "pause",
						IdempotencyKey: "pause-" + string(status),
					},
				)
				return err
			},
		},
		{
			name: "cancel",
			allowed: func(status MissionStatus) bool {
				return status.CanTransitionTo(MissionCancelled)
			},
			run: func(t *testing.T, status MissionStatus) error {
				store, mission := newApprovedRunningMission(t)
				setMissionStatusForCommandTest(t, store, mission.ID, status)
				_, err := NewCommandService(store).CancelMission(
					context.Background(),
					CancelMissionCommand{
						MissionID:      mission.ID,
						Actor:          "user:zsa",
						Reason:         "cancel",
						IdempotencyKey: "cancel-" + string(status),
					},
				)
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, status := range allStatuses {
				if tt.allowed(status) {
					continue
				}
				t.Run(string(status), func(t *testing.T) {
					if err := tt.run(t, status); !errors.Is(err, ErrInvalidTransition) {
						t.Fatalf("command error = %v, want ErrInvalidTransition", err)
					}
				})
			}
		})
	}
}

func TestCommandsRejectBlankIdempotencyKeyBeforeMutation(t *testing.T) {
	svc, store, mission, draft := newDraftMissionService(t)
	_, err := svc.ApprovePlan(context.Background(), ApprovePlanCommand{
		MissionID: mission.ID,
		Version:   draft.Version,
		Actor:     "user:zsa",
		Reason:    "approve",
	})
	if err == nil {
		t.Fatal("ApprovePlan accepted a blank idempotency key")
	}
	got, readErr := store.GetPlan(context.Background(), mission.ID, draft.Version)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if got.Status != PlanDraft {
		t.Fatalf("Plan status = %s after rejected command, want %s", got.Status, PlanDraft)
	}
}

func TestCommandIdempotencyIsScopedByCommandName(t *testing.T) {
	svc, _, mission, request := newPendingChangeService(t)
	const key = "shared-key"

	if _, err := svc.SubmitPlanChange(
		context.Background(),
		SubmitPlanChangeCommand{
			MissionID:      mission.ID,
			BaseVersion:    mission.CurrentPlanVersion,
			ProposedPlan:   revisedPlanInput(),
			Actor:          "coordinator",
			Reason:         "another request",
			IdempotencyKey: key,
		},
	); err != nil {
		t.Fatal(err)
	}
	plan, err := svc.ResolvePlanChange(
		context.Background(),
		ResolvePlanChangeCommand{
			MissionID:       mission.ID,
			ChangeRequestID: request.ID,
			Approve:         false,
			Actor:           "user:zsa",
			Reason:          "reject original",
			IdempotencyKey:  key,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Version != request.BaseVersion {
		t.Fatalf("ResolvePlanChange returned version %d, want %d", plan.Version, request.BaseVersion)
	}
}

func TestApprovePlanRollsBackWhenEventInsertFails(t *testing.T) {
	svc, store, mission, draft := newDraftMissionService(t)
	if _, err := store.db.Exec(`
		CREATE TRIGGER fail_plan_approval_event
		BEFORE INSERT ON mission_events
		WHEN NEW.type = 'plan.approved'
		BEGIN
			SELECT RAISE(ABORT, 'forced plan approval event failure');
		END`); err != nil {
		t.Fatal(err)
	}

	_, err := svc.ApprovePlan(context.Background(), ApprovePlanCommand{
		MissionID:      mission.ID,
		Version:        draft.Version,
		Actor:          "user:zsa",
		Reason:         "approve",
		IdempotencyKey: "rollback-approve",
	})
	if err == nil {
		t.Fatal("ApprovePlan succeeded despite forced Event failure")
	}
	gotMission, readErr := store.GetMission(context.Background(), mission.ID)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if gotMission.Status != MissionAwaitingPlanApproval ||
		gotMission.CurrentPlanVersion != 0 {
		t.Fatalf("Mission survived failed approval as %+v", gotMission)
	}
	gotPlan, readErr := store.GetPlan(context.Background(), mission.ID, draft.Version)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if gotPlan.Status != PlanDraft || gotPlan.ApprovedAt != nil {
		t.Fatalf("Plan survived failed approval as %+v", gotPlan)
	}
	var commandCount int
	if readErr := store.db.QueryRow(`
		SELECT COUNT(*)
		FROM mission_commands
		WHERE mission_id = ?`,
		mission.ID,
	).Scan(&commandCount); readErr != nil {
		t.Fatal(readErr)
	}
	if commandCount != 0 {
		t.Fatalf("stored Command count = %d, want 0", commandCount)
	}
}

func newDraftMissionService(
	t *testing.T,
) (*CommandService, *Store, Mission, *PlanVersion) {
	t.Helper()
	store, mission := newStoreWithMission(t)
	draft, err := store.CreateDraftPlan(
		context.Background(),
		mission.ID,
		samplePlanInput(),
		"coordinator",
	)
	if err != nil {
		t.Fatal(err)
	}
	return NewCommandService(store), store, mission, draft
}

func newPendingChangeService(
	t *testing.T,
) (*CommandService, *Store, Mission, *PlanChangeRequest) {
	t.Helper()
	store, mission := newApprovedRunningMission(t)
	request, err := store.CreatePlanChangeRequest(
		context.Background(),
		mission.ID,
		mission.CurrentPlanVersion,
		revisedPlanInput(),
		"缺少迁移文档",
		"coordinator",
	)
	if err != nil {
		t.Fatal(err)
	}
	return NewCommandService(store), store, mission, request
}

func countEventsOfType(events []Event, eventType string) int {
	count := 0
	for _, event := range events {
		if event.Type == eventType {
			count++
		}
	}
	return count
}

func assertCommandEvent(
	t *testing.T,
	store *Store,
	missionID string,
	eventType string,
	actor string,
	reason string,
) {
	t.Helper()
	events, err := store.ListEvents(context.Background(), missionID, nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	if got := countEventsOfType(events, eventType); got != 1 {
		t.Fatalf("%s Event count = %d, want 1", eventType, got)
	}
	for _, event := range events {
		if event.Type != eventType {
			continue
		}
		var payload struct {
			Actor  string `json:"actor"`
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Actor != actor || payload.Reason != reason {
			t.Fatalf("%s Event payload = %+v", eventType, payload)
		}
	}
}

func setMissionStatusForCommandTest(
	t *testing.T,
	store *Store,
	missionID string,
	status MissionStatus,
) {
	t.Helper()
	if _, err := store.db.Exec(
		`UPDATE missions SET status = ? WHERE id = ?`,
		status,
		missionID,
	); err != nil {
		t.Fatal(err)
	}
}
