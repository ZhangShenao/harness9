// Package verifier implements the independent re-verification half of
// Mission's execution loop: it re-runs a completed implementation Task's
// build/test/vet checks from a fresh, detached checkout of the same commit,
// and is the only component allowed to advance a Task from verifying to
// succeeded or failed.
package verifier

import (
	"fmt"
	"os/exec"
)

// CreateDetachedWorktree creates a new git worktree at path checked out at
// ref in detached HEAD state. Unlike a normal `git worktree add <path> <ref>`,
// this does not require ref to be free — it works even when ref (typically a
// branch name) is already checked out in another worktree, since detached
// HEAD only resolves ref to a commit once and never claims the branch itself.
func CreateDetachedWorktree(repoRoot, path, ref string) error {
	cmd := exec.Command("git", "worktree", "add", "--detach", path, ref)
	cmd.Dir = repoRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git worktree add --detach %s %s: %w: %s", path, ref, err, out)
	}
	return nil
}

// RemoveWorktree force-removes a git worktree previously created by
// CreateDetachedWorktree.
func RemoveWorktree(repoRoot, path string) error {
	cmd := exec.Command("git", "worktree", "remove", path, "--force")
	cmd.Dir = repoRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git worktree remove %s: %w: %s", path, err, out)
	}
	return nil
}
