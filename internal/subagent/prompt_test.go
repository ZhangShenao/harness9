package subagent

import (
	"strings"
	"testing"
)

func TestPromptBuilderBuild(t *testing.T) {
	pb := newPromptBuilder("你是审查专家。", "/work", nil, nil)
	got := pb.Build()
	if !strings.Contains(got, "你是审查专家。") {
		t.Error("应包含 system prompt 正文")
	}
	if !strings.Contains(got, "/work") {
		t.Error("应包含工作目录")
	}
	if !strings.Contains(got, "## 规划（Planning）") || !strings.Contains(got, "plan_write") {
		t.Error("应包含规划准则段落（子代理拥有独立 plan_write）")
	}
}

func TestPromptBuilderWithSkills(t *testing.T) {
	loader := func(name string) (string, error) {
		return "技能正文-" + name, nil
	}
	pb := newPromptBuilder("base", "/work", []string{"api-conventions"}, loader)
	got := pb.Build()
	if !strings.Contains(got, "技能正文-api-conventions") {
		t.Errorf("应预加载并注入 skill 正文，得: %s", got)
	}
}

func TestPromptBuilderSkillLoadErrorIgnored(t *testing.T) {
	loader := func(name string) (string, error) {
		return "", errSkillMissing
	}
	pb := newPromptBuilder("base", "/work", []string{"missing"}, loader)
	if got := pb.Build(); !strings.Contains(got, "base") {
		t.Error("skill 加载失败应被忽略，base 仍在")
	}
}

func TestPromptBuilderWithSandboxContext(t *testing.T) {
	pb := newPromptBuilder("你是助手。", "/work", nil, nil).WithSandboxContext(true)
	got := pb.Build()
	if !strings.Contains(got, "## Sandbox 执行环境") {
		t.Error("启用 sandbox context 时应包含 Sandbox 环节标题")
	}
	if !strings.Contains(got, "Docker 容器（Ubuntu 22.04）") {
		t.Error("应包含容器描述")
	}
	if !strings.Contains(got, "完全隔离的临时环境") {
		t.Error("应包含隔离说明")
	}
	if !strings.Contains(got, "root") {
		t.Error("应包含权限说明")
	}
	if !strings.Contains(got, "apt-get") {
		t.Error("应包含工具安装说明")
	}
}

func TestPromptBuilderWithoutSandboxContext(t *testing.T) {
	pb := newPromptBuilder("你是助手。", "/work", nil, nil).WithSandboxContext(false)
	got := pb.Build()
	if strings.Contains(got, "## Sandbox 执行环境") {
		t.Error("禁用 sandbox context 时不应包含 Sandbox 环节")
	}
}

func TestPromptBuilderSandboxContextChaining(t *testing.T) {
	pb := newPromptBuilder("你是助手。", "/work", nil, nil)
	result := pb.WithSandboxContext(true).Build()
	if !strings.Contains(result, "## Sandbox 执行环境") {
		t.Error("WithSandboxContext 应支持链式调用")
	}
}
