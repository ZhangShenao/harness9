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
// 引擎在 agent loop 的每个 Turn 中调用 Generate，传入完整的对话上下文和当前可用的
// 工具集合。Provider 返回一条 assistant Message，可能包含纯文本推理、一个或多个
// 工具调用请求，或两者的组合。
type LLMProvider interface {
	// Generate 将对话历史和可用工具定义发送给 LLM，返回模型的响应 Message。
	//
	// 参数:
	//   - ctx: 控制底层 HTTP 调用的取消和超时
	//   - messages: 完整的对话上下文，包含 system prompt、之前的 user/assistant 消息、
	//     以及工具 Observation
	//   - availableTools: 当前 Turn 中模型可调用的工具定义列表
	//
	// 返回的 Message 中 ToolCalls 字段在模型决定调用工具时填充；否则 Content 包含
	// 最终文本回复，agent loop 终止。
	Generate(ctx context.Context, messages []schema.Message, availableTools []schema.ToolDefinition) (*schema.Message, error)
}
