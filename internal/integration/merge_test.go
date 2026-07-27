package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// newTestRepo creates a fresh git repository in a temp dir with one initial
// commit — a minimal but valid, self-contained Go module (own go.mod, not
// harness9's own module). Reused by later tasks' tests, which run real `go
// build`/`go vet`/`go test` against worktrees of this repo.
func newTestRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	runGit(t, root, "init", "-q")
	runGit(t, root, "config", "user.email", "integration-test@example.com")
	runGit(t, root, "config", "user.name", "integration-test")
	files := map[string]string{
		"go.mod":       "module integrationtestfixture\n\ngo 1.25\n",
		"main.go":      "package main\n\nfunc main() {}\n",
		"main_test.go": "package main\n\nimport \"testing\"\n\nfunc TestMainOK(t *testing.T) {}\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runGit(t, root, "add", "-A")
	runGit(t, root, "commit", "-q", "-m", "initial commit")
	return root
}

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
	// Change main.go in the integration worktree itself.
	if err := os.WriteFile(filepath.Join(integratePath, "main.go"), []byte("package main\n\nfunc main() { println(\"from integrate\") }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, integratePath, "add", "-A")
	runGit(t, integratePath, "commit", "-q", "-m", "feat: change main from integrate")

	branchPath := filepath.Join(repoRoot, ".harness9", "missions", "conflict")
	runGit(t, repoRoot, "worktree", "add", branchPath, "-b", "mission/conflict")
	// Change the exact same line of main.go on a diverging branch.
	if err := os.WriteFile(filepath.Join(branchPath, "main.go"), []byte("package main\n\nfunc main() { println(\"from conflict\") }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, branchPath, "add", "-A")
	runGit(t, branchPath, "commit", "-q", "-m", "feat: change main from conflict")

	err := MergeBranch(integratePath, "mission/conflict")
	if err == nil {
		t.Fatal("MergeBranch on a genuine conflict = nil error, want an error")
	}
	status := runGit(t, integratePath, "status", "--porcelain")
	if status != "" {
		t.Fatalf("worktree status after aborted merge = %q, want a clean tree (merge --abort should have restored it)", status)
	}
}
