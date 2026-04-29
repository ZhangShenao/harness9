// Package schema 定义了 harness9 agent loop 中使用的核心数据类型。
// 所有组件 — provider、engine、tool registry、memory — 均基于此包中声明的
// 类型进行交互，确保系统各层之间的消息契约一致。
package schema

import "encoding/json"

// Role 枚举了多轮对话（Multi-Turn Conversation）中可能出现的参与者角色，
// 遵循主流 Chat Completion API（OpenAI、Anthropic、Google 等）使用的
// system / user / assistant 三元组。
type Role string

const (
	// RoleSystem 系统提示词（System Prompt）：定义 agent 的性格、约束和行为边界，
	// 通常在会话开始时注入一次。LLM 在所有后续 Turn 中都会参考此消息。
	RoleSystem Role = "system"

	// RoleUser 用户角色（User）：包含人类操作者的输入 Prompt，以及工具执行后回传的
	// Observation（观察结果），供模型进行下一轮 Reasoning（推理）。
	RoleUser Role = "user"

	// RoleAssistant 模型输出角色（Assistant）：一条 assistant 消息可能包含纯文本推理
	// （Reasoning）、一个或多个工具调用请求（Parallel ToolCall），或两者的组合。
	RoleAssistant Role = "assistant"

	// RoleTool 工具执行结果角色（Tool Observation）：工具调用完成后，引擎将 ToolResult
	// 以 RoleTool 消息形式追加到上下文。Provider 适配器负责将此角色映射到各家 API 的
	// 原生工具结果格式（OpenAI 的 role=tool，Anthropic 的 tool_result block）。
	//
	// 历史上 harness9 使用 RoleUser + ToolCallID != "" 来表达"这是 Observation"，
	// 但把工具结果和人类输入混在同一角色里既模糊语义，也让 Anthropic 的 is_error 字段
	// 无处安放。RoleTool 显式表达"这是工具观察结果"，配合 Message.IsError 可完整透传
	// 工具失败信号给 LLM，便于模型自愈（Self-Healing）重试。
	RoleTool Role = "tool"
)

// Message 是对话上下文的基本单元。每个 Turn 会将新消息追加到 Context History 中，
// 并在下一次调用 LLM Provider 时整体传入。
//
// 消息可以代表任意角色。当 assistant 决定调用工具时，ToolCalls 切片会被填充；
// 当工具执行完毕后，结果会以 ToolCallID 标记的 Message 形式回传，以便模型将
// Observation 与原始请求关联起来。
type Message struct {
	// Role 标识消息的作者角色（system、user 或 assistant）。
	Role Role `json:"role"`

	// Content 存放消息的纯文本部分。对于 assistant 消息，若模型仅发出 ToolCall
	// 而没有文本推理，此字段可能为空。
	Content string `json:"content"`

	// ToolCalls 当 assistant 请求工具调用时非空。切片支持并行调用（Parallel Tool Calling）：
	// 引擎并发执行所有调用，并将每个结果作为独立的 Observation 消息回传。
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`

	// ToolCallID 用于 Observation（RoleTool 消息）中，标识此消息是对哪个 ToolCall
	// 的响应（Request-Response 关联），使 LLM 能够将 Observation 与其原始请求进行匹配。
	ToolCallID string `json:"tool_call_id,omitempty"`

	// IsError 仅在 Role == RoleTool 时有意义。为 true 时，Provider 适配器会把该信号
	// 透传给底层 API（如 Anthropic 的 tool_result.is_error），让 LLM 明确感知到
	// "这是失败结果"，触发自愈（Self-Healing）重试，而不是误把错误文本当成正常输出。
	IsError bool `json:"is_error,omitempty"`
}

// ToolCall 代表模型发出的单个工具调用请求。模型指定要调用的已注册工具名称，
// 并提供符合该工具 Input Schema 的 JSON 参数载荷。
type ToolCall struct {
	// ID 由 LLM Provider 分配的唯一标识符（Unique Identifier），用于将工具执行结果
	// （ToolResult）与原始请求（ToolCall）进行关联（Request-Response Matching）。
	ID string `json:"id"`

	// Name 目标工具在 Registry（注册表）中的标识符（如 "bash"、"read_file"、"glob"）。
	Name string `json:"name"`

	// Arguments 存放工具调用的原始 JSON 参数载荷（Payload）。使用 json.RawMessage
	// 延迟反序列化（Lazy Deserialization），将解析责任交给具体的工具实现，
	// 避免在引擎层进行过早的类型断言（Premature Type Assertion）。
	Arguments json.RawMessage `json:"arguments"`
}

// ToolResult 捕获单个工具在本地执行后的结果。它会被转换为 user 角色的
// Observation 消息并追加到对话上下文中供 LLM 消费。
type ToolResult struct {
	// ToolCallID 镜像原始 ToolCall 的 ID，建立 LLM 期望的请求-响应关联。
	ToolCallID string `json:"tool_call_id"`

	// Output 包含工具执行捕获的 stdout/stderr 输出，或在工具失败时的错误堆栈信息。
	Output string `json:"output"`

	// IsError 标记工具执行是否失败。当为 true 时，引擎可将错误暴露给 LLM，
	// 使其尝试自愈（Self-Healing），例如修正命令语法后重试。
	IsError bool `json:"is_error"`
}

// ToolDefinition 描述 agent 可调用工具的元信息。这些定义会被转发给 LLM Provider，
// 使模型了解有哪些可用工具、各自的用途以及接受的参数格式。
type ToolDefinition struct {
	// Name 工具在 Registry 中的唯一标识符。
	Name string `json:"name"`

	// Description 工具用途和行为的自然语言描述，供 LLM 决定何时以及如何调用该工具。
	Description string `json:"description"`

	// InputSchema 描述工具参数格式的 JSON Schema。使用 any 类型以兼容
	// 不同 SDK 的参数格式要求（OpenAI 需要 shared.FunctionParameters，
	// Anthropic 需要 map[string]any），各 Provider 实现负责类型转换（Type Adaptation）。
	InputSchema any `json:"input_schema"`
}
