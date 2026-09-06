package hooks_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/harness9/internal/hooks"
	"github.com/harness9/internal/planning"
)

func TestFilePlanWriter_NonGitProject_WritesToHomeDir(t *testing.T) {
	workDir := t.TempDir() // 无 .git，非 git 项目
	homeDir := t.TempDir()

	pw, err := hooks.NewFilePlanWriter(workDir, homeDir, "abcdef12-0000-0000-0000-000000000000")
	if err != nil {
		t.Fatal(err)
	}

	plans := []planning.PlanItem{
		{ID: "1", Content: "step one", Status: planning.PlanPending},
		{ID: "2", Content: "step two", Status: planning.PlanCompleted},
	}
	if err := pw.Write(plans); err != nil {
		t.Fatalf("Write error: %v", err)
	}

	// File should be under homeDir/.harness9/plans/
	plansDir := filepath.Join(homeDir, ".harness9", "plans")
	entries, err := os.ReadDir(plansDir)
	if err != nil {
		t.Fatalf("plans dir not created: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 plan file, got %d", len(entries))
	}

	content, _ := os.ReadFile(filepath.Join(plansDir, entries[0].Name()))
	if !strings.Contains(string(content), "[ ] step one") {
		t.Errorf("plan should contain pending task, got:\n%s", content)
	}
	if !strings.Contains(string(content), "[x] step two") {
		t.Errorf("plan should contain completed task, got:\n%s", content)
	}
}

func TestFilePlanWriter_GitProject_WritesToWorkDir(t *testing.T) {
	workDir := t.TempDir()
	homeDir := t.TempDir()
	// 创建 .git 目录使其成为 git 项目
	if err := os.Mkdir(filepath.Join(workDir, ".git"), 0700); err != nil {
		t.Fatal(err)
	}

	pw, err := hooks.NewFilePlanWriter(workDir, homeDir, "sess-git-0000")
	if err != nil {
		t.Fatal(err)
	}

	if err := pw.Write([]planning.PlanItem{{ID: "1", Content: "git task", Status: planning.PlanInProgress}}); err != nil {
		t.Fatal(err)
	}

	plansDir := filepath.Join(workDir, ".harness9", "plans")
	entries, err := os.ReadDir(plansDir)
	if err != nil {
		t.Fatalf("workdir plans dir not created: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 plan file in workdir, got %d", len(entries))
	}
	content, _ := os.ReadFile(filepath.Join(plansDir, entries[0].Name()))
	if !strings.Contains(string(content), "[>] git task") {
		t.Errorf("plan should contain in_progress task, got:\n%s", content)
	}
}

func TestFilePlanWriter_Overwrite(t *testing.T) {
	workDir := t.TempDir()
	homeDir := t.TempDir()

	pw, err := hooks.NewFilePlanWriter(workDir, homeDir, "sess-overwrite")
	if err != nil {
		t.Fatal(err)
	}

	pw.Write([]planning.PlanItem{{ID: "1", Content: "first", Status: planning.PlanPending}})
	pw.Write([]planning.PlanItem{{ID: "1", Content: "first", Status: planning.PlanCompleted}})

	// Should still be one file (overwritten)
	plansDir := filepath.Join(homeDir, ".harness9", "plans")
	entries, _ := os.ReadDir(plansDir)
	if len(entries) != 1 {
		t.Fatalf("expected 1 plan file after overwrite, got %d", len(entries))
	}
	content, _ := os.ReadFile(filepath.Join(plansDir, entries[0].Name()))
	if !strings.Contains(string(content), "[x] first") {
		t.Errorf("overwritten plan should show completed status, got:\n%s", content)
	}
}

func TestFilePlanWriter_AllStatuses(t *testing.T) {
	workDir := t.TempDir()
	homeDir := t.TempDir()
	pw, _ := hooks.NewFilePlanWriter(workDir, homeDir, "sess-status")

	plans := []planning.PlanItem{
		{ID: "1", Content: "pending task", Status: planning.PlanPending},
		{ID: "2", Content: "active task", Status: planning.PlanInProgress},
		{ID: "3", Content: "done task", Status: planning.PlanCompleted},
		{ID: "4", Content: "dropped task", Status: planning.PlanCancelled},
	}
	if err := pw.Write(plans); err != nil {
		t.Fatal(err)
	}

	plansDir := filepath.Join(homeDir, ".harness9", "plans")
	entries, _ := os.ReadDir(plansDir)
	content, _ := os.ReadFile(filepath.Join(plansDir, entries[0].Name()))
	s := string(content)

	checks := map[string]string{
		"[ ] pending task": s,
		"[>] active task":  s,
		"[x] done task":    s,
		"[-] dropped task": s,
	}
	for marker, body := range checks {
		if !strings.Contains(body, marker) {
			t.Errorf("plan missing %q\n---\n%s", marker, body)
		}
	}
}
