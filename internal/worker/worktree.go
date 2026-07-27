// Package worker implements the real scheduler.Dispatcher: it provisions an
// isolated git worktree + WorkspaceLease per Task Attempt and runs the
// default implementation Task Contract inside it.
package worker

import (
	"fmt"
	"os/exec"
)

// CreateWorktree creates a new git worktree at path on a new branch, using
// repoRoot's current HEAD as the start point. It fails if path already
// exists or branch is already checked out elsewhere.
func CreateWorktree(repoRoot, path, branch string) error {
	cmd := exec.Command("git", "worktree", "add", path, "-b", branch)
	cmd.Dir = repoRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git worktree add %s: %w: %s", path, err, out)
	}
	return nil
}

// RemoveWorktree force-removes a git worktree previously created by
// CreateWorktree, including any uncommitted changes inside it.
func RemoveWorktree(repoRoot, path string) error {
	cmd := exec.Command("git", "worktree", "remove", path, "--force")
	cmd.Dir = repoRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git worktree remove %s: %w: %s", path, err, out)
	}
	return nil
}
