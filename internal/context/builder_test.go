package context

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/harness9/internal/skills"
)

func TestBuild_BasePromptOnly(t *testing.T) {
	dir := t.TempDir()
	// skills 目录不存在 → 空 Index
	idx, _ := skills.LoadSkills(filepath.Join(dir, "skills"))

	b := NewPromptBuilder(dir, idx)
	prompt := b.Build()

	if !strings.Contains(prompt, "harness9") {
		t.Error("prompt should contain 'harness9'")
	}
	if !strings.Contains(prompt, dir) {
		t.Error("prompt should contain workDir")
	}
	if strings.Contains(prompt, "项目规范") {
		t.Error("prompt should not contain AGENTS.md section when file absent")
	}
	if strings.Contains(prompt, "可用 Skills") {
		t.Error("prompt should not contain skills section when index is empty")
	}
}

func TestBuild_WithAgentsMd(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("# Project Guide\n\nAlways write tests first."), 0644); err != nil {
		t.Fatal(err)
	}
	idx, _ := skills.LoadSkills(filepath.Join(dir, "skills"))

	b := NewPromptBuilder(dir, idx)
	prompt := b.Build()

	if !strings.Contains(prompt, "项目规范") {
		t.Error("prompt should contain AGENTS.md section header")
	}
	if !strings.Contains(prompt, "Always write tests first.") {
		t.Error("prompt should contain AGENTS.md content")
	}
}

func TestBuild_WithSkills(t *testing.T) {
	dir := t.TempDir()
	skillsDir := filepath.Join(dir, "skills")
	if err := os.Mkdir(skillsDir, 0755); err != nil {
		t.Fatal(err)
	}
	skillSubDir := filepath.Join(skillsDir, "go-refactor")
	if err := os.Mkdir(skillSubDir, 0755); err != nil {
		t.Fatal(err)
	}
	skillContent := "---\nname: go-refactor\ndescription: Go refactoring guide\n---\n\nAlways run go vet first."
	if err := os.WriteFile(filepath.Join(skillSubDir, "SKILL.md"), []byte(skillContent), 0644); err != nil {
		t.Fatal(err)
	}

	idx, err := skills.LoadSkills(skillsDir)
	if err != nil {
		t.Fatal(err)
	}

	b := NewPromptBuilder(dir, idx)
	prompt := b.Build()

	if !strings.Contains(prompt, "可用 Skills") {
		t.Error("prompt should contain skills section header")
	}
	if !strings.Contains(prompt, "go-refactor: Go refactoring guide") {
		t.Error("prompt should contain skill index entry")
	}
	// Progressive Disclosure：skill 全文不能出现在 System Prompt 中
	if strings.Contains(prompt, "Always run go vet first.") {
		t.Error("prompt must NOT contain skill body content (progressive disclosure violated)")
	}
}

func TestBuild_NilSkillsIndex(t *testing.T) {
	dir := t.TempDir()
	b := NewPromptBuilder(dir, nil)
	prompt := b.Build()
	if !strings.Contains(prompt, "harness9") {
		t.Error("prompt should contain 'harness9' even with nil skills index")
	}
}

func TestBuildInjectsLongTermMemory(t *testing.T) {
	b := NewPromptBuilder(t.TempDir(), nil).WithLongTermMemory(func() string { return "## 偏好\n用户偏好中文" })
	out := b.Build()
	if !strings.Contains(out, "用户偏好中文") {
		t.Errorf("system prompt 应注入长期记忆内容: %s", out)
	}
}

func TestBuildSkipsEmptyLongTermMemory(t *testing.T) {
	b := NewPromptBuilder(t.TempDir(), nil).WithLongTermMemory(func() string { return "" })
	out := b.Build()
	if strings.Contains(out, "长期记忆") {
		t.Error("空长期记忆不应注入标题段落")
	}
}

func TestBuild_WithSandboxContext(t *testing.T) {
	b := NewPromptBuilder(t.TempDir(), nil).WithSandboxContext(true)
	out := b.Build()
	if !strings.Contains(out, "Sandbox 执行环境") {
		t.Error("启用 Sandbox 时 system prompt 应包含 Sandbox 执行环境 Section")
	}
	if !strings.Contains(out, "apt-get") {
		t.Error("Sandbox Section 应提示可用 apt-get 安装工具")
	}
	if !strings.Contains(out, "先安装后验证") {
		t.Error("Sandbox Section 应明确要求先安装缺失工具再验证")
	}
}

func TestBuild_WithoutSandboxContext(t *testing.T) {
	b := NewPromptBuilder(t.TempDir(), nil)
	out := b.Build()
	if strings.Contains(out, "Sandbox 执行环境") {
		t.Error("未启用 Sandbox 时 system prompt 不应包含 Sandbox 执行环境 Section")
	}
}

// TestBuild_PlanningSection 验证规划准则段落的注入开关：
// WithPlanEnabled(true) 时包含准则要点；未启用时整段缺失（规划是按需注入的能力提示）。
func TestBuild_PlanningSection(t *testing.T) {
	b := NewPromptBuilder(t.TempDir(), nil).WithPlanEnabled(true)
	out := b.Build()
	for _, want := range []string{"## 规划（Planning）", "plan_write", "简单任务"} {
		if !strings.Contains(out, want) {
			t.Errorf("prompt should contain %q, got: %s", want, out)
		}
	}
	// 未启用时不注入
	out2 := NewPromptBuilder(t.TempDir(), nil).Build()
	if strings.Contains(out2, "## 规划（Planning）") {
		t.Error("planning section should be absent when disabled")
	}
}
