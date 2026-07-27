package worker

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestCreateWorktreeAddsWorktreeAtPath(t *testing.T) {
	repoRoot := newTestRepo(t)
	worktreePath := filepath.Join(repoRoot, ".harness9", "missions", "m1", "task-a")

	if err := CreateWorktree(repoRoot, worktreePath, "mission/m1/task-a"); err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}

	if _, err := os.Stat(filepath.Join(worktreePath, ".git")); err != nil {
		t.Fatalf("worktree %s does not look like a git worktree: %v", worktreePath, err)
	}
	branch := runGit(t, worktreePath, "branch", "--show-current")
	if branch != "mission/m1/task-a" {
		t.Fatalf("branch = %q, want mission/m1/task-a", branch)
	}
}

func TestCreateWorktreeRejectsDuplicatePath(t *testing.T) {
	repoRoot := newTestRepo(t)
	worktreePath := filepath.Join(repoRoot, ".harness9", "missions", "m1", "task-a")

	if err := CreateWorktree(repoRoot, worktreePath, "mission/m1/task-a"); err != nil {
		t.Fatalf("first CreateWorktree: %v", err)
	}
	if err := CreateWorktree(repoRoot, worktreePath, "mission/m1/task-a-2"); err == nil {
		t.Fatal("second CreateWorktree at the same path succeeded, want an error")
	}
}

func TestRemoveWorktreeCleansUpPath(t *testing.T) {
	repoRoot := newTestRepo(t)
	worktreePath := filepath.Join(repoRoot, ".harness9", "missions", "m1", "task-a")
	if err := CreateWorktree(repoRoot, worktreePath, "mission/m1/task-a"); err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}

	if err := RemoveWorktree(repoRoot, worktreePath); err != nil {
		t.Fatalf("RemoveWorktree: %v", err)
	}
	if _, err := os.Stat(worktreePath); !os.IsNotExist(err) {
		t.Fatalf("worktree path still exists after RemoveWorktree: err = %v", err)
	}
}

// newTestRepo creates a fresh git repository in a temp dir with one initial
// commit (so later `git worktree add -b <branch>` calls have a start point),
// and local git identity configured (so test commits succeed without relying
// on the host machine's global git config). Reused by later tasks' tests.
func newTestRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	runGit(t, root, "init", "-q")
	runGit(t, root, "config", "user.email", "worker-test@example.com")
	runGit(t, root, "config", "user.name", "worker-test")
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("test repo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", "-A")
	runGit(t, root, "commit", "-q", "-m", "initial commit")
	return root
}

// runGit runs a git command in dir and returns trimmed stdout, failing the
// test with full output on error.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return trimTrailingNewline(string(out))
}

func trimTrailingNewline(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
