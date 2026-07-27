package integration

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/harness9/internal/mission"
	"github.com/harness9/internal/worker"
)

// Adapter implements scheduler.Dispatcher for mission.ContractIntegration
// Tasks. Dispatch requires task to depend on at least one other Task — the
// ones whose branches it merges. It provisions one Mission-level worktree
// (not per-Task, since integration combines multiple Tasks' work into one
// place, unlike internal/worker's and internal/verifier's per-Task
// worktrees), then asynchronously merges every dependency's branch into it
// and independently re-verifies the combined result — bounded by the
// Adapter's own long-lived context, not the caller's per-Tick ctx, the same
// pattern internal/worker.Adapter and internal/verifier.Adapter established.
type Adapter struct {
	store    *mission.Store
	repoRoot string
	leaseTTL time.Duration
	baseCtx  context.Context
}

// NewAdapter creates an Adapter. repoRoot is the harness9 repository root new
// integration worktrees are created relative to.
func NewAdapter(store *mission.Store, repoRoot string, baseCtx context.Context) *Adapter {
	return &Adapter{store: store, repoRoot: repoRoot, leaseTTL: 4 * time.Hour, baseCtx: baseCtx}
}

// var _ ... proves Adapter has the exact method shape scheduler.Dispatcher
// requires, without importing internal/scheduler.
var _ interface {
	Dispatch(ctx context.Context, task mission.Task) error
} = (*Adapter)(nil)

// Dispatch implements scheduler.Dispatcher. On any failure it leaves no
// partial state behind, mirroring internal/worker.Adapter and
// internal/verifier.Adapter's Dispatch.
func (a *Adapter) Dispatch(ctx context.Context, task mission.Task) error {
	if len(task.DependsOn) == 0 {
		return fmt.Errorf("integration task %s must depend on at least one task", task.ID)
	}

	path, branch := integrationWorktreeFor(a.repoRoot, task)
	if err := worker.CreateWorktree(a.repoRoot, path, branch); err != nil {
		return fmt.Errorf("create integration worktree: %w", err)
	}
	baseSHA, err := currentCommit(path)
	if err != nil {
		if removeErr := worker.RemoveWorktree(a.repoRoot, path); removeErr != nil {
			return fmt.Errorf("read base commit: %w (worktree cleanup also failed: %v)", err, removeErr)
		}
		return fmt.Errorf("read base commit: %w", err)
	}

	lease, err := a.store.AcquireLease(ctx, task.ID, path, branch, "", a.leaseTTL)
	if err != nil {
		if removeErr := worker.RemoveWorktree(a.repoRoot, path); removeErr != nil {
			return fmt.Errorf("acquire lease: %w (worktree cleanup also failed: %v)", err, removeErr)
		}
		return fmt.Errorf("acquire lease: %w", err)
	}

	attempt, err := a.store.StartAttempt(ctx, task.ID, "integration-adapter")
	if err != nil {
		if _, transErr := a.store.TransitionTask(ctx, task.ID, mission.TaskQueued); transErr != nil {
			err = fmt.Errorf("%w (requeue also failed: %v)", err, transErr)
		}
		if _, releaseErr := a.store.ReleaseLease(ctx, lease.ID); releaseErr != nil {
			err = fmt.Errorf("%w (lease release also failed: %v)", err, releaseErr)
		}
		if removeErr := worker.RemoveWorktree(a.repoRoot, path); removeErr != nil {
			err = fmt.Errorf("%w (worktree cleanup also failed: %v)", err, removeErr)
		}
		return fmt.Errorf("start attempt: %w", err)
	}

	go a.run(task, lease, attempt, baseSHA)
	return nil
}

// integrationWorktreeFor computes a deterministic, collision-free path and
// branch name for one Integration Task, keyed off the Integration Task's own
// ID — always unique, for the same reason internal/worker.worktreeFor and
// internal/verifier.verifyWorktreeFor key off Task.ID.
func integrationWorktreeFor(repoRoot string, task mission.Task) (path, branch string) {
	path = filepath.Join(repoRoot, ".harness9", "integrate", task.MissionID, task.ID)
	branch = fmt.Sprintf("integrate/%s", task.ID)
	return path, branch
}

// currentCommit returns the commit currently checked out at dir.
func currentCommit(dir string) (string, error) {
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD: %w: %s", err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

// run is deliberately a no-op in this task: Task 2's tests only assert on
// Dispatch's synchronous bookkeeping, not on any asynchronous outcome. Task 3
// replaces this body with real merge-and-verify logic (and its own tests).
func (a *Adapter) run(task mission.Task, lease mission.WorkspaceLease, attempt mission.TaskAttempt, baseSHA string) {
}
