// Package engine 实现了 harness9 的核心 agent loop — 驱动
// Two-Stage ReAct（Thinking → Action → Observation）循环的编排层。
//
// 本文件（stream.go）提供流式输出能力，是 agent_loop.go 中阻塞式 Run 方法的
// 流式对应。RunStream 通过 Go channel 逐事件输出 agent loop 的运行状态，
// 使客户端能够实时接收 LLM 逐 token 输出、工具执行进度等信息。
//
// # 流式架构
//
// 数据流经两层 channel 转换：
//
//	Provider.GenerateStream() → chan StreamChunk → streamPhase() → chan Event → 客户端
//
// Provider 层产出底层的 token 级增量（StreamChunk），引擎层将其转化为面向客户端的
// 语义事件（Event），客户端只需关心业务含义，无需处理 SDK 差异。
//
// # 与阻塞模式的关系
//
// RunStream 与 Run 共享相同的 Two-Stage ReAct 循环逻辑和配置（MaxTurns、ToolTimeout
// 等），但有以下关键区别：
//   - Run 使用 provider.Generate（阻塞式），RunStream 使用 provider.GenerateStream（流式）
//   - Run 通过 fmt.Printf 直接输出文本到 stdout，RunStream 通过 Event channel 输出
//   - Run 在循环结束后返回 error，RunStream 通过 EventError 事件报告错误
//   - 两种模式使用不同的日志前缀：[engine] vs [engine-stream]
package engine

import (
	"context"
	"fmt"
	"log"
	"sync"

	"github.com/harness9/internal/schema"
)

// EventType 枚举了引擎面向客户端的流式事件类型。
// 与 Provider 层的 StreamChunkType 不同，Event 是经过引擎语义化处理的事件：
// 引擎知道当前处于 Thinking 阶段还是 Action 阶段，将 text_delta 转化为
// EventThinkingDelta 或 EventActionDelta；引擎在工具执行前后发送
// EventToolStart 和 EventToolResult。
type EventType string

const (
	// EventThinkingDelta 表示 Thinking 阶段的文本增量。
	// 仅在 EnableThinking == true 时产生。Data 类型为 string（token 文本）。
	EventThinkingDelta EventType = "thinking_delta"

	// EventActionDelta 表示 Action 阶段的文本增量。
	// 无论是否启用 Thinking 模式，Action 阶段的文本输出都会触发此事件。
	// Data 类型为 string（token 文本）。
	EventActionDelta EventType = "action_delta"

	// EventToolStart 表示引擎开始执行一个工具调用。
	// 在引擎通过 Registry 分发工具执行时触发（而非 LLM 流式输出工具调用请求时）。
	// Data 类型为 schema.ToolCall（含 Name、ID、Arguments）。
	EventToolStart EventType = "tool_start"

	// EventToolResult 表示一个工具执行完成。
	// 每个并发执行的工具完成后都会独立触发此事件，顺序不固定。
	// Data 类型为 schema.ToolResult（含 Output、IsError）。
	EventToolResult EventType = "tool_result"

	// EventDone 表示 agent loop 正常结束。
	// 当模型不再请求工具调用（自然终止）时触发。Data 为 nil。
	EventDone EventType = "done"

	// EventError 表示 agent loop 中发生了错误。
	// 可能的原因：MaxTurns 超限、context 取消、Provider 流式错误等。
	// Data 类型为 string（错误描述）。
	EventError EventType = "error"
)

// Event 是引擎面向客户端的流式事件单元。RunStream 方法返回 <-chan Event，
// 客户端从 channel 中读取事件来实现实时交互。
//
// 典型的消费方式：
//
//	for evt := range stream {
//	    switch evt.Type {
//	    case engine.EventActionDelta:
//	        fmt.Print(evt.Data.(string))  // 逐 token 输出
//	    case engine.EventToolResult:
//	        result := evt.Data.(schema.ToolResult)
//	        fmt.Println(result.Output)
//	    case engine.EventDone:
//	        // 循环结束
//	    case engine.EventError:
//	        log.Fatal(evt.Data.(string))
//	    }
//	}
type Event struct {
	// Type 事件类型，决定 Data 字段的实际类型。
	// 参见 EventType 常量的文档说明。
	Type EventType `json:"type"`

	// Turn 当前事件所属的 Turn 编号。Turn 从 1 开始递增。
	// 客户端可通过此字段判断当前是第几轮循环。
	Turn int `json:"turn,omitempty"`

	// Data 事件载荷，类型随 Type 变化：
	//   - EventThinkingDelta / EventActionDelta → string（token 文本）
	//   - EventToolStart   → schema.ToolCall（含 Name、ID、Arguments）
	//   - EventToolResult  → schema.ToolResult（含 Output、IsError）
	//   - EventDone        → nil
	//   - EventError       → string（错误描述）
	Data any `json:"data,omitempty"`
}

// sendEvent 向 Event channel 发送事件，同时感知 context 取消。
// 使用 select 监听 ctx.Done()，确保在 context 被取消时不会阻塞在 channel 发送上。
// 返回 false 表示 context 已取消，调用方应立即退出 goroutine。
func sendEvent(ctx context.Context, ch chan<- Event, evt Event) bool {
	select {
	case <-ctx.Done():
		return false
	case ch <- evt:
		return true
	}
}

// RunStream 是 Run 的流式对应方法，通过 Go channel 逐事件输出 agent loop 的运行状态。
//
// 与 Run 的核心区别：
//   - Run 调用 provider.Generate（阻塞等待完整响应），RunStream 调用 provider.GenerateStream（逐 token 增量）
//   - Run 通过 fmt.Printf 直接输出文本，RunStream 通过 Event channel 输出，由消费者决定展示方式
//   - Run 返回 error，RunStream 通过 EventError 事件报告错误
//
// 内部启动一个独立的 goroutine 运行主循环，返回只读 channel 供消费者读取。
// channel 在循环结束（正常或异常）后自动关闭。
//
// 参数和配置与 Run 完全一致，两种模式共享同一个 AgentEngine 实例。
func (e *AgentEngine) RunStream(ctx context.Context, userPrompt string) (<-chan Event, error) {
	ch := make(chan Event)

	go func() {
		defer close(ch)

		log.Printf("[engine-stream] 启动 | workdir=%s thinking=%v maxTurns=%d toolTimeout=%v",
			e.WorkDir, e.EnableThinking, e.MaxTurns, e.ToolTimeout)

		// 初始化对话上下文，与 Run 保持一致：
		// 注入 system prompt（含工作区路径）定义 agent 身份，附上用户任务描述。
		contextHistory := []schema.Message{
			{
				Role: schema.RoleSystem,
				Content: fmt.Sprintf(
					"You are harness9, an expert coding assistant. "+
						"You have full access to tools in the workspace. "+
						"Your working directory is: %s",
					e.WorkDir,
				),
			},
			{
				Role:    schema.RoleUser,
				Content: userPrompt,
			},
		}

		turnCount := 0

		for {
			turnCount++

			// --- 安全阀：防止无限循环 ---
			if e.MaxTurns > 0 && turnCount > e.MaxTurns {
				sendEvent(ctx, ch, Event{Type: EventError, Data: fmt.Sprintf("已达最大 Turn 数 (%d)", e.MaxTurns)})
				return
			}

			// 检查 context 是否已取消（支持超时和手动中断）
			select {
			case <-ctx.Done():
				sendEvent(ctx, ch, Event{Type: EventError, Data: ctx.Err().Error()})
				return
			default:
			}

			log.Printf("[engine-stream] Turn %d | contextMessages=%d", turnCount, len(contextHistory))

			availableTools := e.registry.GetAvailableTools()

			var responseMsg *schema.Message

			if e.EnableThinking {
				// Phase 1: Thinking — 使用 EventThinkingDelta 转发 token 增量
				thinkMsg := e.streamPhase(ctx, ch, turnCount, EventThinkingDelta, contextHistory, nil)
				if thinkMsg == nil {
					return
				}
				log.Printf("[engine-stream] Turn %d | Phase 1 完成 | 思考长度=%d chars", turnCount, len(thinkMsg.Content))

				// 构建临时上下文，将 Phase 1 思考注入 Phase 2 调用。
				// 与 Run 的逻辑一致：思考仅在 Phase 2 调用期间存在，不持久化到主 contextHistory。
				phase2History := make([]schema.Message, len(contextHistory), len(contextHistory)+1)
				copy(phase2History, contextHistory)
				phase2History = append(phase2History, *thinkMsg)

				// Phase 2: Action — 使用 EventActionDelta 转发 token 增量
				responseMsg = e.streamPhase(ctx, ch, turnCount, EventActionDelta, phase2History, availableTools)
				if responseMsg == nil {
					return
				}

				// 合并 Thinking + Action 为单条 assistant 消息（避免连续 assistant 消息）
				merged := &schema.Message{
					Role:      schema.RoleAssistant,
					Content:   joinContent(thinkMsg.Content, responseMsg.Content),
					ToolCalls: responseMsg.ToolCalls,
				}
				responseMsg = merged

				log.Printf("[engine-stream] Turn %d | Two-Stage 合并完成 | thinking=%d chars action=%d chars toolCalls=%d",
					turnCount, len(thinkMsg.Content), len(responseMsg.Content), len(responseMsg.ToolCalls))
			} else {
				// 标准 ReAct 模式：单阶段，直接使用 EventActionDelta
				log.Printf("[engine-stream] Turn %d | Action (tools=%d)", turnCount, len(availableTools))
				responseMsg = e.streamPhase(ctx, ch, turnCount, EventActionDelta, contextHistory, availableTools)
				if responseMsg == nil {
					return
				}
			}

			contextHistory = append(contextHistory, *responseMsg)

			// --- 终止条件检测：模型不再请求工具调用 ---
			if len(responseMsg.ToolCalls) == 0 {
				log.Printf("[engine-stream] Turn %d | 任务完成，模型未请求工具调用", turnCount)
				sendEvent(ctx, ch, Event{Type: EventDone})
				return
			}

			// --- ToolCall 阶段（并发执行，带独立超时，同时发送事件） ---
			results := e.executeToolsStreaming(ctx, ch, turnCount, responseMsg.ToolCalls)

			// --- Observation 阶段：将工具结果追加到上下文 ---
			// 与 Run 保持一致：使用 RoleTool 并透传 IsError，让 Provider 适配器可以把
			// 失败信号原样映射到底层 API（如 Anthropic 的 tool_result.is_error）。
			for i, toolCall := range responseMsg.ToolCalls {
				observationMsg := schema.Message{
					Role:       schema.RoleTool,
					Content:    results[i].Output,
					ToolCallID: toolCall.ID,
					IsError:    results[i].IsError,
				}
				contextHistory = append(contextHistory, observationMsg)
			}

			log.Printf("[engine-stream] Turn %d | Observation 注入完成 | contextMessages=%d",
				turnCount, len(contextHistory))
		}
	}()

	return ch, nil
}

// streamPhase 是 RunStream 中替代直接调用 provider.Generate 的核心方法。
// 它调用 provider.GenerateStream 获取流式 channel，将底层 StreamChunk 逐个读取
// 并转换为面向客户端的语义 Event。
//
// 工作流程：
//  1. 调用 provider.GenerateStream 获取 <-chan StreamChunk
//  2. 从 channel 逐个读取 chunk：
//     - text_delta → 转发为 deltaType（EventThinkingDelta 或 EventActionDelta）事件
//     - tool_call_start → 忽略（工具执行事件在 executeToolsStreaming 中发送）
//     - done → 提取完整的 Message（含累积的 Content 和 ToolCalls）
//     - error → 发送 EventError 事件并返回 nil
//  3. 返回累积的完整 Message，供 RunStream 注入到对话上下文
//
// deltaType 参数决定文本增量事件的类型：
//   - Thinking 阶段传入 EventThinkingDelta
//   - Action 阶段传入 EventActionDelta
//
// 返回 nil 表示应终止 RunStream（错误或 context 取消）。
func (e *AgentEngine) streamPhase(ctx context.Context, ch chan<- Event, turn int, deltaType EventType, history []schema.Message, tools []schema.ToolDefinition) *schema.Message {
	stream, err := e.provider.GenerateStream(ctx, history, tools)
	if err != nil {
		log.Printf("[engine-stream] Turn %d | GenerateStream 失败: %v", turn, err)
		sendEvent(ctx, ch, Event{Type: EventError, Turn: turn, Data: err.Error()})
		return nil
	}

	// 从 Provider 的 StreamChunk channel 中读取并转发。
	// Provider 在流结束时自动关闭 channel 并在最后一个 StreamChunkDone 中携带完整 Message。
	var msg *schema.Message
	for chunk := range stream {
		switch chunk.Type {
		case schema.StreamChunkTextDelta:
			if !sendEvent(ctx, ch, Event{Type: deltaType, Turn: turn, Data: chunk.Delta}) {
				return nil
			}
		case schema.StreamChunkToolCallStart:
			// 工具调用请求已到达，但实际执行在 executeToolsStreaming 中进行。
			// EventToolStart 在那里发送，保证语义正确。
		case schema.StreamChunkDone:
			msg = chunk.Message
		case schema.StreamChunkError:
			log.Printf("[engine-stream] Turn %d | Provider 流式错误: %s", turn, chunk.Error)
			sendEvent(ctx, ch, Event{Type: EventError, Turn: turn, Data: chunk.Error})
			return nil
		}
	}

	// 防御性检查：Provider 必须在流结束前发送 StreamChunkDone。
	if msg == nil {
		sendEvent(ctx, ch, Event{Type: EventError, Turn: turn, Data: "provider stream ended without done chunk"})
		return nil
	}

	return msg
}

// executeToolsStreaming 并发执行所有工具调用，与阻塞模式的 executeToolsConcurrently 逻辑一致，
// 但在工具启动和完成时通过 Event channel 发送事件，使客户端能够实时感知工具执行进度。
//
// 事件发送时序：
//
//	EventToolStart(name=read_file, id=call_1)   ← 工具 1 开始
//	EventToolStart(name=bash, id=call_2)        ← 工具 2 开始
//	EventToolResult(output=..., id=call_1)       ← 工具 1 完成
//	EventToolResult(output=..., id=call_2)       ← 工具 2 完成
//
// 由于工具并发执行，EventToolResult 的顺序不固定（先完成的先发送）。
func (e *AgentEngine) executeToolsStreaming(ctx context.Context, ch chan<- Event, turn int, toolCalls []schema.ToolCall) []schema.ToolResult {
	log.Printf("[engine-stream] Turn %d | 执行 %d 个工具调用", turn, len(toolCalls))

	results := make([]schema.ToolResult, len(toolCalls))
	var wg sync.WaitGroup

	for i, toolCall := range toolCalls {
		wg.Add(1)
		go func(idx int, tc schema.ToolCall) {
			defer wg.Done()

			// 为每个工具创建带独立超时的子 context。
			// 超时不影响其他工具执行，仅将当前工具标记为失败。
			toolCtx := ctx
			var cancel context.CancelFunc
			if e.ToolTimeout > 0 {
				toolCtx, cancel = context.WithTimeout(ctx, e.ToolTimeout)
				defer cancel()
			}

			log.Printf("[engine-stream] Turn %d | 工具启动 | name=%s id=%s", turn, tc.Name, tc.ID)

			sendEvent(ctx, ch, Event{Type: EventToolStart, Turn: turn, Data: tc})

			results[idx] = e.registry.Execute(toolCtx, tc)

			if results[idx].IsError {
				log.Printf("[engine-stream] Turn %d | 工具失败 | name=%s id=%s", turn, tc.Name, tc.ID)
			} else {
				log.Printf("[engine-stream] Turn %d | 工具完成 | name=%s id=%s", turn, tc.Name, tc.ID)
			}

			sendEvent(ctx, ch, Event{Type: EventToolResult, Turn: turn, Data: results[idx]})
		}(i, toolCall)
	}

	wg.Wait()
	return results
}
