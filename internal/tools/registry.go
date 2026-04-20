// Package tools 提供 harness9 agent 框架的工具注册表 (Registry) 抽象及内置工具实现。
// Registry 接口将工具发现与工具执行解耦，使引擎可以在不了解具体工具实现的前提下
// 查询可用工具列表并分发调用。
package tools

import (
	"context"

	"github.com/harness9/internal/schema"
)

// Registry 定义了工具注册表的契约 — 负责枚举可用工具并执行工具调用的组件。
//
// 引擎在每个 agent loop Turn 中与 Registry 交互两次：
//  1. 调用 LLM 之前，获取 ToolDefinition 列表，使模型了解可调用哪些工具
//  2. LLM 发出 ToolCall 后，通过 Execute 分发每个调用并收集结果作为 Observation
type Registry interface {
	// GetAvailableTools 返回当前注册的所有工具定义。这些定义在每个 Turn 中
	// 转发给 LLM Provider，使模型能够决定调用哪些工具以及传递什么参数。
	GetAvailableTools() []schema.ToolDefinition

	// Execute 在本地执行单个工具调用并返回结果。
	// call 参数携带工具名称、参数和用于关联的唯一 ID。
	// 实现应：
	//   - 根据 call.Name 查找目标工具
	//   - 按工具的 Input Schema 反序列化参数
	//   - 执行工具逻辑（如运行 shell 命令、编辑文件等）
	//   - 返回包含输出和错误状态的 ToolResult
	Execute(ctx context.Context, call schema.ToolCall) schema.ToolResult
}
