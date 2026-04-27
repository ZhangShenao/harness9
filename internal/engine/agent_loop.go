// Package engine 实现了 harness9 的核心 agent loop — 驱动
// Two-Stage ReAct（Thinking → Action → Observation）循环的编排层。
//
// # Two-Stage ReAct 设计理念
//
// 传统 ReAct 循环在每个 Turn 中执行一次 LLM 调用，让模型同时完成推理和行动。
// 这在复杂任务中容易出现"未经深思的冲动行为"——模型在充分理解问题之前就急于调用工具。
//
// harness9 引入 Two-Stage ReAct，将每个 Turn 拆分为两个阶段：
//
//	Phase 1 — Thinking（慢思考）：剥夺所有工具，迫使模型在没有行动能力的情况下
//	           进行纯粹的推理、分析和规划。因为没有工具可用，模型必须充分理解
//	           问题、拆解任务、制定策略，而不仅仅是"试一试"。
//
//	Phase 2 — Action（行动）：恢复完整工具列表，模型基于 Phase 1 的思考结果
//	           采取有针对性的行动。此时模型已经"想清楚了"，工具调用更精准高效。
//
// # 上下文一致性保证
//
// 每个 Turn 最终只向 contextHistory 注入一条 assistant 消息。Thinking 的思考内容
// 与 Action 的行动内容会被合并为同一条消息，避免连续 assistant 消息导致的 API
// 兼容性问题（Anthropic Messages API 要求 user/assistant 严格交替）。
//
// # 引擎职责
//
//   - 维护跨 Turn 的对话上下文 (Context History)
//   - 在每个 Turn 中编排 Thinking → Action 两阶段 LLM 调用
//   - 通过 Registry 接口路由工具调用
//   - 将工具执行结果 (Observation) 回注上下文供下一轮使用
//   - 检测终止条件（模型不再发起 ToolCall）
package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/harness9/internal/provider"
	"github.com/harness9/internal/schema"
	"github.com/harness9/internal/tools"
)

// Option 是 AgentEngine 的函数选项，用于在构造时配置非必需参数。
type Option func(*AgentEngine)

// WithMaxTurns 设置单次 Run 允许的最大 Turn 数。n <= 0 表示不限制。
func WithMaxTurns(n int) Option {
	return func(e *AgentEngine) {
		e.MaxTurns = n
	}
}

// WithToolTimeout 设置单个工具执行的超时时间。0 表示使用 context 原始截止时间。
func WithToolTimeout(d time.Duration) Option {
	return func(e *AgentEngine) {
		e.ToolTimeout = d
	}
}

// AgentEngine 是 harness9 agent loop 的核心编排器。它将 LLM Provider（"大脑"）
// 与 Tool Registry（"双手"）组合在一起，执行多轮 Two-Stage ReAct 循环直到任务完成。
//
// 当 EnableThinking 为 true 时，每个 Turn 由两次 LLM 调用组成：
//
//	Thinking 调用（tools=nil）→ Action 调用（tools=availableTools）
//
// 两次调用的结果会合并为一条 assistant 消息注入上下文，保证 API 兼容性。
//
// 当 EnableThinking 为 false 时，退化为标准单阶段 ReAct：
//
//	Action 调用（tools=availableTools）
type AgentEngine struct {
	// provider LLM 后端，负责生成 assistant 响应（推理文本和/或工具调用请求）。
	provider provider.LLMProvider

	// registry 工具注册表，负责将 ToolCall 解析为具体执行并返回结果。
	registry tools.Registry

	// WorkDir agent 操作的工作区绝对路径，注入到 system prompt 中使 LLM 了解其工作上下文。
	WorkDir string

	// EnableThinking 控制是否启用两阶段 Thinking-Action 模式。
	// true:  每个 Turn 先进行无工具的深度思考（Phase 1），再恢复工具执行行动（Phase 2）
	// false: 标准 ReAct 模式，每个 Turn 只进行一次 LLM 调用
	EnableThinking bool

	// MaxTurns 单次 Run 允许的最大 Turn 数。0 表示不限制。
	// 防止模型陷入无限循环，消耗过多 token。
	MaxTurns int

	// ToolTimeout 单个工具执行的超时时间。0 表示使用传入 context 的原始截止时间。
	// 超时后工具执行会被取消，结果标记为 IsError。
	ToolTimeout time.Duration
}

// NewAgentEngine 使用给定的 Provider、Registry 和工作目录创建新的 AgentEngine。
// 通过 Option 函数可配置 MaxTurns、ToolTimeout 等可选参数。
//
// 参数:
//   - p:              LLM Provider 实现（如 OpenAI、Anthropic 的适配器）
//   - r:              Tool Registry 实现（管理工具的注册与执行）
//   - workDir:        工作区绝对路径，注入 system prompt
//   - enableThinking: 是否启用两阶段 Thinking-Action 模式
//   - opts:           可选配置（WithMaxTurns, WithToolTimeout 等）
func NewAgentEngine(p provider.LLMProvider, r tools.Registry, workDir string, enableThinking bool, opts ...Option) *AgentEngine {
	e := &AgentEngine{
		provider:       p,
		registry:       r,
		WorkDir:        workDir,
		EnableThinking: enableThinking,
		MaxTurns:       50,
		ToolTimeout:    60 * time.Second,
	}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// Run 执行单个用户 prompt 的主循环。流程如下：
//
//  1. 使用 system prompt（含 WorkDir）和用户初始消息初始化对话上下文
//  2. 进入 Two-Stage ReAct 循环：
//     a. [Phase 1] 若启用 Thinking，先以空工具列表调用 LLM，迫使模型深度思考
//     b. [Phase 2] 以完整工具列表调用 LLM，模型基于思考结果采取行动
//     c. 将 Thinking + Action 合并为单条 assistant 消息追加到 Context History
//     d. 若响应不含 ToolCall，任务完成 — 退出
//     e. 否则通过 Registry 并发执行每个请求的工具调用（带独立超时）
//     f. 将每个工具结果作为 Observation 消息追加到上下文
//     g. 重复步骤 2a
//
// 参数：
//   - ctx: 控制整个循环的取消和超时。若循环中途 context 被取消，
//     挂起的工具执行和下一次 LLM 调用将响应取消信号
//   - userPrompt: 来自人类操作者的自然语言任务描述
func (e *AgentEngine) Run(ctx context.Context, userPrompt string) error {
	log.Printf("[engine] 启动 | workdir=%s thinking=%v maxTurns=%d toolTimeout=%v",
		e.WorkDir, e.EnableThinking, e.MaxTurns, e.ToolTimeout)

	// 初始化对话上下文：注入 system prompt（含工作区路径）定义 agent 身份和能力，
	// 然后附上用户任务描述。
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
			return fmt.Errorf("已达最大 Turn 数 (%d)，循环终止", e.MaxTurns)
		}

		// 检查 context 是否已取消（支持超时和手动中断）
		select {
		case <-ctx.Done():
			return fmt.Errorf("context 已取消: %w", ctx.Err())
		default:
		}

		log.Printf("[engine] Turn %d | contextMessages=%d", turnCount, len(contextHistory))

		availableTools := e.registry.GetAvailableTools()

		var responseMsg *schema.Message
		var actionContent string

		if e.EnableThinking {
			var merged *schema.Message
			merged, actionContent = e.runTwoStageTurn(ctx, turnCount, contextHistory, availableTools)
			if merged == nil {
				return nil
			}
			responseMsg = merged
		} else {
			var err error
			responseMsg, err = e.runActionOnly(ctx, turnCount, contextHistory, availableTools)
			if err != nil {
				return err
			}
			actionContent = responseMsg.Content
		}

		contextHistory = append(contextHistory, *responseMsg)

		if actionContent != "" {
			fmt.Printf("[assistant] %s\n", actionContent)
		}

		// --- 终止条件检测 ---
		if len(responseMsg.ToolCalls) == 0 {
			log.Printf("[engine] Turn %d | 任务完成，模型未请求工具调用", turnCount)
			break
		}

		// --- ToolCall 阶段（并发执行，带独立超时） ---
		results := e.executeToolsConcurrently(ctx, turnCount, responseMsg.ToolCalls)

		// --- Observation 阶段 ---
		for i, toolCall := range responseMsg.ToolCalls {
			observationMsg := schema.Message{
				Role:       schema.RoleUser,
				Content:    results[i].Output,
				ToolCallID: toolCall.ID,
			}
			contextHistory = append(contextHistory, observationMsg)
		}

		log.Printf("[engine] Turn %d | Observation 注入完成 | contextMessages=%d",
			turnCount, len(contextHistory))
	}

	log.Printf("[engine] 循环结束 | 总Turns=%d | contextMessages=%d", turnCount, len(contextHistory))
	return nil
}

// runTwoStageTurn 执行一个完整的两阶段 Turn（Thinking → Action），
// 返回合并后的单条 assistant 消息和 Phase 2 的行动内容（用于显示）。
//
// 核心设计：Phase 1 的思考内容通过临时上下文传递给 Phase 2，
// 但最终只合并为一条 assistant 消息注入到主 contextHistory，
// 避免 API 兼容性问题（连续 assistant 消息）。
//
// 返回 (nil, "") 表示应直接退出 Run（已在内部处理错误）。
func (e *AgentEngine) runTwoStageTurn(ctx context.Context, turn int, contextHistory []schema.Message, availableTools []schema.ToolDefinition) (*schema.Message, string) {
	// ============================================================
	// Phase 1: Thinking（慢思考与规划）
	// ============================================================
	//
	// 通过传入 nil 剥夺所有工具。LLM 没有行动能力，被迫进行纯推理。
	// 思考结果不会直接注入主 contextHistory，而是通过临时上下文传递给 Phase 2。
	//
	log.Printf("[engine] Turn %d | Phase 1: Thinking (tools=none)", turn)

	thinkResp, err := e.provider.Generate(ctx, contextHistory, nil)
	if err != nil {
		log.Printf("[engine] Turn %d | Thinking 阶段生成失败: %v", turn, err)
		return nil, ""
	}

	// 安全清除：确保 Thinking 响应不含 ToolCalls（tools=nil 时理论上不会返回，
	// 但防御性编程可防止 LLM 不遵守指令时污染上下文）。
	thinkResp.ToolCalls = nil

	if thinkResp.Content != "" {
		log.Printf("[engine] Turn %d | Phase 1 完成 | 思考长度=%d chars", turn, len(thinkResp.Content))
		fmt.Printf("[thinking] %s\n", thinkResp.Content)
	}

	// ============================================================
	// Phase 2: Action（行动与工具调用）
	// ============================================================
	//
	// 构建 Phase 2 的临时上下文：在主 contextHistory 基础上追加 Phase 1 的思考。
	// 这个临时上下文仅在本次 Generate 调用中使用，不会持久化到主 contextHistory。
	//
	// 这样 Phase 2 的 LLM 能"看到"思考内容并据此行动，而主上下文中
	// 最终只保留一条合并后的 assistant 消息（思考 + 行动），
	// 保证 user/assistant 严格交替的 API 兼容性。
	//
	phase2History := make([]schema.Message, len(contextHistory), len(contextHistory)+1)
	copy(phase2History, contextHistory)
	phase2History = append(phase2History, *thinkResp)

	log.Printf("[engine] Turn %d | Phase 2: Action (tools=%d)", turn, len(availableTools))

	actionResp, err := e.provider.Generate(ctx, phase2History, availableTools)
	if err != nil {
		log.Printf("[engine] Turn %d | Action 阶段生成失败: %v", turn, err)
		return nil, ""
	}

	// 合并 Thinking + Action 为单条 assistant 消息。
	// 这是解决"连续 assistant 消息"问题的关键：Phase 1 的思考不会作为独立消息
	// 留在上下文中，而是与 Phase 2 的行动合并。
	// 在后续 Turn 中，LLM 仍然可以看到合并后的完整内容。
	mergedMsg := &schema.Message{
		Role:      schema.RoleAssistant,
		Content:   joinContent(thinkResp.Content, actionResp.Content),
		ToolCalls: actionResp.ToolCalls,
	}

	log.Printf("[engine] Turn %d | Two-Stage 合并完成 | thinking=%d chars action=%d chars toolCalls=%d",
		turn, len(thinkResp.Content), len(actionResp.Content), len(actionResp.ToolCalls))

	return mergedMsg, actionResp.Content
}

// runActionOnly 执行标准单阶段 ReAct（EnableThinking=false 时的降级路径）。
func (e *AgentEngine) runActionOnly(ctx context.Context, turn int, contextHistory []schema.Message, availableTools []schema.ToolDefinition) (*schema.Message, error) {
	log.Printf("[engine] Turn %d | Action (tools=%d)", turn, len(availableTools))

	responseMsg, err := e.provider.Generate(ctx, contextHistory, availableTools)
	if err != nil {
		return nil, fmt.Errorf("模型生成失败 (turn %d): %w", turn, err)
	}
	return responseMsg, nil
}

// executeToolsConcurrently 并发执行所有工具调用，每个工具带有独立的超时控制。
// 通过预分配切片 + 索引写入确保结果顺序与 ToolCalls 一致。
func (e *AgentEngine) executeToolsConcurrently(ctx context.Context, turn int, toolCalls []schema.ToolCall) []schema.ToolResult {
	log.Printf("[engine] Turn %d | 执行 %d 个工具调用", turn, len(toolCalls))

	results := make([]schema.ToolResult, len(toolCalls))
	var wg sync.WaitGroup

	for i, toolCall := range toolCalls {
		wg.Add(1)
		go func(idx int, tc schema.ToolCall, currentTurn int) {
			defer wg.Done()

			// 为每个工具创建带独立超时的子 context。
			// 超时不影响其他工具执行，仅将当前工具标记为失败。
			toolCtx := ctx
			var cancel context.CancelFunc
			if e.ToolTimeout > 0 {
				toolCtx, cancel = context.WithTimeout(ctx, e.ToolTimeout)
				defer cancel()
			}

			log.Printf("[engine] Turn %d | 工具启动 | name=%s id=%s arguments=%s",
				currentTurn, tc.Name, tc.ID, toJSON(tc.Arguments))

			results[idx] = e.registry.Execute(toolCtx, tc)

			if results[idx].IsError {
				log.Printf("[engine] Turn %d | 工具失败 | name=%s id=%s result=%s",
					currentTurn, tc.Name, tc.ID, toJSON(map[string]interface{}{
						"is_error": true,
						"output":   truncStr(results[idx].Output),
					}))
			} else {
				log.Printf("[engine] Turn %d | 工具完成 | name=%s id=%s result=%s",
					currentTurn, tc.Name, tc.ID, toJSON(map[string]interface{}{
						"is_error": false,
						"output":   truncStr(results[idx].Output),
					}))
			}
		}(i, toolCall, turn)
	}

	wg.Wait()
	return results
}

// joinContent 将 Phase 1 的思考内容与 Phase 2 的行动内容合并为单段文本。
// 避免在上下文中出现连续的 assistant 消息。
func joinContent(thinking, action string) string {
	switch {
	case thinking == "" && action == "":
		return ""
	case thinking == "":
		return action
	case action == "":
		return thinking
	default:
		return thinking + "\n\n" + action
	}
}

// toJSON 将任意值序列化为 JSON 字符串，用于日志输出。
// 序列化失败时返回错误提示文本，不会 panic。
func toJSON(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("<marshal error: %v>", err)
	}
	return string(b)
}

// maxLogOutputLen 日志中单条输出的最大字节数，防止工具返回的超大内容撑爆日志。
const maxLogOutputLen = 512

// truncStr 截断过长的字符串，超出部分替换为截断提示。
// 用于在日志中安全输出工具执行结果，同时保留长度信息供调试参考。
func truncStr(s string) string {
	if len(s) <= maxLogOutputLen {
		return s
	}
	return s[:maxLogOutputLen] + fmt.Sprintf("...[截断，全文 %d 字节]", len(s))
}
