package subagent

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadFromDirMissingIsSilent(t *testing.T) {
	r := NewRegistry()
	if err := r.LoadFromDir(filepath.Join(t.TempDir(), "nonexist")); err != nil {
		t.Fatalf("目录不存在应静默返回 nil，得 %v", err)
	}
	if len(r.List()) != 0 {
		t.Fatal("不应加载任何定义")
	}
}

func TestLoadFromDirLoadsAgents(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "reviewer.md", `---
name: reviewer
description: 审查
tools: read_file
---
审查正文`)
	writeFile(t, dir, "notmarkdown.txt", "ignored")

	r := NewRegistry()
	if err := r.LoadFromDir(dir); err != nil {
		t.Fatal(err)
	}
	got, ok := r.Get("reviewer")
	if !ok {
		t.Fatal("未加载 reviewer")
	}
	if got.Source == "" {
		t.Error("Source 应记录文件路径")
	}
}

func TestLoadFromDirNameFallbackToFilename(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "my-agent.md", `---
description: 无 name 字段
---
正文`)
	r := NewRegistry()
	if err := r.LoadFromDir(dir); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Get("my-agent"); !ok {
		t.Fatal("缺 name 时应回退到文件名")
	}
}

func TestLoadFromDirSkipsInvalid(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "bad.md", "no frontmatter here")
	writeFile(t, dir, "good.md", `---
name: good
description: ok
---
正文`)
	r := NewRegistry()
	if err := r.LoadFromDir(dir); err != nil {
		t.Fatal(err)
	}
	if len(r.List()) != 1 || r.List()[0].Name != "good" {
		t.Fatalf("应只加载合法定义：%+v", r.List())
	}
}

func TestLoadFromDirOverridesProgrammatic(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "reviewer.md", `---
name: reviewer
description: 文件版
---
文件正文`)
	r := NewRegistry()
	_ = r.Register(SubAgentDefinition{Name: "reviewer", Description: "编程版", SystemPrompt: "p"})
	if err := r.LoadFromDir(dir); err != nil {
		t.Fatal(err)
	}
	got, _ := r.Get("reviewer")
	if got.Description != "文件版" {
		t.Fatalf("文件定义应覆盖编程式定义，得 %q", got.Description)
	}
}
