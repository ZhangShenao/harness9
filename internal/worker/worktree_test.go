package worker

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/harness9/internal/mission"
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

// TestWorktreeForKeysOffTaskIDNotClientID is a regression test for a
// permanent-collision bug: worktreeFor used to key off task.ClientID
// (falling back to task.ID only when ClientID was empty). ClientID is only
// unique within one (mission_id, plan_version) — internal/mission's Plan
// Change Request flow can create a new Plan version whose Tasks legally
// reuse a ClientID from an earlier version, each getting a fresh, globally
// unique task.ID (see insertPlanGraphTx's newID() call in
// internal/mission/plan_store.go). Two such Tasks must resolve to distinct,
// non-colliding worktree paths/branches, and worktreeFor must key off
// task.ID unconditionally to guarantee that.
func TestWorktreeForKeysOffTaskIDNotClientID(t *testing.T) {
	repoRoot := newTestRepo(t)
	taskV1 := mission.Task{ID: "task-id-v1", MissionID: "m1", ClientID: "task-a"}
	taskV2 := mission.Task{ID: "task-id-v2", MissionID: "m1", ClientID: "task-a"}

	pathV1, branchV1 := worktreeFor(repoRoot, taskV1)
	pathV2, branchV2 := worktreeFor(repoRoot, taskV2)

	if pathV1 == pathV2 {
		t.Fatalf("worktreeFor produced identical paths for two Tasks with different IDs sharing a reused ClientID: %s", pathV1)
	}
	if branchV1 == branchV2 {
		t.Fatalf("worktreeFor produced identical branches for two Tasks with different IDs sharing a reused ClientID: %s", branchV1)
	}

	if err := CreateWorktree(repoRoot, pathV1, branchV1); err != nil {
		t.Fatalf("CreateWorktree for the first Task: %v", err)
	}
	if err := CreateWorktree(repoRoot, pathV2, branchV2); err != nil {
		t.Fatalf("CreateWorktree for the second Task must not collide with the first despite the reused ClientID: %v", err)
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
