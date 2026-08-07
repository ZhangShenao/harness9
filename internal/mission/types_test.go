package mission

import "testing"

func TestContractKindConstants(t *testing.T) {
	cases := []struct {
		kind ContractKind
		want string
	}{
		{ContractImplementation, "implementation"},
		{ContractVerification, "verification"},
		{ContractIntegration, "integration"},
	}
	for _, c := range cases {
		if string(c.kind) != c.want {
			t.Errorf("ContractKind = %q, want %q", c.kind, c.want)
		}
	}
}

func TestValidMissionTransition(t *testing.T) {
	cases := []struct {
		from, to MissionStatus
		want     bool
	}{
		{MissionDraft, MissionPlanning, true},
		{MissionPlanning, MissionReady, true},
		{MissionPlanning, MissionDraft, false},
		{MissionReady, MissionRunning, true},
		{MissionRunning, MissionVerifying, true},
		{MissionVerifying, MissionSucceeded, true},
		{MissionVerifying, MissionNeedsAttention, true},
		{MissionRunning, MissionCancelled, true},
		{MissionNeedsAttention, MissionRunning, true},
		{MissionSucceeded, MissionRunning, false},
		{MissionFailed, MissionRunning, false},
	}
	for _, c := range cases {
		if got := validMissionTransition(c.from, c.to); got != c.want {
			t.Errorf("validMissionTransition(%q,%q)=%v want %v", c.from, c.to, got, c.want)
		}
	}
}

func TestTaskInputFields(t *testing.T) {
	in := TaskInput{
		Kind:       ContractImplementation,
		Goal:       "implement X",
		Acceptance: []string{"go test passes"},
		Budget:     Budget{MaxTokens: 100000, MaxTurns: 50},
		MaxRetries: 2,
	}
	if in.Kind != ContractImplementation {
		t.Fatalf("kind = %q", in.Kind)
	}
	if in.Budget.MaxTokens != 100000 {
		t.Fatalf("tokens = %d", in.Budget.MaxTokens)
	}
}

func TestPlanStatusConstants(t *testing.T) {
	if string(PlanDraft) != "draft" || string(PlanApproved) != "approved" || string(PlanSuperseded) != "superseded" {
		t.Fatal("PlanStatus constants mismatch")
	}
}

func TestLeaseStatusConstants(t *testing.T) {
	if string(LeaseActive) != "active" || string(LeaseReleased) != "released" || string(LeaseExpired) != "expired" {
		t.Fatal("LeaseStatus constants mismatch")
	}
}

func TestPolicyDefaults(t *testing.T) {
	p := DefaultPolicy()
	if p.MissionConcurrency != 1 {
		t.Fatalf("default mission concurrency = %d, want 1", p.MissionConcurrency)
	}
	if p.GlobalConcurrency != 2 {
		t.Fatalf("default global concurrency = %d, want 2", p.GlobalConcurrency)
	}
}
