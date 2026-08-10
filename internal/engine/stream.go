// Package engine — 流式输出支持。
//
// RunStream 是 Run 的流式对应方法，复用 runLoop 共享内核，通过 emitter 注入输出侧差异：
// LLM 文本增量和工具进度通过 Go channel 以语义化 Event 推送给消费者。
//
// 数据流：Provider.GenerateStream → chan StreamChunk → streamGenerate → chan Event → 客户端
package engine

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/harness9/internal/hooks"
	"github.com/harness9/internal/logfmt"
	"github.com/harness9/internal/memory"
	"github.com/harness9/internal/schema"
)

// EventType 枚举了引擎面向客户端的流式事件类型。
type EventType string

const (
	// EventActionDelta 表示 Action 阶段的文本增量（逐 token）。Data 类型为 string。
	EventActionDelta EventType = "action_delta"

	// EventThinkingDelta 表示推理阶段的文本增量（逐 token）。Data 类型为 string。
	EventThinkingDelta EventType = "thinking_delta"

	// EventToolStart 表示引擎开始执行一个工具调用。Data 类型为 schema.ToolCall。
	EventToolStart EventType = "tool_start"

	// EventToolResult 表示一个工具执行完成。Data 类型为 ToolResultData。
	EventToolResult EventType = "tool_result"

	// EventDone 表示 agent loop 正常结束。
	EventDone EventType = "done"

	// EventError 表示 agent loop 中发生了错误。Data 类型为 string（错误描述）。
	EventError EventType = "error"

	// EventTokenUpdate 在每次 LLM 调用前发出，报告当前轮次的上下文 token 估算值。
	// Data 类型为 TokenUpdateData。
	EventTokenUpdate EventType = "token_update"

	// EventCompaction 在上下文发生有效压缩时发出。
	// Data 类型为 memory.CompactionRecord（向后兼容：CompactionData 类型保留但不再作为事件载荷）。
	EventCompaction EventType = "compaction"

	// EventApprovalRequired 表示工具执行需要人类审批。Data 类型为 ApprovalRequest。
	// 引擎在工具执行前发出此事件，同时工具 goroutine 阻塞在 ApprovalRequest.ResponseCh 等待回复。
	//
	// 并发工具调用时，多个工具 goroutine 可能同时请求审批。由于 Event channel 是无缓冲的，
	// 第二个审批请求会阻塞在 ch <- Event 直到 TUI 处理完第一个请求并恢复读取。
	// TUI 实现必须在展示审批对话框期间继续消费 Event channel（不可直接 select 等待 ResponseCh），
	// 否则多工具场景下会发生死锁。
	EventApprovalRequired EventType = "approval_required"

	// EventSubAgent 表示一次子代理进度更新。Data 类型为 schema.SubAgentUpdate。
	// 由 task 工具执行期间，Runner 消费子引擎事件流时经 ctx 注入的进度回调透传。
	EventSubAgent EventType = "sub_agent"
)

// Event 是引擎面向客户端的流式事件单元。RunStream 返回 <-chan Event，
// 客户端从 channel 中读取事件实现实时交互。
//
// 典型消费方式：
//
//	for evt := range stream {
//	    switch evt.Type {
//	    case engine.EventActionDelta:
//	        fmt.Print(evt.Data.(string))
//	    case engine.EventDone:
//	        return
//	    case engine.EventError:
//	        log.Fatal(evt.Data.(string))
//	    }
//	}
type Event struct {
	Type EventType `json:"type"`
	Turn int       `json:"turn,omitempty"`
	// Data 事件载荷，类型随 Type 变化：
	//   EventActionDelta  → string, EventThinkingDelta → string,
	//   EventToolStart    → schema.ToolCall,
	//   EventToolResult   → ToolResultData, EventDone → nil, EventError → string,
	//   EventTokenUpdate  → TokenUpdateData, EventCompaction → memory.CompactionRecord,
	//   EventSubAgent     → schema.SubAgentUpdate
	Data any `json:"data,omitempty"`
}

// TokenUpdateData 是 EventTokenUpdate 事件的载荷。
type TokenUpdateData struct {
	// EstimatedTokens 当前上下文的估算 token 数（消息 + 工具定义）。
	EstimatedTokens int `json:"estimated_tokens"`
	// ContextWindow 当前模型的最大 context window（tokens）。0 表示未知。
	ContextWindow int `json:"context_window"`
}

// ToolResultData 是 EventToolResult 事件的载荷，携带工具执行结果和引擎侧精确耗时。
type ToolResultData struct {
	// Result 是工具执行的结果。
	Result schema.ToolResult
	// Duration 是工具在引擎侧的精确执行耗时（从工具函数入口到返回的真实时长，不含 channel 传输延迟）。
	Duration time.Duration
}

// CompactionData 是历史遗留的压缩事件载荷类型，保留用于向后兼容。
// EventCompaction 事件现在携带 memory.CompactionRecord（含 tier、锚点、外存条目等完整审计信息）。
// 新代码应使用 memory.CompactionRecord。
type CompactionData struct {
	// TokensBefore 压缩前的估算 token 数。
	TokensBefore int `json:"tokens_before"`
	// TokensAfter 压缩后的估算 token 数。
	TokensAfter int `json:"tokens_after"`
	// MsgsBefore 压缩前的消息条数。
	MsgsBefore int `json:"msgs_before"`
	// MsgsAfter 压缩后的消息条数。
	MsgsAfter int `json:"msgs_after"`
}

// ApprovalRequest 是 EventApprovalRequired 的事件载荷。
// 引擎 goroutine 通过 ResponseCh 阻塞等待 TUI（或其他消费者）的审批决策。
type ApprovalRequest struct {
	ToolCall   schema.ToolCall
	Reason     string
	RiskLevel  string
	ResponseCh chan hooks.ApprovalResponse
}

// sendEvent 向 Event channel 发送事件，同时感知 context 取消。
// 返回 false 表示 context 已取消，调用方应立即退出。
// 终止事件（EventDone / EventError）应使用直接 ch <- 而非本函数，以确保消费者收到。
func sendEvent(ctx context.Context, ch chan<- Event, evt Event) bool {
	select {
	case <-ctx.Done():
		return false
	case ch <- evt:
		return true
	}
}

// RunStream 是 Run 的流式对应方法，通过 Go channel 逐事件输出 agent loop 的运行状态。
// 内部启动独立 goroutine 运行共享 runLoop，channel 在循环结束后自动关闭。
func (e *AgentEngine) RunStream(ctx context.Context, userPrompt string) (<-chan Event, error) {
	ch := make(chan Event)

	go func() {
		defer close(ch)

		em := emitter{
			generate: func(ctx context.Context, turn int, history []schema.Message, tools []schema.ToolDefinition) (*schema.Message, *schema.Usage, error) {
				return e.streamGenerate(ctx, ch, turn, history, tools)
			},
			toolStart: func(turn int, tc schema.ToolCall) {
				log.Print(logfmt.FormatToolStart("engine-stream", turn, tc))
				sendEvent(ctx, ch, Event{Type: EventToolStart, Turn: turn, Data: tc})
			},
			toolDone: func(turn int, tc schema.ToolCall, result schema.ToolResult, d time.Duration) {
				log.Print(logfmt.FormatToolDone("engine-stream", turn, tc, result, d))
				sendEvent(ctx, ch, Event{Type: EventToolResult, Turn: turn, Data: ToolResultData{Result: result, Duration: d}})
			},
			tokenUpdate: func(tokens, window int) {
				sendEvent(ctx, ch, Event{Type: EventTokenUpdate, Data: TokenUpdateData{
					EstimatedTokens: tokens,
					ContextWindow:   window,
				}})
			},
			compaction: func(record memory.CompactionRecord) {
				sendEvent(ctx, ch, Event{Type: EventCompaction, Data: record})
			},
			// 审批等待使用会话级 ctx（RunStream 的外层 ctx），不受工具执行超时约束。
			// 工具超时（toolTimeout）仅应限制工具本身的计算时间，而非人类决策时间：
			// 若用 toolCtx（含 60s 超时）等待 respCh，用户超时未响应时工具会被自动拒绝，
			// 且 TUI 端 ResponseCh 不再被读取，导致工具 goroutine 在发送时短暂挂起。
			// 参数 _ context.Context 为 toolCtx，此处故意忽略，使用外层 ctx。
			approval: func(_ context.Context, tc schema.ToolCall, reason, riskLevel string) hooks.ApprovalResponse {
				if e.permissionMode == PermissionModeBypassAll {
					return hooks.ApprovalResponse{Approved: true}
				}
				respCh := make(chan hooks.ApprovalResponse, 1)
				req := ApprovalRequest{
					ToolCall:   tc,
					Reason:     reason,
					RiskLevel:  riskLevel,
					ResponseCh: respCh,
				}
				select {
				case <-ctx.Done():
					return hooks.ApprovalResponse{Approved: false}
				case ch <- Event{Type: EventApprovalRequired, Data: req}:
				}
				select {
				case <-ctx.Done():
					return hooks.ApprovalResponse{Approved: false}
				case resp := <-respCh:
					return resp
				}
			},
		}

		// 注入子代理进度 sink：task 工具执行期间，Runner 经此回调把子代理事件透传给 TUI。
		progressCtx := hooks.WithSubAgentProgress(ctx, func(u schema.SubAgentUpdate) {
			sendEvent(ctx, ch, Event{Type: EventSubAgent, Data: u})
		})
		if err := e.runLoop(progressCtx, userPrompt, "engine-stream", em); err != nil {
			ch <- Event{Type: EventError, Data: err.Error()}
			return
		}
		ch <- Event{Type: EventDone}
	}()

	return ch, nil
}

// streamGenerate 驱动 Provider.GenerateStream，将 text_delta 转发为 EventActionDelta，
// 最终返回 StreamChunkDone 中的完整 Message 和实际 token 用量供 runLoop 消费。
func (e *AgentEngine) streamGenerate(ctx context.Context, ch chan<- Event, turn int, history []schema.Message, tools []schema.ToolDefinition) (*schema.Message, *schema.Usage, error) {
	stream, err := e.provider.GenerateStream(ctx, history, tools)
	if err != nil {
		return nil, nil, err
	}

	var msg *schema.Message
	var usage *schema.Usage
	for chunk := range stream {
		switch chunk.Type {
		case schema.StreamChunkTextDelta:
			if !sendEvent(ctx, ch, Event{Type: EventActionDelta, Turn: turn, Data: chunk.Delta}) {
				return nil, nil, ctx.Err()
			}
		case schema.StreamChunkThinkingDelta:
			if !sendEvent(ctx, ch, Event{Type: EventThinkingDelta, Turn: turn, Data: chunk.Delta}) {
				return nil, nil, ctx.Err()
			}
		case schema.StreamChunkDone:
			msg = chunk.Message
			usage = chunk.Usage
		case schema.StreamChunkError:
			return nil, nil, fmt.Errorf("%s", chunk.Error)
		}
	}

	if msg == nil {
		return nil, nil, fmt.Errorf("provider stream ended without done chunk")
	}
	return msg, usage, nil
}
