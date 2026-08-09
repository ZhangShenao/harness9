// Package verifier provides the independent verification adapter that
// re-runs build, test, and vet checks on implementation Task output and
// produces Evidence records. The Verifier never verifies its own output.
package verifier

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"

	"github.com/harness9/internal/mission"
	"github.com/harness9/internal/scheduler"
)

// Check defines one verification command.
type Check struct {
	Kind string
	Cmd  []string
}

// DefaultChecks returns the standard go build/vet/test checks.
func DefaultChecks() []Check {
	return []Check{
		{"build", []string{"go", "build", "./..."}},
		{"vet", []string{"go", "vet", "./..."}},
		{"test", []string{"go", "test", "./...", "-count=1"}},
	}
}

// Adapter implements scheduler.Dispatcher for verification Tasks.
type Adapter struct {
	store   *mission.Store
	repoDir string
	checks  []Check
}

// NewAdapter creates a Verifier Adapter with default checks.
func NewAdapter(store *mission.Store, repoDir string) *Adapter {
	return &Adapter{store: store, repoDir: repoDir, checks: DefaultChecks()}
}

// NewAdapterWithChecks creates a Verifier Adapter with custom checks (for testing).
func NewAdapterWithChecks(store *mission.Store, repoDir string, checks []Check) *Adapter {
	return &Adapter{store: store, repoDir: repoDir, checks: checks}
}

// Dispatch runs deterministic verification checks and produces Evidence.
func (a *Adapter) Dispatch(ctx context.Context, task mission.Task, attempt mission.TaskAttempt) (scheduler.Result, error) {
	allPassed := true
	for _, check := range a.checks {
		output, passed := a.runCheck(ctx, check.Cmd)
		if a.store != nil {
			a.store.AddEvidence(ctx, mission.CreateEvidenceInput{
				MissionID: task.MissionID, TaskID: task.ID, AttemptID: attempt.ID,
				Kind: check.Kind, Content: output, Passed: passed,
			})
		}
		if !passed {
			allPassed = false
		}
	}
	status := "succeeded"
	exitReason := "all checks passed"
	if !allPassed {
		status = "failed"
		exitReason = "one or more checks failed"
	}
	return scheduler.Result{Status: status, ExitReason: exitReason}, nil
}

func (a *Adapter) runCheck(ctx context.Context, cmd []string) ([]byte, bool) {
	c := exec.CommandContext(ctx, cmd[0], cmd[1:]...)
	c.Dir = a.repoDir
	var buf bytes.Buffer
	c.Stdout = &buf
	c.Stderr = &buf
	err := c.Run()
	if err != nil {
		buf.WriteString(fmt.Sprintf("\nexit: %v", err))
		return buf.Bytes(), false
	}
	return buf.Bytes(), true
}
