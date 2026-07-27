package worker

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/harness9/internal/mission"
	"github.com/harness9/internal/subagent"
)

// Executor runs one Task's implementation sub-agent to completion inside a
// given worktree and returns its raw result. The real implementation
// (Task 5's RunnerExecutor) wraps subagent.Runner + an optional Sandbox;
// tests use a fake that performs real git operations without a real LLM.
type Executor interface {
	Execute(ctx context.Context, workDir, prompt string) (subagent.SubAgentResult, error)
}

// Adapter implements scheduler.Dispatcher. Dispatch only performs the fast,
// synchronous bookkeeping (worktree + lease + attempt); the slow part
// (running the sub-agent to completion) happens in a detached goroutine
// bounded by baseCtx, not by Dispatch's caller-supplied ctx, so an Attempt
// keeps running after a single Scheduler.Tick call returns.
type Adapter struct {
	store      *mission.Store
	repoRoot   string
	leaseTTL   time.Duration
	executor   Executor
	baseCtx    context.Context
	workerName string
}

// NewAdapter creates an Adapter. repoRoot is the harness9 repository root new
// worktrees are created relative to.
func NewAdapter(store *mission.Store, repoRoot string, executor Executor, baseCtx context.Context) *Adapter {
	return &Adapter{
		store:      store,
		repoRoot:   repoRoot,
		leaseTTL:   2 * time.Hour,
		executor:   executor,
		baseCtx:    baseCtx,
		workerName: "worker-adapter",
	}
}

// Dispatch implements scheduler.Dispatcher. On any failure it leaves no
// partial state behind: an already-created worktree is removed before the
// error is returned, and the Task remains schedulable (queued, or whatever
// status it already had) for the Scheduler to retry or an operator to
// inspect.
func (a *Adapter) Dispatch(ctx context.Context, task mission.Task) error {
	path, branch := worktreeFor(a.repoRoot, task)
	if err := CreateWorktree(a.repoRoot, path, branch); err != nil {
		return fmt.Errorf("create worktree: %w", err)
	}

	lease, err := a.store.AcquireLease(ctx, task.ID, path, branch, "", a.leaseTTL)
	if err != nil {
		if removeErr := RemoveWorktree(a.repoRoot, path); removeErr != nil {
			return fmt.Errorf("acquire lease: %w (worktree cleanup also failed: %v)", err, removeErr)
		}
		return fmt.Errorf("acquire lease: %w", err)
	}

	attempt, err := a.store.StartAttempt(ctx, task.ID, a.workerName)
	if err != nil {
		if _, releaseErr := a.store.ReleaseLease(ctx, lease.ID); releaseErr != nil {
			err = fmt.Errorf("%w (lease release also failed: %v)", err, releaseErr)
		}
		// AcquireLease already advanced the Task from queued to leased; undo
		// that here so it is not left stuck in mission.TaskLeased with no
		// active lease and no attempt, which would make it permanently
		// unschedulable (see scheduler.Dispatcher's contract).
		if _, transitionErr := a.store.TransitionTask(ctx, task.ID, mission.TaskQueued); transitionErr != nil {
			err = fmt.Errorf("%w (task requeue also failed: %v)", err, transitionErr)
		}
		if removeErr := RemoveWorktree(a.repoRoot, path); removeErr != nil {
			err = fmt.Errorf("%w (worktree cleanup also failed: %v)", err, removeErr)
		}
		return fmt.Errorf("start attempt: %w", err)
	}

	go a.run(task, lease, attempt)
	return nil
}

// worktreeFor computes a deterministic, collision-free worktree path and
// branch name for one Task, rooted under the repo's .harness9/missions/ tree.
func worktreeFor(repoRoot string, task mission.Task) (path, branch string) {
	name := task.ClientID
	if name == "" {
		name = task.ID
	}
	path = filepath.Join(repoRoot, ".harness9", "missions", task.MissionID, name)
	branch = fmt.Sprintf("mission/%s/%s", task.MissionID, name)
	return path, branch
}

// run is deliberately a no-op in this task: Task 3's tests only assert on
// Dispatch's synchronous bookkeeping, not on any asynchronous outcome. Task 4
// replaces this body with real completion logic (and its own tests).
func (a *Adapter) run(task mission.Task, lease mission.WorkspaceLease, attempt mission.TaskAttempt) {
}
