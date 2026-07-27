package verifier

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// newTestRepo creates a fresh git repository in a temp dir with one initial
// commit and local git identity configured. The initial commit is a minimal
// but valid, self-contained Go module (own go.mod, not related to harness9's
// own module) — Task 5's tests run real `go build`/`go vet`/`go test` inside
// worktrees of this repo, so it must actually be a buildable, testable
// module, not just arbitrary files. Reused by later tasks' tests.
func newTestRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	runGit(t, root, "init", "-q")
	runGit(t, root, "config", "user.email", "verifier-test@example.com")
	runGit(t, root, "config", "user.name", "verifier-test")
	files := map[string]string{
		"go.mod":       "module verifiertestfixture\n\ngo 1.25\n",
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

func TestCreateDetachedWorktreeChecksOutRefEvenIfCheckedOutElsewhere(t *testing.T) {
	repoRoot := newTestRepo(t)
	implPath := filepath.Join(repoRoot, ".harness9", "missions", "impl")
	runGit(t, repoRoot, "worktree", "add", implPath, "-b", "impl-branch")
	if err := os.WriteFile(filepath.Join(implPath, "output.txt"), []byte("done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, implPath, "add", "-A")
	runGit(t, implPath, "commit", "-q", "-m", "feat: implementation work")

	verifyPath := filepath.Join(repoRoot, ".harness9", "verify", "task")
	if err := CreateDetachedWorktree(repoRoot, verifyPath, "impl-branch"); err != nil {
		t.Fatalf("CreateDetachedWorktree: %v", err)
	}

	if _, err := os.Stat(filepath.Join(verifyPath, "output.txt")); err != nil {
		t.Fatalf("verifier worktree missing the implementer's commit: %v", err)
	}
	branch := runGit(t, verifyPath, "branch", "--show-current")
	if branch != "" {
		t.Fatalf("verifier worktree branch = %q, want detached (empty)", branch)
	}
}

func TestRemoveWorktreeCleansUpVerifierWorktree(t *testing.T) {
	repoRoot := newTestRepo(t)
	path := filepath.Join(repoRoot, ".harness9", "verify", "task")
	if err := CreateDetachedWorktree(repoRoot, path, "HEAD"); err != nil {
		t.Fatalf("CreateDetachedWorktree: %v", err)
	}
	if err := RemoveWorktree(repoRoot, path); err != nil {
		t.Fatalf("RemoveWorktree: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("worktree path still exists after RemoveWorktree: err = %v", err)
	}
}
