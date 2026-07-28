package worker

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMergeBranchMergesNonConflictingChange(t *testing.T) {
	repoRoot := newTestRepo(t)
	integratePath := filepath.Join(repoRoot, ".harness9", "integrate", "task")
	runGit(t, repoRoot, "worktree", "add", integratePath, "-b", "integrate/task")

	branchPath := filepath.Join(repoRoot, ".harness9", "missions", "a")
	runGit(t, repoRoot, "worktree", "add", branchPath, "-b", "mission/a")
	if err := os.WriteFile(filepath.Join(branchPath, "feature_a.go"), []byte("package main\n\nfunc FeatureA() string { return \"a\" }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, branchPath, "add", "-A")
	runGit(t, branchPath, "commit", "-q", "-m", "feat: add feature a")

	if err := MergeBranch(integratePath, "mission/a"); err != nil {
		t.Fatalf("MergeBranch: %v", err)
	}
	if _, err := os.Stat(filepath.Join(integratePath, "feature_a.go")); err != nil {
		t.Fatalf("merged worktree missing feature_a.go: %v", err)
	}
}

func TestMergeBranchAbortsOnConflictAndLeavesCleanState(t *testing.T) {
	repoRoot := newTestRepo(t)
	integratePath := filepath.Join(repoRoot, ".harness9", "integrate", "task")
	runGit(t, repoRoot, "worktree", "add", integratePath, "-b", "integrate/task")
	// Change README.md in the integration worktree itself.
	if err := os.WriteFile(filepath.Join(integratePath, "README.md"), []byte("from integrate\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, integratePath, "add", "-A")
	runGit(t, integratePath, "commit", "-q", "-m", "feat: change readme from integrate")

	branchPath := filepath.Join(repoRoot, ".harness9", "missions", "conflict")
	runGit(t, repoRoot, "worktree", "add", branchPath, "-b", "mission/conflict")
	// Change the exact same line of README.md on a diverging branch.
	if err := os.WriteFile(filepath.Join(branchPath, "README.md"), []byte("from conflict\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, branchPath, "add", "-A")
	runGit(t, branchPath, "commit", "-q", "-m", "feat: change readme from conflict")

	err := MergeBranch(integratePath, "mission/conflict")
	if err == nil {
		t.Fatal("MergeBranch on a genuine conflict = nil error, want an error")
	}
	status := runGit(t, integratePath, "status", "--porcelain")
	if status != "" {
		t.Fatalf("worktree status after aborted merge = %q, want a clean tree (merge --abort should have restored it)", status)
	}
}
