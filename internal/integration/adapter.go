package integration

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/harness9/internal/logfmt"
	"github.com/harness9/internal/mission"
	"github.com/harness9/internal/verifier"
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

// run merges every dependency Task's branch into the integration worktree,
// independently re-verifies the combined result, and records the outcome.
// It always runs on a's baseCtx, not the ctx Dispatch's caller passed in, so
// it survives past the lifetime of any single Tick call.
func (a *Adapter) run(task mission.Task, lease mission.WorkspaceLease, attempt mission.TaskAttempt, baseSHA string) {
	for _, depID := range task.DependsOn {
		depLease, err := a.store.GetLatestLease(a.baseCtx, depID)
		if err != nil {
			a.complete(task, attempt, lease, baseSHA, verifier.VerificationReport{}, fmt.Errorf("load dependency lease: %w", err))
			return
		}
		if err := MergeBranch(lease.Path, depLease.Branch); err != nil {
			a.complete(task, attempt, lease, baseSHA, verifier.VerificationReport{}, fmt.Errorf("merge branch %s: %w", depLease.Branch, err))
			return
		}
	}
	report := verifier.RunVerificationChecks(lease.Path)
	a.complete(task, attempt, lease, baseSHA, report, nil)
}

// complete records the result of one Integration Attempt. Success records a
// Mission-level Artifact (the combined diff) and passing Evidence, advances
// the Attempt to succeeded, and the Integration Task itself through
// verifying to succeeded — which, via internal/mission's TransitionTask,
// automatically completes the whole Mission once every other Task has also
// succeeded. Any failure (a load/merge error, or a failing joint
// verification) records failing Evidence, fails the Task, and escalates the
// Mission to needs_attention: nothing in this increment can resolve a merge
// conflict or a failing joint test suite automatically.
func (a *Adapter) complete(
	task mission.Task,
	attempt mission.TaskAttempt,
	lease mission.WorkspaceLease,
	baseSHA string,
	report verifier.VerificationReport,
	runErr error,
) {
	defer func() {
		if _, err := a.store.ReleaseLease(a.baseCtx, lease.ID); err != nil {
			log.Print(logfmt.FormatMsg("integration", fmt.Sprintf("释放 lease %s 失败: %v", lease.ID, err)))
		}
	}()

	if runErr != nil {
		a.fail(task, attempt, "integration process error", runErr.Error())
		return
	}
	if !report.Passed {
		a.fail(task, attempt, "joint verification failed", report.Output)
		return
	}

	diff := captureDiff(lease.Path, baseSHA)
	if _, err := a.store.AddArtifact(a.baseCtx, mission.CreateArtifactInput{
		MissionID: task.MissionID, TaskID: task.ID, AttemptID: attempt.ID,
		Kind: "integration_diff", Content: []byte(diff),
	}); err != nil {
		a.fail(task, attempt, "record artifact", err.Error())
		return
	}
	if _, err := a.store.AddEvidence(a.baseCtx, mission.CreateEvidenceInput{
		MissionID: task.MissionID, TaskID: task.ID, AttemptID: attempt.ID,
		Kind: "integration", Content: []byte(report.Output), Passed: true,
	}); err != nil {
		a.fail(task, attempt, "record evidence", err.Error())
		return
	}
	if _, err := a.store.TransitionAttempt(a.baseCtx, attempt.ID, mission.AttemptSucceeded); err != nil {
		log.Print(logfmt.FormatMsg("integration", fmt.Sprintf("推进 attempt %s 失败: %v", attempt.ID, err)))
	}
	if _, err := a.store.TransitionTask(a.baseCtx, task.ID, mission.TaskVerifying); err != nil {
		log.Print(logfmt.FormatMsg("integration", fmt.Sprintf("推进 task %s 失败: %v", task.ID, err)))
		return
	}
	if _, err := a.store.TransitionTask(a.baseCtx, task.ID, mission.TaskSucceeded); err != nil {
		log.Print(logfmt.FormatMsg("integration", fmt.Sprintf("推进 task %s 失败: %v", task.ID, err)))
	}
}

// fail records failing Evidence against the Integration Task itself (there
// is no separate "target" Task the way Verifier has one — Integration IS the
// terminal Task for whatever it depends on), marks it failed, and escalates
// the whole Mission to needs_attention. Called only from within complete,
// whose own defer still handles releasing the lease — fail itself must not
// touch the lease.
func (a *Adapter) fail(task mission.Task, attempt mission.TaskAttempt, kind, detail string) {
	if _, err := a.store.AddEvidence(a.baseCtx, mission.CreateEvidenceInput{
		MissionID: task.MissionID, TaskID: task.ID, AttemptID: attempt.ID,
		Kind: "integration", Content: []byte(fmt.Sprintf("%s: %s", kind, detail)), Passed: false,
	}); err != nil {
		log.Print(logfmt.FormatMsg("integration", fmt.Sprintf("记录失败 evidence 失败: %v", err)))
	}
	if _, err := a.store.TransitionAttempt(a.baseCtx, attempt.ID, mission.AttemptFailed); err != nil {
		log.Print(logfmt.FormatMsg("integration", fmt.Sprintf("推进 attempt %s 失败: %v", attempt.ID, err)))
	}
	if _, err := a.store.TransitionTask(a.baseCtx, task.ID, mission.TaskFailed); err != nil {
		log.Print(logfmt.FormatMsg("integration", fmt.Sprintf("推进 task %s 失败: %v", task.ID, err)))
	}
	if _, err := a.store.MarkMissionNeedsAttention(a.baseCtx, task.MissionID, fmt.Sprintf("%s: %s", kind, detail)); err != nil {
		log.Print(logfmt.FormatMsg("integration", fmt.Sprintf("升级 mission %s 到 needs_attention 失败: %v", task.MissionID, err)))
	}
}
