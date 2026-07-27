// Package integration implements the multi-Task consolidation half of
// Mission's execution loop: it merges every dependency Task's completed
// branch into one Mission-level worktree, independently re-verifies the
// combined result, and is the only component allowed to advance an
// Integration Task (and, indirectly, its whole Mission) to succeeded.
package integration

import (
	"fmt"
	"os/exec"
)

// MergeBranch merges branch into the repository checked out at worktreePath,
// using a non-fast-forward merge so multiple independent branches can be
// combined in sequence without losing their individual commit history. On
// conflict, it aborts the merge — restoring worktreePath to the clean state
// it was in before this call — and returns an error describing the conflict.
func MergeBranch(worktreePath, branch string) error {
	cmd := exec.Command("git", "merge", "--no-ff", "--no-edit", branch)
	cmd.Dir = worktreePath
	out, err := cmd.CombinedOutput()
	if err != nil {
		mergeErr := fmt.Errorf("git merge %s: %w: %s", branch, err, out)
		abortCmd := exec.Command("git", "merge", "--abort")
		abortCmd.Dir = worktreePath
		if abortOut, abortErr := abortCmd.CombinedOutput(); abortErr != nil {
			return fmt.Errorf("%w (merge --abort also failed: %v: %s)", mergeErr, abortErr, abortOut)
		}
		return mergeErr
	}
	return nil
}
