package provider

import (
	"context"

	"github.com/harness9/internal/schema"
)

// mockProvider 是 LLMProvider 的确定性桩实现，用于集成测试和早期开发。
// 它模拟一个两轮对话：
//
//	Turn 1 — "模型" 发出 bash 工具调用来检查文件系统
//	Turn 2 — "模型" 返回最终文本回复，agent loop 终止
//
// 这使得引擎的 agent loop 可以在不依赖真实 LLM API Key 或网络访问的情况下
// 进行端到端验证。
type mockProvider struct {
	// turn 记录 Generate 被调用的次数，用于在不同调用轮次返回不同的响应。
	turn int
}

// Generate 模拟 LLM 响应周期。首次调用时请求 bash 工具调用；第二次调用时
// 返回最终文本回复，使 agent loop 自然退出。
func (m *mockProvider) Generate(ctx context.Context, msgs []schema.Message, _ []schema.ToolDefinition) (*schema.Message, error) {
	m.turn++

	// Turn 1: 模拟 Reasoning 后发出 ToolCall 请求
	if m.turn == 1 {
		return &schema.Message{
			Role:    schema.RoleAssistant,
			Content: "让我来看看当前目录下有什么文件。",
			ToolCalls: []schema.ToolCall{
				{ID: "call_123", Name: "bash", Arguments: []byte(`{"command": "ls -la"}`)},
			},
		}, nil
	}

	// Turn 2: 模拟模型在收到上一轮工具 Observation 后产出最终回复
	return &schema.Message{
		Role:    schema.RoleAssistant,
		Content: "我看到了文件列表，里面包含 main.go，任务完成！",
	}, nil
}

// NewMockProvider 构造并返回一个新的 mockProvider 实例。turn 计数器从 0 开始，
// 确保首次 Generate 调用触发 ToolCall 响应。
func NewMockProvider() LLMProvider {
	return &mockProvider{turn: 0}
}
