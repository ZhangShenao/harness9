package verifier

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"time"

	"github.com/harness9/internal/logfmt"
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

// run executes independent verification and records the outcome. It always
// runs on a's baseCtx, not the ctx Dispatch's caller passed in, so it
// survives past the lifetime of any single Tick call.
func (a *Adapter) run(verifierTask mission.Task, targetID string, lease mission.WorkspaceLease, attempt mission.TaskAttempt) {
	report := runVerificationChecks(lease.Path)
	a.complete(verifierTask, targetID, attempt, lease, report)
}

// complete records the result of one verification Attempt. A conclusive
// verification result — pass or fail — always: (1) records Evidence tagged
// with both the target's own AttemptID and this Attempt's ID as
// VerifierAttemptID, (2) advances the TARGET Task (not this verification
// Task) to succeeded or failed based on the report, and (3) advances this
// verification Task itself to succeeded, since it did its job correctly
// regardless of what it found. Only a process failure (couldn't load the
// target's attempt, or a Store write itself failed) fails this verification
// Task via failVerifierOnly, leaving the target Task untouched in verifying
// for a future verification Attempt to retry — a broken verification
// process must never be mistaken for a verified failure of the target.
func (a *Adapter) complete(
	verifierTask mission.Task,
	targetID string,
	attempt mission.TaskAttempt,
	lease mission.WorkspaceLease,
	report verificationReport,
) {
	defer func() {
		if _, err := a.store.ReleaseLease(a.baseCtx, lease.ID); err != nil {
			log.Print(logfmt.FormatMsg("verifier", fmt.Sprintf("释放 lease %s 失败: %v", lease.ID, err)))
		}
	}()

	targetAttempt, err := a.store.GetLatestAttempt(a.baseCtx, targetID)
	if err != nil {
		a.failVerifierOnly(verifierTask, attempt, "load target attempt", err.Error())
		return
	}
	if _, err := a.store.AddEvidence(a.baseCtx, mission.CreateEvidenceInput{
		MissionID: verifierTask.MissionID, TaskID: targetID, AttemptID: targetAttempt.ID,
		VerifierAttemptID: attempt.ID, Kind: "independent_verification",
		Content: []byte(report.output), Passed: report.passed,
	}); err != nil {
		a.failVerifierOnly(verifierTask, attempt, "record evidence", err.Error())
		return
	}

	targetNext := mission.TaskFailed
	if report.passed {
		targetNext = mission.TaskSucceeded
	}
	if _, err := a.store.TransitionTask(a.baseCtx, targetID, targetNext); err != nil {
		a.failVerifierOnly(verifierTask, attempt, "transition target task", err.Error())
		return
	}

	if _, err := a.store.TransitionAttempt(a.baseCtx, attempt.ID, mission.AttemptSucceeded); err != nil {
		log.Print(logfmt.FormatMsg("verifier", fmt.Sprintf("推进 attempt %s 失败: %v", attempt.ID, err)))
	}
	if _, err := a.store.TransitionTask(a.baseCtx, verifierTask.ID, mission.TaskVerifying); err != nil {
		log.Print(logfmt.FormatMsg("verifier", fmt.Sprintf("推进 verifier task %s 失败: %v", verifierTask.ID, err)))
		return
	}
	if _, err := a.store.TransitionTask(a.baseCtx, verifierTask.ID, mission.TaskSucceeded); err != nil {
		log.Print(logfmt.FormatMsg("verifier", fmt.Sprintf("推进 verifier task %s 失败: %v", verifierTask.ID, err)))
	}
}

// failVerifierOnly marks the verification Task itself failed without
// touching the target Task at all, so a future verification Attempt can
// still retry it.
func (a *Adapter) failVerifierOnly(verifierTask mission.Task, attempt mission.TaskAttempt, kind, detail string) {
	if _, err := a.store.TransitionAttempt(a.baseCtx, attempt.ID, mission.AttemptFailed); err != nil {
		log.Print(logfmt.FormatMsg("verifier", fmt.Sprintf("推进 attempt %s 失败: %v", attempt.ID, err)))
	}
	if _, err := a.store.TransitionTask(a.baseCtx, verifierTask.ID, mission.TaskFailed); err != nil {
		log.Print(logfmt.FormatMsg("verifier", fmt.Sprintf("推进 verifier task %s 失败: %v", verifierTask.ID, err)))
	}
	log.Print(logfmt.FormatMsg("verifier", fmt.Sprintf("%s: %s", kind, detail)))
}
