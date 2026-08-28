// Package skills — UseSkillTool：Progressive Disclosure 的执行层工具。
// 本文件实现 use_skill 工具，LLM 通过调用它按名称加载指定 skill 的全文内容，
// 避免 System Prompt 在启动时整体注入所有 skill 正文。
package skills

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/harness9/internal/schema"
)

// UseSkillTool 使 LLM 能够按需加载指定 skill 的全文内容（Progressive Disclosure 执行层）。
// 通过 Go 结构类型（Structural Typing）隐式满足 tools.BaseTool 接口，无需 import tools 包。
type UseSkillTool struct {
	index *Index
}

// NewUseSkillTool 创建绑定到指定 Index 的 use_skill 工具。
func NewUseSkillTool(index *Index) *UseSkillTool {
	return &UseSkillTool{index: index}
}

// Name 返回工具标识符 "use_skill"。
func (t *UseSkillTool) Name() string { return "use_skill" }

// Definition 返回工具元信息，包含描述和参数 JSON Schema。
func (t *UseSkillTool) Definition() schema.ToolDefinition {
	return schema.ToolDefinition{
		Name:        "use_skill",
		Description: "加载指定技能的完整内容。当需要执行特定领域任务时，先调用此工具获取对应技能的完整指导内容。",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"skill_name": map[string]interface{}{
					"type":        "string",
					"description": "要加载的技能名称（来自 System Prompt 技能索引中的 name 字段）",
				},
			},
			"required": []string{"skill_name"},
		},
	}
}

type useSkillArgs struct {
	SkillName string `json:"skill_name"`
}

// Execute 加载并返回指定 skill 的全文内容。
func (t *UseSkillTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	var a useSkillArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("参数解析失败：%w", err)
	}
	if a.SkillName == "" {
		return "", fmt.Errorf("skill_name 不能为空")
	}
	return t.index.GetFullContent(a.SkillName)
}
