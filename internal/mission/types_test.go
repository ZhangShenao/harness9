package mission

import "testing"

func TestMissionStatusCanTransition(t *testing.T) {
	tests := []struct {
		name string
		from MissionStatus
		to   MissionStatus
		want bool
	}{
		{name: "draft to planning", from: MissionDraft, to: MissionPlanning, want: true},
		{name: "draft to cancelled", from: MissionDraft, to: MissionCancelled, want: true},
		{name: "planning to awaiting approval", from: MissionPlanning, to: MissionAwaitingPlanApproval, want: true},
		{name: "planning to cancelled", from: MissionPlanning, to: MissionCancelled, want: true},
		{name: "awaiting approval to ready", from: MissionAwaitingPlanApproval, to: MissionReady, want: true},
		{name: "awaiting approval back to planning", from: MissionAwaitingPlanApproval, to: MissionPlanning, want: true},
		{name: "awaiting approval to cancelled", from: MissionAwaitingPlanApproval, to: MissionCancelled, want: true},
		{name: "ready to running", from: MissionReady, to: MissionRunning, want: true},
		{name: "ready to cancelled", from: MissionReady, to: MissionCancelled, want: true},
		{name: "running to verifying", from: MissionRunning, to: MissionVerifying, want: true},
		{name: "running to needs attention", from: MissionRunning, to: MissionNeedsAttention, want: true},
		{name: "running to failed", from: MissionRunning, to: MissionFailed, want: true},
		{name: "running to cancelled", from: MissionRunning, to: MissionCancelled, want: true},
		{name: "verifying to succeeded", from: MissionVerifying, to: MissionSucceeded, want: true},
		{name: "verifying to failed", from: MissionVerifying, to: MissionFailed, want: true},
		{name: "verifying to needs attention", from: MissionVerifying, to: MissionNeedsAttention, want: true},
		{name: "draft cannot run", from: MissionDraft, to: MissionRunning, want: false},
		{name: "terminal cannot run", from: MissionSucceeded, to: MissionRunning, want: false},
		{name: "unknown cannot transition", from: MissionStatus("unknown"), to: MissionPlanning, want: false},
	}

	for _, test := range tests {
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
