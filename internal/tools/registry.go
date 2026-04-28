// Package tools 提供 harness9 agent 框架的工具注册表（Registry）抽象及内置工具实现。
// Registry 接口将工具发现（Tool Discovery）与工具执行（Tool Execution）解耦，使引擎
// 可以在不了解具体工具实现的前提下查询可用工具列表并分发调用。
//
// 工具通过实现 BaseTool 接口注册到 Registry 中。每个工具需要提供名称、参数定义
// （JSON Schema）和执行逻辑。引擎在每个 agent loop Turn 中与 Registry 交互两次：
//
//  1. 调用 LLM 之前，获取 ToolDefinition 列表，使模型了解可调用哪些工具
//  2. LLM 发出 ToolCall 后，通过 Execute 分发每个调用并收集结果作为 Observation（观察）
package tools

import (
	"context"
	"fmt"
	"log"

	"github.com/harness9/internal/schema"
)

// Registry 定义了工具注册表（Tool Registry）的接口，负责工具的注册、发现与执行。
// 引擎通过此接口与工具系统交互，实现关注点分离：
// 引擎关注循环编排，Registry 关注工具生命周期管理。
type Registry interface {
	// Register 将一个 BaseTool 实现注册到工具表中。
	// 若注册同名工具，后注册的会覆盖先注册的。
	Register(tool BaseTool)

	// GetAvailableTools 返回所有已注册工具的 ToolDefinition 列表，
	// 供 LLM 在 Generate 调用时了解可用工具集。
	GetAvailableTools() []schema.ToolDefinition

	// Execute 根据 ToolCall 中的工具名称查找并执行对应工具，
	// 返回封装后的 ToolResult（包含输出或错误信息）。
	Execute(ctx context.Context, call schema.ToolCall) schema.ToolResult
}

// registryImpl 是 Registry 接口的默认实现，使用 map 存储工具实例。
// 工具名称作为键，确保查找的时间复杂度为 O(1)。
type registryImpl struct {
	// tools 以工具名称为键的映射表，存储所有已注册的 BaseTool 实现。
	tools map[string]BaseTool
}

// NewRegistry 创建并返回一个空的工具注册表实例。
func NewRegistry() Registry {
	return &registryImpl{
		tools: make(map[string]BaseTool),
	}
}

// Register 将工具注册到注册表中。如果同名工具已存在，会记录日志并覆盖旧实现。
func (r *registryImpl) Register(tool BaseTool) {
	name := tool.Name()
	if _, exists := r.tools[name]; exists {
		log.Printf("[Registry] 工具 '%s' 已被注册，将被覆盖", name)
	}
	r.tools[name] = tool
	log.Printf("[Registry] 成功挂载工具: %s", name)
}

// GetAvailableTools 遍历注册表，收集所有工具的 ToolDefinition 并返回。
// 返回的列表顺序不固定（map 迭代顺序不确定）。
func (r *registryImpl) GetAvailableTools() []schema.ToolDefinition {
	defs := make([]schema.ToolDefinition, 0, len(r.tools))
	for _, tool := range r.tools {
		defs = append(defs, tool.Definition())
	}
	return defs
}

// Execute 根据工具名称查找并执行对应工具。若工具不存在，返回包含错误信息的 ToolResult。
// 工具执行失败时，错误会被捕获并封装为 IsError=true 的 ToolResult，
// 使引擎能够将错误信息作为 Observation 回传给 LLM，支持自愈（Self-Healing）重试。
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
