package main

import (
	"testing"

	"github.com/harness9/internal/mission"
)

func TestFeatureMissionTasksAreValid(t *testing.T) {
	if err := mission.ValidateTaskInputs(featureMissionTasks()); err != nil {
		t.Fatalf("ValidateTaskInputs: %v", err)
	}
}

func TestFeatureMissionTasksHaveExpectedShape(t *testing.T) {
	tasks := featureMissionTasks()
	if len(tasks) != 7 {
		t.Fatalf("len(tasks) = %d, want 7", len(tasks))
	}

	byClientID := make(map[string]mission.TaskInput, len(tasks))
	kindCounts := make(map[string]int, 3)
	for _, task := range tasks {
		byClientID[task.ClientID] = task
		kindCounts[task.ContractKind]++
	}

	if kindCounts[mission.ContractImplementation] != 3 {
		t.Errorf("implementation task count = %d, want 3", kindCounts[mission.ContractImplementation])
	}
	if kindCounts[mission.ContractVerification] != 3 {
		t.Errorf("verification task count = %d, want 3", kindCounts[mission.ContractVerification])
	}
	if kindCounts[mission.ContractIntegration] != 1 {
		t.Errorf("integration task count = %d, want 1", kindCounts[mission.ContractIntegration])
	}

	for _, tc := range []struct {
		clientID string
		wantDeps []string
	}{
		{"memory-search", nil},
		{"search-docs", nil},
		{"search-tool", []string{"memory-search"}},
		{"verify-memory-search", []string{"memory-search"}},
		{"verify-search-docs", []string{"search-docs"}},
		{"verify-search-tool", []string{"search-tool"}},
		{"integration", []string{"memory-search", "search-docs", "search-tool"}},
	} {
		task, ok := byClientID[tc.clientID]
		if !ok {
			t.Fatalf("missing task %q", tc.clientID)
		}
		if len(task.Dependencies) != len(tc.wantDeps) {
			t.Fatalf("task %q dependencies = %v, want %v", tc.clientID, task.Dependencies, tc.wantDeps)
		}
		for i, dep := range tc.wantDeps {
			if task.Dependencies[i] != dep {
				t.Fatalf("task %q dependencies = %v, want %v", tc.clientID, task.Dependencies, tc.wantDeps)
			}
		}
	}
}
