// Package provider 抽象了 harness9 引擎与各种 LLM 后端（OpenAI、Anthropic、Google 等）
// 之间的通信层。使用方面向 LLMProvider 接口编程，无需修改引擎逻辑即可切换具体实现。
package provider

import (
	"context"

	"github.com/harness9/internal/schema"
)

// LLMProvider 定义了与大型语言模型交互的统一契约。实现封装了 API 特定细节，
// 包括认证、端点解析、请求/响应映射、流式传输和重试策略。
//
// 引擎在 agent loop 的每个 Turn 中调用 Generate 或 GenerateStream，传入完整的对话上下文
// 和当前可用的工具集合。Provider 返回一条 assistant Message，可能包含纯文本推理、
// 一个或多个工具调用请求，或两者的组合。
//
// 双模式设计：
//   - Generate: 阻塞式调用，同步返回完整响应，适用于批处理或不需要实时输出的场景
//   - GenerateStream: 流式调用，通过 channel 逐 chunk 返回增量，适用于实时交互场景
//
// 两个方法共享相同的消息转换逻辑（convertMessages / convertTools），
// 只是底层 SDK 调用方式不同（New vs NewStreaming）。
type LLMProvider interface {
	// Generate 将对话历史和可用工具定义发送给 LLM，返回模型的完整响应 Message。
	//
	// 参数:
	//   - ctx: 控制底层 HTTP 调用的取消和超时
	//   - messages: 完整的对话上下文，包含 system prompt、之前的 user/assistant 消息、
	//     以及工具 Observation
	//   - availableTools: 当前 Turn 中模型可调用的工具定义列表；
	//     传入 nil 剥夺所有工具（Phase 1 Thinking），传入非空恢复工具（Phase 2 Action）
	//
	// 返回的 Message 中 ToolCalls 字段在模型决定调用工具时填充；否则 Content 包含
	// 最终文本回复，agent loop 终止。
	Generate(ctx context.Context, messages []schema.Message, availableTools []schema.ToolDefinition) (*schema.Message, error)

	// GenerateStream 以流式方式调用 LLM，通过 channel 逐 chunk 返回响应增量。
	//
	// 参数与 Generate 完全一致。返回的 channel 中每个 chunk 代表 LLM 的一次增量产出：
	//   - StreamChunkTextDelta: 文本增量（逐 token）
	//   - StreamChunkToolCallStart: 新工具调用开始（含 ID、Name）
	//   - StreamChunkToolCallDelta: 工具参数 JSON 增量
	//   - StreamChunkDone: 流结束，携带完整的 Message
	//   - StreamChunkError: 出错
	//
	// 返回的 channel 会在流结束时自动关闭。调用方必须从 channel 读取直到关闭，
	// 以确保底层 HTTP 连接被正确释放。
	GenerateStream(ctx context.Context, messages []schema.Message, availableTools []schema.ToolDefinition) (<-chan schema.StreamChunk, error)
}

// sendStreamChunk 向 StreamChunk channel 发送 chunk，同时感知 context 取消。
// 使用 select 监听 ctx.Done()，确保在 context 被取消时（如超时或手动中断）
// 不会阻塞在 channel 发送上，避免 goroutine 泄漏。
//
// 返回 false 表示 context 已取消，调用方应立即退出当前 goroutine。
func sendStreamChunk(ctx context.Context, ch chan<- schema.StreamChunk, chunk schema.StreamChunk) bool {
	select {
	case <-ctx.Done():
		return false
	case ch <- chunk:
		return true
	}
}
