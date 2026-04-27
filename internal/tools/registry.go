// Package tools 提供 harness9 agent 框架的工具注册表（Registry）抽象及内置工具实现。
// Registry 接口将工具发现与工具执行解耦，使引擎可以在不了解具体工具实现的前提下
// 查询可用工具列表并分发调用。
//
// 工具通过实现 BaseTool 接口注册到 Registry 中。每个工具需要提供名称、参数定义（JSON Schema）
// 和执行逻辑。引擎在每个 agent loop Turn 中与 Registry 交互两次：
//
//  1. 调用 LLM 之前，获取 ToolDefinition 列表，使模型了解可调用哪些工具
//  2. LLM 发出 ToolCall 后，通过 Execute 分发每个调用并收集结果作为 Observation
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/harness9/internal/schema"
)

type BaseTool interface {
	Name() string
	Definition() schema.ToolDefinition
	Execute(ctx context.Context, args json.RawMessage) (string, error)
}

type Registry interface {
	Register(tool BaseTool)
	GetAvailableTools() []schema.ToolDefinition
	Execute(ctx context.Context, call schema.ToolCall) schema.ToolResult
}

type registryImpl struct {
	tools map[string]BaseTool
}

func NewRegistry() Registry {
	return &registryImpl{
		tools: make(map[string]BaseTool),
	}
}

func (r *registryImpl) Register(tool BaseTool) {
	name := tool.Name()
	if _, exists := r.tools[name]; exists {
		log.Printf("[Registry] 工具 '%s' 已被注册，将被覆盖", name)
	}
	r.tools[name] = tool
	log.Printf("[Registry] 成功挂载工具: %s", name)
}

func (r *registryImpl) GetAvailableTools() []schema.ToolDefinition {
	var defs []schema.ToolDefinition
	for _, tool := range r.tools {
		defs = append(defs, tool.Definition())
	}
	return defs
}

func (r *registryImpl) Execute(ctx context.Context, call schema.ToolCall) schema.ToolResult {
	tool, exists := r.tools[call.Name]
	if !exists {
		errMsg := fmt.Sprintf("Error: 系统中不存在名为 '%s' 的工具。", call.Name)
		return schema.ToolResult{
			ToolCallID: call.ID,
			Output:     errMsg,
			IsError:    true,
		}
	}

	output, err := tool.Execute(ctx, call.Arguments)

	if err != nil {
		return schema.ToolResult{
			ToolCallID: call.ID,
			Output:     fmt.Sprintf("Error executing %s: %v", call.Name, err),
			IsError:    true,
		}
	}

	return schema.ToolResult{
		ToolCallID: call.ID,
		Output:     output,
		IsError:    false,
	}
}
