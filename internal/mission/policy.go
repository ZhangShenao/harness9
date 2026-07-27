package mission

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Policy describes Scheduler-enforced limits derived from a Mission's PolicyJSON.
// Only concurrency is defined so far; approval/retry/tool-scope policy fields are
// added by later Mission Control milestones once their consumers exist.
type Policy struct {
	// MaxConcurrentTasks caps how many Tasks the Scheduler may run at once for
	// one Mission. Missing or zero in PolicyJSON defaults to 1.
	MaxConcurrentTasks int
}

// ParsePolicy decodes a Mission's PolicyJSON into a Policy with defaults applied.
func ParsePolicy(raw string) (Policy, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = "{}"
	}
	var decoded struct {
		MaxConcurrentTasks int `json:"max_concurrent_tasks"`
	}
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		return Policy{}, fmt.Errorf("decode mission policy: %w", err)
	}
	if decoded.MaxConcurrentTasks < 0 {
		return Policy{}, fmt.Errorf("policy max_concurrent_tasks cannot be negative")
	}
	if decoded.MaxConcurrentTasks == 0 {
		decoded.MaxConcurrentTasks = 1
	}
	return Policy{MaxConcurrentTasks: decoded.MaxConcurrentTasks}, nil
}
