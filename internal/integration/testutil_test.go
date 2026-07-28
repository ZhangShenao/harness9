package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// newTestRepo creates a fresh git repository in a temp dir with one initial
// commit — a minimal but valid, self-contained Go module (own go.mod, not
// harness9's own module). Reused across this package's tests, which run
// real `go build`/`go vet`/`go test` against worktrees of this repo.
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
