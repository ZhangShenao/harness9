package provider

import (
	"context"
	"sync/atomic"

	"github.com/harness9/internal/schema"
)

// mockProvider 是 LLMProvider 的确定性桩实现，用于集成测试和早期开发。
//
// 它模拟一个启用 Two-Stage ReAct 的完整对话流程：
//
//	Thinking 调用 (tools=nil) → 模型进行深度思考
//	Action 调用 1 (tools=[bash]) → 模型发出 bash 工具调用
//	Action 调用 2 (tools=[bash]) → 模型返回最终文本回复，agent loop 终止
//
// 这使得引擎的 agent loop 可以在不依赖真实 LLM API Key 或网络访问的情况下
// 进行端到端验证，包括 Two-Stage ReAct 的 Thinking 阶段。
//
// mockProvider 是线程安全的：使用 atomic.Int32 管理内部状态，支持并发 Generate 调用。
type mockProvider struct {
	// turn 记录非 Thinking 模式下的 Generate 调用次数。
	// Thinking 阶段的调用（tools=nil）不计入 turn。
	turn atomic.Int32
}

// Generate 模拟 LLM 响应周期。行为根据传入的工具列表和调用次数变化：
//
//   - tools 为空（Thinking 阶段）：返回一段深度思考文本，模拟模型在没有工具
//     的情况下进行推理和规划
//   - tools 非空，turn==1：模拟首次行动 — 返回 bash 工具调用请求
//   - tools 非空，turn>1：模拟最终回复 — 返回纯文本，触发 loop 终止
func (m *mockProvider) Generate(_ context.Context, _ []schema.Message, tools []schema.ToolDefinition) (*schema.Message, error) {
	if len(tools) == 0 {
		return &schema.Message{
			Role:    schema.RoleAssistant,
			Content: "【深度思考】目标是检查文件。我不能直接盲猜，我需要先调用 bash 工具执行 ls 命令，看看当前目录下有什么，然后再做定夺。",
		}, nil
	}

	t := m.turn.Add(1)
	if t == 1 {
		return &schema.Message{
			Role:    schema.RoleAssistant,
			Content: "我要执行我刚才规划的步骤了。",
			ToolCalls: []schema.ToolCall{
				{ID: "call_123", Name: "bash", Arguments: []byte(`{"command": "ls -la"}`)},
			},
		}, nil
	}

	return &schema.Message{
		Role:    schema.RoleAssistant,
		Content: "根据工具返回的结果，我看到了 main.go，任务圆满完成！",
	}, nil
}

// NewMockProvider 构造并返回一个新的 mockProvider 实例。turn 计数器从 0 开始，
// 确保首次 Generate 调用（Thinking 阶段除外）触发 ToolCall 响应。
func NewMockProvider() LLMProvider {
	return &mockProvider{}
}
