package main

import (
	"strings"
	"testing"
)

func TestSwebenchPromptBuilder(t *testing.T) {
	inst := Instance{
		InstanceID:       "django__django-99",
		ProblemStatement: "There is a bug in QuerySet.filter() when using complex Q objects.",
	}
	b := &swebenchPromptBuilder{instance: inst, workDir: "/tmp/swebench-test"}
	prompt := b.Build()

	// 占位符注入
	if !strings.Contains(prompt, inst.ProblemStatement) {
		t.Error("prompt should contain the problem statement")
	}
	if strings.Contains(prompt, "{{PROBLEM_STATEMENT}}") {
		t.Error("prompt should not contain unreplaced placeholder")
	}
	if !strings.Contains(prompt, "/tmp/swebench-test") {
		t.Error("prompt should contain the injected workDir")
	}
	if strings.Contains(prompt, "{{WORK_DIR}}") {
		t.Error("prompt should not contain unreplaced WORK_DIR placeholder")
	}

	// 结构化工作流
	if !strings.Contains(prompt, "Step 1") || !strings.Contains(prompt, "Step 5") {
		t.Error("prompt should contain structured workflow steps")
	}

	// 约束：不修改测试文件
	if !strings.Contains(prompt, "Never modify test files") {
		t.Error("prompt should contain constraint about not modifying test files")
	}

	// 英文推理 + 单行防漂移
	if !strings.Contains(prompt, "Reason and respond in English") {
		t.Error("prompt should mandate English reasoning with anti-drift line")
	}

	// 相对路径强约束（修复 safePath 路径翻倍的诱因）
	if !strings.Contains(prompt, "relative to the working directory") {
		t.Error("prompt should require relative paths for file tools")
	}
	if !strings.Contains(prompt, "NEVER pass an absolute path") {
		t.Error("prompt should forbid absolute paths to file tools")
	}

	// 行为验证而非语法验证（核心修复：杜绝 plausible-but-wrong 过度修复）
	if !strings.Contains(prompt, "does NOT prove the behavior is correct") {
		t.Error("prompt should reframe verification as behavioral, not syntactic")
	}
	// 禁止内联重抄自测的假验证
	if !strings.Contains(prompt, "re-implementing the class/function inline") {
		t.Error("prompt should forbid fake verification via inline re-implementation")
	}

	// 阅读现有测试（最强行为信号）
	if !strings.Contains(prompt, "never modify — the existing tests") {
		t.Error("prompt should encourage reading existing tests")
	}

	// heredoc 跑复现，禁止在仓库内建临时文件污染 patch
	if !strings.Contains(prompt, "heredoc") {
		t.Error("prompt should instruct running repros via heredoc")
	}
	if !strings.Contains(prompt, "corrupt the final patch") {
		t.Error("prompt should warn that scratch files corrupt the patch")
	}

	// read_file 行号模式 + 并发探索
	if !strings.Contains(prompt, "start_line") {
		t.Error("prompt should mention read_file start_line parameter")
	}
	if !strings.Contains(prompt, "multiple tool calls in parallel") {
		t.Error("prompt should encourage concurrent tool calls")
	}

	// timeout_secs 放宽慢命令
	if !strings.Contains(prompt, "timeout_secs") {
		t.Error("prompt should mention per-call timeout_secs for slow commands")
	}

	// 不应再保留旧的"放弃验证"反模式
	if strings.Contains(prompt, "diff 即为权威确认") || strings.Contains(prompt, "验证至多 1-2 步") {
		t.Error("prompt should no longer contain the discourage-verification anti-pattern")
	}
}

// TestSwebenchPrompt_InjectsHints 验证 hints_text 被注入 prompt（R3：维护者讨论常含决定性
// API 设计，如 flask 的 text=True；此前被解析却从未注入，是最强信号被静默丢弃）。
func TestSwebenchPrompt_InjectsHints(t *testing.T) {
	inst := Instance{InstanceID: "x", ProblemStatement: "P", HintsText: "maintainers chose text=True over mode"}
	b := &swebenchPromptBuilder{instance: inst, workDir: "/w"}
	p := b.Build()
	if !strings.Contains(p, "maintainers chose text=True over mode") {
		t.Error("prompt should inject hints_text content")
	}
	if strings.Contains(p, "{{HINTS}}") {
		t.Error("prompt should not contain unreplaced HINTS placeholder")
	}
	if !strings.Contains(p, "Maintainer hints") {
		t.Error("prompt should label the injected hints section")
	}
}

// TestSwebenchPrompt_OmitsHintsSectionWhenEmpty 验证无 hints 时优雅省略整段（不留空标题/占位符）。
func TestSwebenchPrompt_OmitsHintsSectionWhenEmpty(t *testing.T) {
	b := &swebenchPromptBuilder{instance: Instance{ProblemStatement: "P"}, workDir: "/w"}
	p := b.Build()
	if strings.Contains(p, "{{HINTS}}") {
		t.Error("empty hints: should not contain unreplaced placeholder")
	}
	if strings.Contains(p, "Maintainer hints") {
		t.Error("empty hints: section header should be omitted entirely")
	}
}

// TestSwebenchPrompt_MandatesRealVerification 验证 prompt 重平衡（R5/R7）：
// 不再把"退化为静态分析"写成默认逃生门，并加入最小化/错误点局部修复偏置与 deprecation 约定提示。
func TestSwebenchPrompt_MandatesRealVerification(t *testing.T) {
	b := &swebenchPromptBuilder{instance: Instance{ProblemStatement: "P"}, workDir: "/w"}
	p := b.Build()
	// R5：删除"默认退化为静态分析"反模式
	if strings.Contains(p, "fall back to careful static analysis of the real source") {
		t.Error("prompt should no longer license static analysis as the default fallback")
	}
	// R7：最小化、错误点局部修复偏置
	if !strings.Contains(p, "smallest change") {
		t.Error("prompt should bias toward the smallest change at the error site")
	}
	// R7：deprecation 约定提示（xarray-4493：gold 发 DeprecationWarning 而非静默改行为）
	if !strings.Contains(p, "DeprecationWarning") {
		t.Error("prompt should hint at the project's deprecation-warning convention")
	}
}

// TestPlanningHintSection 验证 P1-1 路线 A：复杂任务（长问题描述或环境重量高的
// repo）注入规划建议，简单任务零干扰。
func TestPlanningHintSection(t *testing.T) {
	longProblem := strings.Repeat("复杂问题描述。", 300) // 1800 字符，超阈值
	tests := []struct {
		name     string
		inst     Instance
		wantHint bool
	}{
		{name: "长问题描述触发", inst: Instance{Repo: "django/django", ProblemStatement: longProblem}, wantHint: true},
		{name: "环境重量 repo 触发", inst: Instance{Repo: "astropy/astropy", ProblemStatement: "short"}, wantHint: true},
		{name: "简单任务零干扰", inst: Instance{Repo: "django/django", ProblemStatement: "short bug"}, wantHint: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := (&swebenchPromptBuilder{instance: tt.inst, workDir: "/tmp"}).planningHintSection()
			if (got != "") != tt.wantHint {
				t.Fatalf("planningHintSection() = %q, wantHint = %v", got, tt.wantHint)
			}
		})
	}
}
