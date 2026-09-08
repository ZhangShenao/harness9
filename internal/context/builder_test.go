package context

import (
	"os"
	"path/filepath"
	"runtime"
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

// TestBuild_WithSandboxDegraded 验证降级说明注入：Sandbox 已启用但启动失败时，
// prompt 必须如实告知 Agent 当前运行在宿主机本地（含原因与真实 OS），
// 且不再注入容器环境说明——降级后谎报"运行在 Ubuntu 容器"会误导 Agent
// 高估隔离边界、误判操作系统（线上事故：Agent 依据陈旧记忆以为是 Ubuntu 沙箱）。
func TestBuild_WithSandboxDegraded(t *testing.T) {
	b := NewPromptBuilder(t.TempDir(), nil).
		WithSandboxContext(true).
		WithSandboxDegraded("Docker Sandbox 启动失败：daemon 不可用")
	out := b.Build()

	if !strings.Contains(out, "执行环境（Sandbox 已降级") {
		t.Error("降级时 system prompt 应包含降级说明 Section")
	}
	if !strings.Contains(out, "daemon 不可用") {
		t.Error("降级说明应包含启动失败原因")
	}
	if !strings.Contains(out, runtime.GOOS) {
		t.Errorf("降级说明应注入宿主机真实 OS (%s)", runtime.GOOS)
	}
	if !strings.Contains(out, "真实系统") {
		t.Error("降级说明应警示操作直接影响真实系统")
	}
	if strings.Contains(out, "Docker 容器（Ubuntu 22.04）") {
		t.Error("降级时不应再注入容器环境说明（实际并未运行在容器内）")
	}
}
