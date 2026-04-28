// Package schema — 流式数据类型。
// 本文件定义 Provider 层的流式增量类型（StreamChunk、ToolCallDelta），
// 用于 GenerateStream 方法通过 channel 逐 chunk 传递 LLM 响应增量。
package schema

import "encoding/json"

// StreamChunkType 枚举了 LLM 流式响应中可能出现的增量 chunk 类型。
// 每种类型对应 LLM 流式输出过程中的一个语义阶段，形成完整的流式生命周期：
//
//	text_delta ──────────────────────┐    (多次，逐 token 文本)
//	                                  │
//	tool_call_start ─── tool_call_delta ──┤    (每个工具调用重复此模式)
//	                                  │
//	                                  ▼
//	                               done      (流结束，携带完整 Message)
type StreamChunkType string

const (
	// StreamChunkTextDelta 表示一段文本增量（token-by-token）。
	// 每次收到此 chunk 时，Delta 字段包含一个 token 的文本内容。
	// 引擎层会将其映射为 EventThinkingDelta 或 EventActionDelta 事件。
	StreamChunkTextDelta StreamChunkType = "text_delta"

	// StreamChunkToolCallStart 表示一个新的工具调用开始。
	// 此时 ToolCall 字段包含工具调用的 Index、ID 和 Name，
	// 但参数尚未开始传输（Arguments 为空）。
	StreamChunkToolCallStart StreamChunkType = "tool_call_start"

	// StreamChunkToolCallDelta 表示工具调用参数的 JSON 增量。
	// ToolCall.Arguments 包含一段部分 JSON 文本，需要累积拼接后
	// 才能得到完整的参数 JSON。多个 delta chunk 按顺序拼接即可。
	StreamChunkToolCallDelta StreamChunkType = "tool_call_delta"

	// StreamChunkDone 表示流式响应结束。Message 字段携带完整的响应 Message
	// （含累积完成的 Content 文本和 ToolCalls 列表），供引擎直接注入到
	// 对话上下文中，无需再次组装。
	StreamChunkDone StreamChunkType = "done"

	// StreamChunkError 表示流式过程中发生错误。Error 字段包含错误描述。
	// 收到此 chunk 后，流可能立即关闭，不应再期待后续 chunk。
	StreamChunkError StreamChunkType = "error"
)

// StreamChunk 是 LLM 流式响应的单个增量单元。Provider 通过 GenerateStream 方法
// 返回 <-chan StreamChunk，引擎和消费者从 channel 中逐个读取以实现实时输出。
//
// 每个 chunk 的有效字段取决于 Type：
//
//	Type == text_delta         → Delta 有效，包含一个 token 的文本
//	Type == tool_call_start    → ToolCall 有效，包含 Index、ID、Name
//	Type == tool_call_delta    → ToolCall 有效，包含 Index、Arguments（部分 JSON）
//	Type == done               → Message 有效，包含完整的响应 Message
//	Type == error              → Error 有效，包含错误描述
type StreamChunk struct {
	// Type 标识此 chunk 的类型，决定其他字段的有效性。
	Type StreamChunkType `json:"type"`

	// Delta 文本增量。仅在 Type == text_delta 时有效，包含一个 token 的文本。
	// 多次 text_delta 的 Delta 拼接即为完整的文本响应。
	Delta string `json:"delta,omitempty"`

	// ToolCall 工具调用增量信息。在 Type 为 tool_call_start 或 tool_call_delta 时有效。
	// 工具调用的完整信息需要通过 Index 关联多次 chunk 来累积。
	ToolCall *ToolCallDelta `json:"tool_call,omitempty"`

	// Message 完整的响应消息。仅在 Type == done 时有效。
	// Provider 在流结束时将累积的完整 Message 放入此字段，
	// 引擎可直接使用此 Message 注入到对话上下文中。
	Message *Message `json:"message,omitempty"`

	// Error 错误描述。仅在 Type == error 时有效。
	Error string `json:"error,omitempty"`
}

// ToolCallDelta 描述工具调用的增量信息。LLM 在流式模式下，每个工具调用的
// 传输分为两个阶段：
//
//  1. tool_call_start — 包含 Index、ID、Name，标识一个新的工具调用开始
//  2. tool_call_delta — 包含 Index、Arguments（部分 JSON），逐步传输参数
//
// 消费者需要按 Index 累积同一个工具调用的所有 delta，最终拼接出完整的参数 JSON。
type ToolCallDelta struct {
	// Index 工具调用在数组中的位置索引。LLM 可能同时发起多个并行工具调用，
	// Index 用于将增量 chunk 关联到正确的工具调用。值从 0 开始递增。
	Index int `json:"index"`

	// ID 工具调用的唯一标识符。仅在 tool_call_start chunk 中有效。
	// 用于后续将 ToolResult 与原始 ToolCall 关联（Request-Response Matching）。
	ID string `json:"id,omitempty"`

	// Name 目标工具在 Registry 中的标识符（如 "bash"、"read_file"）。
	// 仅在 tool_call_start chunk 中有效。
	Name string `json:"name,omitempty"`

	// Arguments 工具调用参数的部分 JSON 文本。仅在 tool_call_delta chunk 中有效。
	// 多个 delta 的 Arguments 按顺序拼接后得到完整的 JSON 参数。
	// 使用 json.RawMessage 延迟解析，与 ToolCall.Arguments 设计一致。
	Arguments json.RawMessage `json:"arguments,omitempty"`
}
