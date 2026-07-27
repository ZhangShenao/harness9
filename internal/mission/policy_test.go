package mission

import "testing"

func TestParsePolicyDefaultsMaxConcurrentTasksToOne(t *testing.T) {
	tests := []string{"", "{}", `{"max_concurrent_tasks":0}`}
	for _, raw := range tests {
		t.Run(raw, func(t *testing.T) {
			policy, err := ParsePolicy(raw)
			if err != nil {
				t.Fatalf("ParsePolicy(%q): %v", raw, err)
			}
			if policy.MaxConcurrentTasks != 1 {
				t.Fatalf("MaxConcurrentTasks = %d, want 1", policy.MaxConcurrentTasks)
			}
		})
	}
}

func TestParsePolicyReadsMaxConcurrentTasks(t *testing.T) {
	policy, err := ParsePolicy(`{"max_concurrent_tasks":3}`)
	if err != nil {
		t.Fatalf("ParsePolicy: %v", err)
	}
	if policy.MaxConcurrentTasks != 3 {
		t.Fatalf("MaxConcurrentTasks = %d, want 3", policy.MaxConcurrentTasks)
	}
}

func TestParsePolicyRejectsInvalidInput(t *testing.T) {
	tests := []string{`{`, `{"max_concurrent_tasks":-1}`, `[]`}
	for _, raw := range tests {
		t.Run(raw, func(t *testing.T) {
			if _, err := ParsePolicy(raw); err == nil {
				t.Fatalf("ParsePolicy(%q) = nil error, want an error", raw)
			}
		})
	}
}
