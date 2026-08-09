package worker

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/harness9/internal/mission"
	"github.com/harness9/internal/scheduler"
	"github.com/harness9/internal/subagent"
)

// RunnerExecutor is the interface the WorkerAdapter depends on.
// subagent.Runner satisfies this interface.
type RunnerExecutor interface {
	Run(ctx context.Context, def subagent.SubAgentDefinition, prompt string, background bool) (subagent.SubAgentResult, error)
}

// WorkerAdapterConfig configures a WorkerAdapter.
type WorkerAdapterConfig struct {
	Runner      RunnerExecutor
	Store       *mission.Store
	RepoDir     string
	WorkDirBase string
}

// WorkerAdapter implements scheduler.Dispatcher for implementation Tasks.
type WorkerAdapter struct {
	runner      RunnerExecutor
	store       *mission.Store
	repoDir     string
	workDirBase string
}

// NewWorkerAdapter creates a WorkerAdapter.
func NewWorkerAdapter(cfg WorkerAdapterConfig) *WorkerAdapter {
	base := cfg.WorkDirBase
	if base == "" {
		base = ".missions"
	}
	return &WorkerAdapter{
		runner:      cfg.Runner,
		store:       cfg.Store,
		repoDir:     cfg.RepoDir,
		workDirBase: base,
	}
}

// Dispatch executes a Task Attempt: creates worktree, runs sub-agent, records artifact, cleans up.
func (w *WorkerAdapter) Dispatch(ctx context.Context, task mission.Task, attempt mission.TaskAttempt) (scheduler.Result, error) {
	wtPath := filepath.Join(w.workDirBase, task.MissionID, task.ID, attempt.ID)
	branch := fmt.Sprintf("mission/%s/%s/%s", shortID(task.MissionID), shortID(task.ID), shortID(attempt.ID))

	if err := CreateWorktree(ctx, w.repoDir, wtPath, branch); err != nil {
		return scheduler.Result{Status: "failed", ExitReason: err.Error()}, nil
	}
	defer RemoveWorktree(context.Background(), w.repoDir, wtPath)

	def := subagent.SubAgentDefinition{
		Name:         "worker",
		Description:  "Generic implementation worker for Mission tasks",
		SystemPrompt: "You are a Mission Worker. Implement the task, run tests, commit, and output TASK_RESULT.",
		Tools:        task.Input.AllowedTools,
		MaxTurns:     task.Input.Budget.MaxTurns,
	}

	depArtifacts := w.loadDepArtifacts(ctx, task)
	prompt := BuildImplementationContract(task, depArtifacts)

	result, err := w.runner.Run(ctx, def, prompt, true)
	if err != nil {
		return scheduler.Result{Status: "indeterminate", ExitReason: err.Error()}, nil
	}

	taskResult, err := ParseResult(result.FinalText)
	if err != nil {
		return scheduler.Result{Status: "failed", ExitReason: fmt.Sprintf("parse result: %v", err)}, nil
	}

	manifest := fmt.Sprintf("commit: %s\nfiles: %v\nsummary: %s", taskResult.Commit, taskResult.Files, taskResult.Summary)
	if w.store != nil {
		w.store.AddArtifact(ctx, mission.CreateArtifactInput{
			MissionID: task.MissionID, TaskID: task.ID, AttemptID: attempt.ID,
			Kind: "manifest", Content: []byte(manifest),
		})
	}

	return scheduler.Result{Status: "succeeded", ExitReason: taskResult.Summary}, nil
}

func (w *WorkerAdapter) loadDepArtifacts(ctx context.Context, task mission.Task) []mission.Artifact {
	var artifacts []mission.Artifact
	for _, depID := range task.DependsOn {
		_, err := w.store.GetLatestAttempt(ctx, depID)
		if err != nil {
			continue
		}
	}
	return artifacts
}

func shortID(id string) string {
	if len(id) >= 8 {
		return id[:8]
	}
	return id
}
