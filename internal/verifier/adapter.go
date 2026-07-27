package verifier

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/harness9/internal/mission"
)

// Adapter implements scheduler.Dispatcher for mission.ContractVerification
// Tasks. Dispatch requires task to depend on exactly one other Task — the
// one it verifies. It looks up that Task's most recent Lease to find the
// branch its Worker committed to, checks out an independent, detached
// worktree from that branch, and asynchronously re-runs verification there —
// bounded by the Adapter's own long-lived context, not the caller's per-Tick
// ctx, the same pattern internal/worker.Adapter already established.
type Adapter struct {
	store    *mission.Store
	repoRoot string
	leaseTTL time.Duration
	baseCtx  context.Context
}

// NewAdapter creates an Adapter. repoRoot is the harness9 repository root new
// verification worktrees are created relative to.
func NewAdapter(store *mission.Store, repoRoot string, baseCtx context.Context) *Adapter {
	return &Adapter{store: store, repoRoot: repoRoot, leaseTTL: 2 * time.Hour, baseCtx: baseCtx}
}

// Dispatch implements scheduler.Dispatcher. On any failure it leaves no
// partial state behind, mirroring internal/worker.Adapter.Dispatch: an
// already-created worktree is removed, an already-acquired lease is
// released, and (if StartAttempt itself is what failed) the Task is
// explicitly requeued rather than left stuck in leased.
func (a *Adapter) Dispatch(ctx context.Context, task mission.Task) error {
	if len(task.DependsOn) != 1 {
		return fmt.Errorf("verification task %s must depend on exactly one task, got %d", task.ID, len(task.DependsOn))
	}
	targetID := task.DependsOn[0]

	targetLease, err := a.store.GetLatestLease(ctx, targetID)
	if err != nil {
		return fmt.Errorf("get target lease: %w", err)
	}

	path, branch := verifyWorktreeFor(a.repoRoot, task)
	if err := CreateDetachedWorktree(a.repoRoot, path, targetLease.Branch); err != nil {
		return fmt.Errorf("create detached worktree: %w", err)
	}

	lease, err := a.store.AcquireLease(ctx, task.ID, path, branch, "", a.leaseTTL)
	if err != nil {
		if removeErr := RemoveWorktree(a.repoRoot, path); removeErr != nil {
			return fmt.Errorf("acquire lease: %w (worktree cleanup also failed: %v)", err, removeErr)
		}
		return fmt.Errorf("acquire lease: %w", err)
	}

	attempt, err := a.store.StartAttempt(ctx, task.ID, "verifier-adapter")
	if err != nil {
		if _, transErr := a.store.TransitionTask(ctx, task.ID, mission.TaskQueued); transErr != nil {
			err = fmt.Errorf("%w (requeue also failed: %v)", err, transErr)
		}
		if _, releaseErr := a.store.ReleaseLease(ctx, lease.ID); releaseErr != nil {
			err = fmt.Errorf("%w (lease release also failed: %v)", err, releaseErr)
		}
		if removeErr := RemoveWorktree(a.repoRoot, path); removeErr != nil {
			err = fmt.Errorf("%w (worktree cleanup also failed: %v)", err, removeErr)
		}
		return fmt.Errorf("start attempt: %w", err)
	}

	go a.run(task, targetID, lease, attempt)
	return nil
}

// verifyWorktreeFor computes a deterministic, collision-free path and an
// informational (non-git, since the worktree is detached) branch label for
// one verification Task, keyed off the verification Task's own ID — always
// unique, for the same reason internal/worker.worktreeFor keys off Task.ID.
func verifyWorktreeFor(repoRoot string, task mission.Task) (path, branch string) {
	path = filepath.Join(repoRoot, ".harness9", "verify", task.MissionID, task.ID)
	branch = fmt.Sprintf("verify:%s", task.ID)
	return path, branch
}

// run is deliberately a no-op in this task: Task 4's tests only assert on
// Dispatch's synchronous bookkeeping, not on any asynchronous outcome. Task 5
// replaces this body with real verification logic (and its own tests).
func (a *Adapter) run(verifierTask mission.Task, targetID string, lease mission.WorkspaceLease, attempt mission.TaskAttempt) {
}
