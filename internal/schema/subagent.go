// Package schema — 子代理进度更新类型。
package schema

// SubAgentUpdateKind 标识一次子代理进度更新的语义类型。
type SubAgentUpdateKind string

const (
	// SubAgentStart 子代理开始执行。
	SubAgentStart SubAgentUpdateKind = "start"
	// SubAgentDelta 子代理正文文本增量。
	SubAgentDelta SubAgentUpdateKind = "delta"
	// SubAgentThinking 子代理推理文本增量。
	SubAgentThinking SubAgentUpdateKind = "thinking"
	// SubAgentToolStart 子代理开始执行一个工具。
	SubAgentToolStart SubAgentUpdateKind = "tool_start"
	// SubAgentToolResult 子代理一个工具执行完成。
	SubAgentToolResult SubAgentUpdateKind = "tool_result"
	// SubAgentDone 子代理正常结束。
	SubAgentDone SubAgentUpdateKind = "done"
	// SubAgentError 子代理执行出错。
	SubAgentError SubAgentUpdateKind = "error"
	// SubAgentPaused 子代理被暂停（Turn 边界门控生效）。
	SubAgentPaused SubAgentUpdateKind = "paused"
	// SubAgentResumed 子代理被恢复运行。
	SubAgentResumed SubAgentUpdateKind = "resumed"
	// SubAgentCancelled 子代理被取消（Text 携带原因）。
	SubAgentCancelled SubAgentUpdateKind = "cancelled"
)

// SubAgentUpdate 是子代理运行过程向上层（TUI）转发的一次进度更新。
// 由 Runner 在消费子引擎事件流时生成，经 hooks.SubAgentProgressFunc 透传。
type SubAgentUpdate struct {
	// AgentName 子代理类型名称（如 "general-purpose"）。
	AgentName string `json:"agent_name"`
	// Kind 更新的语义类型。
	Kind SubAgentUpdateKind `json:"kind"`
	// Text 文本载荷（delta/thinking 为增量；error/done 为完整信息）。
	Text string `json:"text,omitempty"`
	// ToolName 工具名称（tool_start/tool_result 时填写）。
	ToolName string `json:"tool_name,omitempty"`
	// IsError 工具结果是否为错误（tool_result 时有意义）。
	IsError bool `json:"is_error,omitempty"`
}
