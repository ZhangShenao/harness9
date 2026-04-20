// Package engine 实现了 harness9 的核心 agent loop — 驱动
// Reasoning → ToolCall → Observation 循环的编排层。
//
// 引擎职责：
//   - 维护跨 Turn 的对话上下文 (Context History)
//   - 通过 Provider 接口分发 LLM 推理请求
//   - 通过 Registry 接口路由工具调用
//   - 将工具执行结果 (Observation) 回注上下文供下一轮使用
//   - 检测终止条件（模型不再发起 ToolCall）
package engine

import (
	"context"
	"fmt"
	"log"
	"sync"

	"github.com/harness9/internal/provider"
	"github.com/harness9/internal/schema"
	"github.com/harness9/internal/tools"
)

// AgentEngine 是 harness9 agent loop 的核心编排器。它将 LLM Provider（"大脑"）
// 与 Tool Registry（"双手"）组合在一起，执行多轮 Reasoning-Action 循环直到任务完成。
type AgentEngine struct {
	// provider LLM 后端，负责生成 assistant 响应（推理文本和/或工具调用请求）。
	provider provider.LLMProvider

	// registry 工具注册表，负责将 ToolCall 解析为具体执行并返回结果。
	registry tools.Registry

	// WorkDir agent 操作的工作区绝对路径，注入到 system prompt 中使 LLM 了解其工作上下文。
	WorkDir string
}

// NewAgentEngine 使用给定的 Provider、Registry 和工作目录创建新的 AgentEngine。
func NewAgentEngine(p provider.LLMProvider, r tools.Registry, workDir string) *AgentEngine {
	return &AgentEngine{
		provider: p,
		registry: r,
		WorkDir:  workDir,
	}
}

// Run 执行单个用户 prompt 的主循环。流程如下：
//
//  1. 使用 system prompt 和用户初始消息初始化对话上下文
//  2. 进入 Reasoning → ToolCall → Observation 循环：
//     a. 将完整上下文和可用工具发送给 LLM Provider
//     b. 将 assistant 响应追加到 Context History
//     c. 若响应不含 ToolCall，任务完成 — 退出
//     d. 否则通过 Registry 执行每个请求的工具调用
//     e. 将每个工具结果作为 Observation 消息追加到上下文
//     f. 重复步骤 2a
//
// 参数：
//   - ctx: 控制整个循环的取消和超时。若循环中途 context 被取消，
//     挂起的工具执行和下一次 LLM 调用将响应取消信号
//   - userPrompt: 来自人类操作者的自然语言任务描述
func (e *AgentEngine) Run(ctx context.Context, userPrompt string) error {
	log.Printf("[Engine] 引擎启动，锁定工作区: %s\n", e.WorkDir)

	// 初始化对话上下文：注入 system prompt 定义 agent 身份和能力，
	// 然后附上用户任务描述。
	contextHistory := []schema.Message{
		{
			Role:    schema.RoleSystem,
			Content: "You are harness9, an expert coding assistant. You have full access to tools in the workspace.",
		},
		{
			Role:    schema.RoleUser,
			Content: userPrompt,
		},
	}

	// turnCount 记录已完成的 Reasoning-ToolCall-Observation 迭代次数，便于调试和日志追踪。
	turnCount := 0

	// 核心 agent loop: Reasoning → ToolCall → Observation，直到终止条件满足。
	for {
		turnCount++
		log.Printf("========== [Turn %d] 开始 ==========\n", turnCount)

		// 获取当前 Turn 中 LLM 可调用的工具定义列表。
		// 在动态 Registry 中，此列表可能在 Turn 之间发生变化。
		availableTools := e.registry.GetAvailableTools()

		// --- Reasoning 阶段 ---
		// 将完整对话上下文发送给 LLM，接收模型响应（可能包含推理文本和/或工具调用请求）。
		log.Println("[Engine] 正在推理 (Reasoning)...")
		responseMsg, err := e.provider.Generate(ctx, contextHistory, availableTools)
		if err != nil {
			return fmt.Errorf("模型生成失败: %w", err)
		}

		// 将 assistant 响应追加到上下文，使后续 Turn 包含此轮对话历史。
		contextHistory = append(contextHistory, *responseMsg)

		// 输出模型产生的文本推理内容。
		if responseMsg.Content != "" {
			fmt.Printf("🤖 模型: %s\n", responseMsg.Content)
		}

		// --- 终止条件检测 ---
		// 若模型未请求任何工具调用，说明已产出最终回复，循环退出。
		if len(responseMsg.ToolCalls) == 0 {
			log.Println("[Engine] 任务完成，退出循环。")
			break
		}

		// --- ToolCall 阶段 (并发执行) ---
		// 当模型请求调用多个工具时，使用 goroutine + sync.WaitGroup 并发执行，
		// 通过预分配切片按索引写入结果，确保 Observation 消息的顺序与 ToolCall 一致。
		log.Printf("[Engine] 模型请求调用 %d 个工具...\n", len(responseMsg.ToolCalls))

		results := make([]schema.ToolResult, len(responseMsg.ToolCalls))
		var wg sync.WaitGroup

		for i, toolCall := range responseMsg.ToolCalls {
			wg.Add(1)
			go func(idx int, tc schema.ToolCall) {
				defer wg.Done()
				log.Printf("  -> 🛠️ 执行工具: %s, 参数: %s\n", tc.Name, string(tc.Arguments))

				// 将工具调用分发到 Registry 执行。
				results[idx] = e.registry.Execute(ctx, tc)

				// 记录执行结果，用于可观测性。
				if results[idx].IsError {
					log.Printf("  -> ❌ 工具执行报错: %s\n", results[idx].Output)
				} else {
					log.Printf("  -> ✅ 工具执行成功 (返回 %d 字节)\n", len(results[idx].Output))
				}
			}(i, toolCall)
		}

		wg.Wait()

		// --- Observation 阶段 ---
		// 所有工具并发执行完毕后，按原始 ToolCall 顺序将结果追加到上下文历史。
		// ToolCallID 字段确保 LLM 能将此 Observation 与其原始请求关联。
		for i, toolCall := range responseMsg.ToolCalls {
			observationMsg := schema.Message{
				Role:       schema.RoleUser,
				Content:    results[i].Output,
				ToolCallID: toolCall.ID,
			}
			contextHistory = append(contextHistory, observationMsg)
		}
	}

	return nil
}
