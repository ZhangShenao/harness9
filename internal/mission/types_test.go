package mission

import (
	"fmt"
	"testing"
)

func TestMissionStatusCanTransition(t *testing.T) {
	statuses := []MissionStatus{MissionDraft, MissionPlanning, MissionAwaitingPlanApproval, MissionReady, MissionRunning, MissionVerifying, MissionSucceeded, MissionFailed, MissionNeedsAttention, MissionCancelled}
	allowed := map[MissionStatus]map[MissionStatus]bool{
		MissionDraft:                {MissionPlanning: true, MissionCancelled: true},
		MissionPlanning:             {MissionAwaitingPlanApproval: true, MissionCancelled: true},
		MissionAwaitingPlanApproval: {MissionReady: true, MissionPlanning: true, MissionCancelled: true},
		MissionReady:                {MissionRunning: true, MissionCancelled: true},
		MissionRunning:              {MissionVerifying: true, MissionNeedsAttention: true, MissionFailed: true, MissionCancelled: true},
		MissionVerifying:            {MissionSucceeded: true, MissionFailed: true, MissionNeedsAttention: true},
	}
	for _, test := range transitionCases(statuses, allowed) {
		t.Run(test.name, func(t *testing.T) {
			if got := test.from.CanTransitionTo(test.to); got != test.want {
				t.Fatalf("CanTransitionTo(%q, %q) = %t, want %t", test.from, test.to, got, test.want)
			}
		})
	}
	if MissionStatus("unknown").CanTransitionTo(MissionPlanning) {
		t.Fatal("unknown mission status must not transition")
	}
}

func TestPlanStatusCanTransition(t *testing.T) {
	statuses := []PlanStatus{PlanDraft, PlanAwaitingApproval, PlanApproved, PlanRejected, PlanSuperseded}
	allowed := map[PlanStatus]map[PlanStatus]bool{
		PlanDraft:            {PlanAwaitingApproval: true},
		PlanAwaitingApproval: {PlanApproved: true, PlanDraft: true, PlanRejected: true},
		PlanApproved:         {PlanSuperseded: true},
	}
	for _, test := range transitionCases(statuses, allowed) {
		t.Run(test.name, func(t *testing.T) {
			if got := test.from.CanTransitionTo(test.to); got != test.want {
				t.Fatalf("CanTransitionTo(%q, %q) = %t, want %t", test.from, test.to, got, test.want)
			}
		})
	}
}

func TestTaskStatusCanTransition(t *testing.T) {
	statuses := []TaskStatus{TaskBlocked, TaskQueued, TaskLeased, TaskRunning, TaskVerifying, TaskSucceeded, TaskFailed, TaskAwaitingInput, TaskIndeterminate}
	allowed := map[TaskStatus]map[TaskStatus]bool{
		TaskBlocked:   {TaskQueued: true},
		TaskQueued:    {TaskLeased: true, TaskFailed: true, TaskAwaitingInput: true},
		TaskLeased:    {TaskRunning: true, TaskQueued: true, TaskIndeterminate: true, TaskFailed: true},
		TaskRunning:   {TaskVerifying: true, TaskFailed: true, TaskAwaitingInput: true, TaskIndeterminate: true},
		TaskVerifying: {TaskSucceeded: true, TaskFailed: true, TaskAwaitingInput: true},
	}
	for _, test := range transitionCases(statuses, allowed) {
		t.Run(test.name, func(t *testing.T) {
			if got := test.from.CanTransitionTo(test.to); got != test.want {
				t.Fatalf("CanTransitionTo(%q, %q) = %t, want %t", test.from, test.to, got, test.want)
			}
		})
	}
}

func TestAttemptStatusCanTransition(t *testing.T) {
	statuses := []AttemptStatus{AttemptRunning, AttemptSucceeded, AttemptFailed, AttemptIndeterminate, AttemptCancelled}
	allowed := map[AttemptStatus]map[AttemptStatus]bool{
		AttemptRunning: {AttemptSucceeded: true, AttemptFailed: true, AttemptIndeterminate: true, AttemptCancelled: true},
	}
	for _, test := range transitionCases(statuses, allowed) {
		t.Run(test.name, func(t *testing.T) {
			if got := test.from.CanTransitionTo(test.to); got != test.want {
				t.Fatalf("CanTransitionTo(%q, %q) = %t, want %t", test.from, test.to, got, test.want)
			}
		})
	}
}

func TestChangeRequestStatusCanTransition(t *testing.T) {
	statuses := []ChangeRequestStatus{ChangeRequestPending, ChangeRequestApproved, ChangeRequestRejected, ChangeRequestCancelled}
	allowed := map[ChangeRequestStatus]map[ChangeRequestStatus]bool{
		ChangeRequestPending: {ChangeRequestApproved: true, ChangeRequestRejected: true, ChangeRequestCancelled: true},
	}
	for _, test := range transitionCases(statuses, allowed) {
		t.Run(test.name, func(t *testing.T) {
			if got := test.from.CanTransitionTo(test.to); got != test.want {
				t.Fatalf("CanTransitionTo(%q, %q) = %t, want %t", test.from, test.to, got, test.want)
			}
		})
	}
}

func TestValidateTaskInputs(t *testing.T) {
	tests := []struct {
		name   string
		inputs []TaskInput
		wantOK bool
	}{
		{
			name: "valid acyclic graph",
			inputs: []TaskInput{
				{ClientID: "spec", Title: "Specification", Contract: "write specification"},
				{ClientID: "api", Title: "API", Contract: "implement API", Dependencies: []string{"spec"}},
			},
			wantOK: true,
		},
		{name: "blank client ID", inputs: []TaskInput{{Title: "API", Contract: "implement API"}}},
		{name: "duplicate client ID", inputs: []TaskInput{{ClientID: "api", Title: "API", Contract: "implement API"}, {ClientID: "api", Title: "Other", Contract: "implement other"}}},
		{name: "blank title", inputs: []TaskInput{{ClientID: "api", Contract: "implement API"}}},
		{name: "blank contract", inputs: []TaskInput{{ClientID: "api", Title: "API"}}},
		{name: "unknown dependency", inputs: []TaskInput{{ClientID: "api", Title: "API", Contract: "implement API", Dependencies: []string{"storage"}}}},
		{name: "self dependency", inputs: []TaskInput{{ClientID: "api", Title: "API", Contract: "implement API", Dependencies: []string{"api"}}}},
		{
			name: "cycle",
			inputs: []TaskInput{
				{ClientID: "api", Title: "API", Contract: "implement API", Dependencies: []string{"storage"}},
				{ClientID: "storage", Title: "Storage", Contract: "implement storage", Dependencies: []string{"api"}},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateTaskInputs(test.inputs)
			if (err == nil) != test.wantOK {
				t.Fatalf("ValidateTaskInputs() error = %v, wantOK %t", err, test.wantOK)
			}
		})
	}
}

type transitionCase[S comparable] struct {
	name string
	from S
	to   S
	want bool
}

func transitionCases[S ~string](statuses []S, allowed map[S]map[S]bool) []transitionCase[S] {
	cases := make([]transitionCase[S], 0, len(statuses)*len(statuses))
	for _, from := range statuses {
		for _, to := range statuses {
			cases = append(cases, transitionCase[S]{name: fmt.Sprintf("%s_to_%s", from, to), from: from, to: to, want: allowed[from][to]})
		}
	}
	return cases
}
