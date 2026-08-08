package worker

import (
	"context"
	"fmt"
	"os/exec"
)

// CreateWorktree creates a git worktree at path with a new branch.
func CreateWorktree(ctx context.Context, repoDir, path, branch string) error {
	if repoDir == "" || path == "" || branch == "" {
		return fmt.Errorf("repoDir, path and branch are required")
	}
	cmd := exec.CommandContext(ctx, "git", "worktree", "add", "-b", branch, path)
	cmd.Dir = repoDir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git worktree add %s: %w: %s", path, err, out)
	}
	return nil
}

// RemoveWorktree removes a git worktree from the repo.
func RemoveWorktree(ctx context.Context, repoDir, path string) error {
	if path == "" {
		return nil
	}
	cmd := exec.CommandContext(ctx, "git", "worktree", "remove", "--force", path)
	cmd.Dir = repoDir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git worktree remove %s: %w: %s", path, err, out)
	}
	return nil
}
