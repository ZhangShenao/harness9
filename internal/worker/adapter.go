package worker

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/harness9/internal/logfmt"
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

// var _ interface{ Dispatch(...) error } = (*Adapter)(nil) structurally checks
// that Adapter has the method shape scheduler.Dispatcher requires, without
// importing internal/scheduler and creating a needless dependency.
var _ interface {
	Dispatch(ctx context.Context, task mission.Task) error
} = (*Adapter)(nil)

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

// run executes the Attempt's Task Contract via a.executor and records the
// outcome. It always runs on a's baseCtx, not the ctx Dispatch's caller
// passed in, so it survives past the lifetime of any single Tick call.
func (a *Adapter) run(task mission.Task, lease mission.WorkspaceLease, attempt mission.TaskAttempt) {
	prompt := fmt.Sprintf("Task: %s\n\n%s", task.Title, task.Contract)
	subResult, err := a.executor.Execute(a.baseCtx, lease.Path, prompt)
	a.complete(task, attempt, lease, subResult, err)
}

// complete records the final outcome of one Attempt: on any failure (execution
// error, unparseable output, or a reported task failure) it records failing
// Evidence and marks the Task failed; on success it records the commit diff
// as an Artifact, passing Evidence, and advances the Task to verifying —
// independent re-verification is a later increment's responsibility, not
// this Adapter's.
func (a *Adapter) complete(
	task mission.Task,
	attempt mission.TaskAttempt,
	lease mission.WorkspaceLease,
	subResult subagent.SubAgentResult,
	execErr error,
) {
	defer func() {
		if _, err := a.store.ReleaseLease(a.baseCtx, lease.ID); err != nil {
			log.Print(logfmt.FormatMsg("worker", fmt.Sprintf("释放 lease %s 失败: %v", lease.ID, err)))
		}
	}()

	if execErr != nil {
		a.fail(task, attempt, "worker execution error", execErr.Error())
		return
	}
	result, err := ParseResult(subResult.FinalText)
	if err != nil {
		a.fail(task, attempt, "unparseable worker output", err.Error())
		return
	}
	if !result.Success {
		a.fail(task, attempt, "worker reported failure", result.Reason)
		return
	}

	diff := captureDiff(lease.Path, result.Commit)
	if _, err := a.store.AddArtifact(a.baseCtx, mission.CreateArtifactInput{
		MissionID: task.MissionID, TaskID: task.ID, AttemptID: attempt.ID,
		Kind: "commit_diff", Content: []byte(diff),
	}); err != nil {
		a.fail(task, attempt, "record artifact", err.Error())
		return
	}
	if _, err := a.store.AddEvidence(a.baseCtx, mission.CreateEvidenceInput{
		MissionID: task.MissionID, TaskID: task.ID, AttemptID: attempt.ID,
		Kind: "worker_report", Content: []byte(subResult.FinalText), Passed: true,
	}); err != nil {
		a.fail(task, attempt, "record evidence", err.Error())
		return
	}
	if _, err := a.store.TransitionAttempt(a.baseCtx, attempt.ID, mission.AttemptSucceeded); err != nil {
		a.fail(task, attempt, "transition attempt", err.Error())
		return
	}
	if _, err := a.store.TransitionTask(a.baseCtx, task.ID, mission.TaskVerifying); err != nil {
		a.fail(task, attempt, "transition task", err.Error())
		return
	}
}

// fail records a single failing Evidence entry describing what went wrong,
// then marks the Attempt and Task failed. Store errors during this best-
// effort cleanup path are logged, not escalated further — there is no
// caller left to propagate them to once Dispatch has already returned.
func (a *Adapter) fail(task mission.Task, attempt mission.TaskAttempt, kind, detail string) {
	if _, err := a.store.AddEvidence(a.baseCtx, mission.CreateEvidenceInput{
		MissionID: task.MissionID, TaskID: task.ID, AttemptID: attempt.ID,
		Kind: "worker_report", Content: []byte(fmt.Sprintf("%s: %s", kind, detail)), Passed: false,
	}); err != nil {
		log.Print(logfmt.FormatMsg("worker", fmt.Sprintf("记录失败 evidence 失败: %v", err)))
	}
	if _, err := a.store.TransitionAttempt(a.baseCtx, attempt.ID, mission.AttemptFailed); err != nil {
		log.Print(logfmt.FormatMsg("worker", fmt.Sprintf("推进 attempt %s 到 failed 失败: %v", attempt.ID, err)))
	}
	if _, err := a.store.TransitionTask(a.baseCtx, task.ID, mission.TaskFailed); err != nil {
		log.Print(logfmt.FormatMsg("worker", fmt.Sprintf("推进 task %s 到 failed 失败: %v", task.ID, err)))
	}
}

// captureDiff returns the full patch of one commit inside worktreePath, for
// storage as an Artifact. If git itself fails (e.g. an invalid commit sha),
// the error text is captured as the artifact content instead of losing the
// failure silently — Store.AddArtifact requires non-empty content, and a
// visible error is more useful downstream than a fabricated empty diff.
func captureDiff(worktreePath, commit string) string {
	cmd := exec.Command("git", "show", commit)
	cmd.Dir = worktreePath
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Sprintf("git show %s failed: %v\n%s", commit, err, out)
	}
	return string(out)
}
