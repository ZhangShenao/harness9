package worker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func initTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"git", "init"},
		{"git", "config", "user.email", "test@test.com"},
		{"git", "config", "user.name", "test"},
		{"git", "checkout", "-b", "main"},
	} {
		c := exec.Command(args[0], args[1:]...)
		c.Dir = dir
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s: %v", args, out, err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("init"), 0644); err != nil {
		t.Fatal(err)
	}
	c := exec.Command("git", "add", ".")
	c.Dir = dir
	c.Run()
	c = exec.Command("git", "commit", "-m", "init")
	c.Dir = dir
	c.Run()
	return dir
}

func TestCreateAndRemoveWorktree(t *testing.T) {
	repoDir := initTestRepo(t)
	wtPath := filepath.Join(t.TempDir(), "wt")
	ctx := context.Background()

	if err := CreateWorktree(ctx, repoDir, wtPath, "mission/test-branch"); err != nil {
		t.Fatal(err)
	}
	if _, err := filepath.Glob(filepath.Join(wtPath, "README.md")); err != nil {
		t.Fatalf("worktree files missing: %v", err)
	}
	if err := RemoveWorktree(ctx, repoDir, wtPath); err != nil {
		t.Fatal(err)
	}
}

func TestCreateWorktreeDuplicateBranch(t *testing.T) {
	repoDir := initTestRepo(t)
	wtPath := filepath.Join(t.TempDir(), "wt")
	ctx := context.Background()
	c := exec.Command("git", "branch", "mission/dup")
	c.Dir = repoDir
	c.Run()
	if err := CreateWorktree(ctx, repoDir, wtPath, "mission/dup"); err == nil {
		t.Fatal("expected error for duplicate branch")
	}
}

func TestParseResultValid(t *testing.T) {
	output := "Some work done.\n```json\n{\"commit\": \"abc123\", \"files\": [\"a.go\"], \"summary\": \"done\"}\n```"
	r, err := ParseResult(output)
	if err != nil {
		t.Fatal(err)
	}
	if r.Commit != "abc123" || len(r.Files) != 1 || r.Summary != "done" {
		t.Fatalf("parsed = %+v", r)
	}
}

func TestParseResultMissing(t *testing.T) {
	_, err := ParseResult("no result here")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestParseResultBareJSON(t *testing.T) {
	r, err := ParseResult(`{"commit": "def456", "files": [], "summary": "empty"}`)
	if err != nil {
		t.Fatal(err)
	}
	if r.Commit != "def456" {
		t.Fatalf("commit = %q", r.Commit)
	}
}
