// Package integration provides the integration adapter that merges
// dependent Task branches and runs joint tests, producing Evidence.
package integration

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"

	"github.com/harness9/internal/mission"
	"github.com/harness9/internal/scheduler"
)

// Adapter implements scheduler.Dispatcher for integration Tasks.
type Adapter struct {
	store   *mission.Store
	repoDir string
}

// NewAdapter creates an Integration Adapter.
func NewAdapter(store *mission.Store, repoDir string) *Adapter {
	return &Adapter{store: store, repoDir: repoDir}
}

// Dispatch merges dependency branches and runs joint verification.
func (a *Adapter) Dispatch(ctx context.Context, task mission.Task, attempt mission.TaskAttempt) (scheduler.Result, error) {
	mergeFailed := false
	var mergeOutput bytes.Buffer
	for _, depID := range task.DependsOn {
		depTask, err := a.store.GetTask(ctx, depID)
		if err != nil {
			mergeOutput.WriteString(fmt.Sprintf("dependency %s not found: %v\n", depID, err))
			mergeFailed = true
			continue
		}
		branch := fmt.Sprintf("mission/%s/%s", shortID(depTask.MissionID), shortID(depTask.ID))
		out, err := a.runGit(ctx, "merge", "--no-edit", branch)
		mergeOutput.Write(out)
		if err != nil {
			mergeOutput.WriteString(fmt.Sprintf("merge %s failed: %v\n", branch, err))
			mergeFailed = true
			a.runGit(ctx, "merge", "--abort")
			break
		}
	}
	if a.store != nil {
		a.store.AddEvidence(ctx, mission.CreateEvidenceInput{
			MissionID: task.MissionID, TaskID: task.ID, AttemptID: attempt.ID,
			Kind: "integration_merge", Content: append([]byte("passed"), mergeOutput.Bytes()...),
			Passed: !mergeFailed,
		})
	}
	if mergeFailed {
		return scheduler.Result{Status: "failed", ExitReason: "merge conflict"}, nil
	}
	testOut, testPassed := a.runCheck(ctx, []string{"go", "test", "./...", "-count=1"})
	if a.store != nil {
		a.store.AddEvidence(ctx, mission.CreateEvidenceInput{
			MissionID: task.MissionID, TaskID: task.ID, AttemptID: attempt.ID,
			Kind: "integration_test", Content: testOut, Passed: testPassed,
		})
	}
	if !testPassed {
		return scheduler.Result{Status: "failed", ExitReason: "joint tests failed"}, nil
	}
	return scheduler.Result{Status: "succeeded", ExitReason: "integration passed"}, nil
}

func (a *Adapter) runGit(ctx context.Context, args ...string) ([]byte, error) {
	c := exec.CommandContext(ctx, "git", args...)
	c.Dir = a.repoDir
	return c.CombinedOutput()
}

func (a *Adapter) runCheck(ctx context.Context, cmd []string) ([]byte, bool) {
	c := exec.CommandContext(ctx, cmd[0], cmd[1:]...)
	c.Dir = a.repoDir
	var buf bytes.Buffer
	c.Stdout = &buf
	c.Stderr = &buf
	if err := c.Run(); err != nil {
		buf.WriteString(fmt.Sprintf("\nexit: %v", err))
		return buf.Bytes(), false
	}
	return buf.Bytes(), true
}

func shortID(id string) string {
	if len(id) >= 8 {
		return id[:8]
	}
	return id
}
