package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadExistingIDsEmpty(t *testing.T) {
	ids, err := loadExistingIDs("/nonexistent/predictions.jsonl")
	if err != nil {
		t.Fatalf("want nil error for missing file, got %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("want empty map, got %v", ids)
	}
}

// TestLoadExistingIDs 验证 --resume 跳过逻辑：仅"已产出非空 patch"的实例算完成并被跳过；
// 空 patch（无改动 / 瞬时失败被记空）不计入，从而在续跑时获得重试机会。
func TestLoadExistingIDs(t *testing.T) {
	content := `{"instance_id":"django__django-1","model_patch":"diff ..."}
{"instance_id":"astropy__astropy-2","model_patch":""}
{"instance_id":"flask__flask-3","model_patch":"   "}
`
	tmp := filepath.Join(t.TempDir(), "predictions.jsonl")
	if err := os.WriteFile(tmp, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	ids, err := loadExistingIDs(tmp)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ids["django__django-1"] {
		t.Error("非空 patch 的实例应被视为已完成（跳过）")
	}
	// 空 / 纯空白 patch 不应被跳过——这些正是最该重跑的实例。
	if ids["astropy__astropy-2"] {
		t.Error("空 patch 实例不应被跳过，应在 --resume 时重试")
	}
	if ids["flask__flask-3"] {
		t.Error("纯空白 patch 实例不应被跳过，应在 --resume 时重试")
	}
	if ids["unknown"] {
		t.Error("want unknown NOT in existing IDs")
	}
}

func TestAppendPrediction(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "predictions.jsonl")

	p1 := Prediction{InstanceID: "django__django-1", ModelPatch: "diff line 1"}
	p2 := Prediction{InstanceID: "astropy__astropy-2", ModelPatch: ""}

	if err := appendPrediction(tmp, p1); err != nil {
		t.Fatalf("append p1 error: %v", err)
	}
	if err := appendPrediction(tmp, p2); err != nil {
		t.Fatalf("append p2 error: %v", err)
	}

	data, err := os.ReadFile(tmp)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d", len(lines))
	}

	var got Prediction
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatalf("unmarshal line 1: %v", err)
	}
	if got.InstanceID != "django__django-1" {
		t.Errorf("want django__django-1, got %s", got.InstanceID)
	}
}

func TestWriteSummary(t *testing.T) {
	outDir := t.TempDir()
	results := []RunResult{
		{Instance: Instance{InstanceID: "a", Repo: "django/django"}, Patch: "diff ...", Duration: time.Second},
		{Instance: Instance{InstanceID: "b", Repo: "django/django"}, Patch: "", Duration: time.Second},
		{Instance: Instance{InstanceID: "c", Repo: "astropy/astropy"}, Error: fmt.Errorf("clone failed"), Duration: time.Second},
	}
	start := time.Now().Add(-5 * time.Minute)
	end := time.Now()
	cfg := Config{OutputDir: outDir, Seed: 7, RunID: "20260619-000000"}
	if err := writeSummary(cfg, results, start, end); err != nil {
		t.Fatalf("writeSummary error: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(outDir, "run_summary.md"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.Contains(content, "总实例数：3") {
		t.Errorf("summary should contain total count 3, got:\n%s", content)
	}
	if !strings.Contains(content, "django/django") {
		t.Error("summary should contain django/django repo")
	}
	// 记录 RunID 与 seed 以支持复现
	if !strings.Contains(content, "20260619-000000") {
		t.Error("summary should record RunID")
	}
	if !strings.Contains(content, "seed: 7") {
		t.Error("summary should record sampling seed")
	}
}
