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
// mockProvider 是线程安全的：使用 atomic.Int32 管理内部状态，支持并发调用。
// Generate 和 GenerateStream 共享同一套 simulateResponse 逻辑，确保行为一致。
type mockProvider struct {
	// turn 记录非 Thinking 模式下的调用次数。
	// Thinking 阶段的调用（tools=nil）不计入 turn。
	turn atomic.Int32
}

// Generate 实现 LLMProvider 接口的阻塞式调用。
// 委托给 simulateResponse 生成确定性响应。
func (m *mockProvider) Generate(_ context.Context, _ []schema.Message, tools []schema.ToolDefinition) (*schema.Message, error) {
	return m.simulateResponse(tools), nil
}

// GenerateStream 实现 LLMProvider 接口的流式调用。
// 将 simulateResponse 的结果拆分为 StreamChunk 序列通过 channel 发送：
//   - 文本内容 → StreamChunkTextDelta（一次性发送全部文本）
//   - 工具调用 → StreamChunkToolCallStart + StreamChunkToolCallDelta
//   - 结束 → StreamChunkDone（含完整 Message）
//
// 这是一个简化的流式模拟：不逐 token 发送，而是一次性发送完整文本。
// 对于测试而言足够，因为测试关心的是事件类型和顺序，而非增量粒度。
func (m *mockProvider) GenerateStream(ctx context.Context, _ []schema.Message, tools []schema.ToolDefinition) (<-chan schema.StreamChunk, error) {
	msg := m.simulateResponse(tools)

	ch := make(chan schema.StreamChunk)
	go func() {
		defer close(ch)

		if msg.Content != "" {
			if !sendStreamChunk(ctx, ch, schema.StreamChunk{
				Type:  schema.StreamChunkTextDelta,
				Delta: msg.Content,
			}) {
				return
			}
		}

		for i, tc := range msg.ToolCalls {
			if !sendStreamChunk(ctx, ch, schema.StreamChunk{
				Type: schema.StreamChunkToolCallStart,
				ToolCall: &schema.ToolCallDelta{
					Index: i,
					ID:    tc.ID,
					Name:  tc.Name,
				},
			}) {
				return
			}
			if !sendStreamChunk(ctx, ch, schema.StreamChunk{
				Type: schema.StreamChunkToolCallDelta,
				ToolCall: &schema.ToolCallDelta{
					Index:     i,
					Arguments: tc.Arguments,
				},
			}) {
				return
			}
		}

		sendStreamChunk(ctx, ch, schema.StreamChunk{
			Type:    schema.StreamChunkDone,
			Message: msg,
		})
	}()
	return ch, nil
}

// simulateResponse 根据 tools 参数和内部 turn 计数器生成确定性响应。
// 行为：
//   - tools 为空（Thinking 阶段）：返回深度思考文本
//   - tools 非空，turn==1：返回 bash 工具调用请求
//   - tools 非空，turn>1：返回纯文本最终回复（触发 loop 终止）
func (m *mockProvider) simulateResponse(tools []schema.ToolDefinition) *schema.Message {
	if len(tools) == 0 {
		return &schema.Message{
			Role:    schema.RoleAssistant,
			Content: "【深度思考】目标是检查文件。我不能直接盲猜，我需要先调用 bash 工具执行 ls 命令，看看当前目录下有什么，然后再做定夺。",
		}
	}

	t := m.turn.Add(1)
	if t == 1 {
		return &schema.Message{
			Role:    schema.RoleAssistant,
			Content: "我要执行我刚才规划的步骤了。",
			ToolCalls: []schema.ToolCall{
				{ID: "call_123", Name: "bash", Arguments: []byte(`{"command": "ls -la"}`)},
			},
		}
	}

	return &schema.Message{
		Role:    schema.RoleAssistant,
		Content: "根据工具返回的结果，我看到了 main.go，任务圆满完成！",
	}
}

// NewMockProvider 构造并返回一个新的 mockProvider 实例。
// turn 计数器从 0 开始，确保首次调用（Thinking 阶段除外）触发 ToolCall 响应。
func NewMockProvider() LLMProvider {
	return &mockProvider{}
}
