// 终止原因与事件载荷类型 —— 受控熔断的原因码定义。
//
// TerminationReason 标识受控熔断的裁决原因。设计决策（见 spec §2）：
// Run 的返回语义保持 error 不变，但四种受控熔断通过 EventTerminated
// 携带原因码供流式消费者区分"受控熔断"与"真故障"（后者仍发 EventError）。
package engine

// TerminationReason 标识受控熔断的裁决原因。
type TerminationReason string

const (
	// ReasonNatural 模型不再发起工具调用，自然停止。
	ReasonNatural TerminationReason = "natural"
	// ReasonMaxTurns 达到最大 Turn 数。
	ReasonMaxTurns TerminationReason = "max_turns"
	// ReasonRunTimeout 达到墙钟超时上限。
	ReasonRunTimeout TerminationReason = "run_timeout"
	// ReasonTokenBudget 达到累计 input token 预算。
	ReasonTokenBudget TerminationReason = "token_budget"
	// ReasonRepetitionLoop 重复调用死循环且提醒已被证明无效，升级为硬终止。
	ReasonRepetitionLoop TerminationReason = "repetition_loop"
)

// TerminationData 是 EventTerminated 的事件载荷。
type TerminationData struct {
	// Reason 受控熔断的原因码。
	Reason TerminationReason `json:"reason"`
	// Message 人类可读的终止描述。
	Message string `json:"message"`
}

// StateChangeData 是 EventStateChange 的事件载荷。
type StateChangeData struct {
	// From 流转前的状态。
	From LoopState `json:"from"`
	// To 流转后的状态。
	To LoopState `json:"to"`
	// Turn 流转发生时的 Turn 编号。
	Turn int `json:"turn"`
}
