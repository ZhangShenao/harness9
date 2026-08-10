// Package memory 提供 harness9 的短期记忆管理：会话历史持久化与上下文压缩。
// 本文件定义 Compactor 接口和三种实现：SlidingWindowCompactor（滑动窗口）、
// TokenBudgetCompactor（token 预算）和辅助的 repairOrphanedToolPairs 修复函数。
package memory

import "github.com/harness9/internal/schema"

// Compactor 在将历史消息注入 LLM 上下文前进行裁剪，防止超出上下文窗口。
// 接口设计允许后续扩展 TokenBudgetCompactor、LLMSummarizationCompactor 等策略。
type Compactor interface {
	Compact(msgs []schema.Message) []schema.Message
}

// ForceCompactor 扩展 Compactor，提供跳过 token 阈值检查的强制压缩能力。
// 由 engine.Compact（/compact 手动命令）使用，强制执行压缩而不受 80% 阈值约束。
type ForceCompactor interface {
	Compactor
	CompactForce(msgs []schema.Message) []schema.Message
}

// RecordedCompactor 扩展 Compactor，返回压缩记录供追踪和持久化。
// ProgressiveCompactor 实现此接口，engine 通过类型断言检测并消费 CompactionRecord。
type RecordedCompactor interface {
	Compactor
	CompactWithRecord(msgs []schema.Message) ([]schema.Message, CompactionRecord)
}

// SlidingWindowCompactor 保留最近 MaxMessages 条消息（System Prompt 固定在首位）。
// MaxMessages 含 system 消息本身；0 或负数时使用默认值 100。
type SlidingWindowCompactor struct {
	MaxMessages int
}

// Compact 对 msgs 进行滑动窗口裁剪，返回裁剪后的切片。
//
// 边界修正：若窗口第一条消息是工具执行结果（ToolCallID != ""），
// 向前回溯直到找到配对的 assistant 工具请求消息，保证上下文完整。
func (c *SlidingWindowCompactor) Compact(msgs []schema.Message) []schema.Message {
	if len(msgs) == 0 || msgs[0].Role != schema.RoleSystem {
		return msgs
	}

	max := c.MaxMessages
	if max <= 0 {
		max = 100
	}
	if max < 2 {
		max = 2 // must hold at least system + one turn
	}
	if len(msgs) <= max {
		return msgs
	}

	// startIdx 为窗口中第一条非 system 消息的索引（msgs[0] 始终是 system）
	startIdx := len(msgs) - max + 1

	// 边界修正：回溯孤立的 Observation 消息
	for startIdx > 1 && msgs[startIdx].ToolCallID != "" {
		startIdx--
	}

	result := make([]schema.Message, 0, len(msgs)-startIdx+1)
	result = append(result, msgs[0]) // system 始终保留
	result = append(result, msgs[startIdx:]...)
	// 窗口边界可能裁掉 assistant 工具调用请求而保留其结果（或反之），
	// 导致 Anthropic API 400（tool_call/tool_result 必须成对）。
	// 调用 repairOrphanedToolPairs 执行双向修复，与 TokenBudgetCompactor 保持一致。
	return repairOrphanedToolPairs(result)
}

// TokenBudgetCompactor 基于 token 预算（字符数÷4 估算）而非消息条数进行上下文裁剪。
// 这是 harness9 推荐的默认 Compactor，能感知实际 token 用量，适配不同模型的上下文窗口。
//
// 压缩策略：
//  1. token 总数 ≤ MaxTokens 时直接返回（无需压缩）
//  2. 从中间删除旧消息，保留 system 消息和最近 MinTailMessages 条消息
//  3. 压缩后执行双向孤立消息修复（HermesAgent 模式）
type TokenBudgetCompactor struct {
	// MaxTokens 是压缩目标上限（tokens 估算值）。通常设为模型 context window 的 80%。
	MaxTokens int
	// MinTailMessages 是无论预算如何都必须保留的最近非 system 消息数量（默认 6）。
	MinTailMessages int
}

// NewTokenBudgetCompactor 创建针对指定 context window 大小的 TokenBudgetCompactor。
// MaxTokens 自动设为 contextWindow 的 80%。
func NewTokenBudgetCompactor(contextWindow int) *TokenBudgetCompactor {
	return &TokenBudgetCompactor{
		MaxTokens:       contextWindow * 80 / 100,
		MinTailMessages: 6,
	}
}

// Compact 压缩消息历史至 MaxTokens 预算内，始终保留 system 消息和最近 MinTailMessages 条消息。
func (c *TokenBudgetCompactor) Compact(msgs []schema.Message) []schema.Message {
	if len(msgs) == 0 {
		return msgs
	}
	if EstimateTokens(msgs) <= c.maxTokens() {
		return msgs
	}
	// 必须以 system 消息开头
	if msgs[0].Role != schema.RoleSystem {
		return msgs
	}

	minTail := c.minTail()
	rest := msgs[1:] // non-system messages

	if len(rest) <= minTail {
		return msgs
	}

	tail := rest[len(rest)-minTail:]

	// 逐条剥离头部旧消息，直到 token 预算满足或头部清空。
	for headEnd := len(rest) - minTail; headEnd > 0; headEnd-- {
		candidate := make([]schema.Message, 0, 1+headEnd+minTail)
		candidate = append(candidate, msgs[0])
		candidate = append(candidate, rest[:headEnd]...)
		candidate = append(candidate, tail...)
		if EstimateTokens(candidate) <= c.maxTokens() {
			return repairOrphanedToolPairs(candidate)
		}
	}

	// 头部完全移除：仅保留 system 消息 + 尾部最近消息。
	result := make([]schema.Message, 0, 1+len(tail))
	result = append(result, msgs[0])
	result = append(result, tail...)
	return repairOrphanedToolPairs(result)
}

// CompactForce 强制压缩，跳过 token 预算阈值检查，直接移除最旧的非 system 消息。
// 用于手动触发的 /compact 命令：无论当前 token 用量多少，都执行一次结构裁剪。
// 与 Compact 的区别：minTail 固定为 1，允许在消息数量很少时也能压缩。
//
// 注意：此方法不做 LLM 摘要，直接丢弃旧消息，仅保留 system + 最新 1 条；
// 通常作为 SummarizationCompactor.CompactForce 摘要失败时的 fallback。
func (c *TokenBudgetCompactor) CompactForce(msgs []schema.Message) []schema.Message {
	if len(msgs) == 0 || msgs[0].Role != schema.RoleSystem {
		return msgs
	}
	const forceMinTail = 1
	rest := msgs[1:]
	if len(rest) <= forceMinTail {
		return msgs // 只有 system + 1 条消息，无可裁剪的头部
	}
	// 直接移除中间最旧的非 system 消息，只保留 system + 最近 1 条。
	tail := rest[len(rest)-forceMinTail:]
	result := make([]schema.Message, 0, 1+len(tail))
	result = append(result, msgs[0])
	result = append(result, tail...)
	return repairOrphanedToolPairs(result)
}

func (c *TokenBudgetCompactor) maxTokens() int {
	if c.MaxTokens <= 0 {
		return 160_000 // 200K * 80% conservative default
	}
	return c.MaxTokens
}

func (c *TokenBudgetCompactor) minTail() int {
	if c.MinTailMessages <= 0 {
		return 6
	}
	return c.MinTailMessages
}

// repairOrphanedToolPairs 在压缩后执行双向工具对完整性修复（HermesAgent 模式）：
//  1. 删除无对应 tool_call 的孤立 user tool_result 消息
//  2. 为缺少响应的 assistant tool_call 插入占位 user tool_result
//
// Anthropic Messages API 要求 tool_call 与 tool_result 必须成对出现，违反此约束会导致 API 400 错误。
func repairOrphanedToolPairs(msgs []schema.Message) []schema.Message {
	// 收集所有 assistant 发起的 tool_call ID 集合。
	calledIDs := make(map[string]bool)
	for _, m := range msgs {
		if m.Role == schema.RoleAssistant {
			for _, tc := range m.ToolCalls {
				calledIDs[tc.ID] = true
			}
		}
	}
	// 收集所有已有 tool_result 的 ID 集合。
	resultIDs := make(map[string]bool)
	for _, m := range msgs {
		if m.ToolCallID != "" {
			resultIDs[m.ToolCallID] = true
		}
	}

	result := make([]schema.Message, 0, len(msgs))
	for _, m := range msgs {
		// 删除孤立的 tool_result（无对应的 tool_call）。
		if m.ToolCallID != "" && !calledIDs[m.ToolCallID] {
			continue
		}
		result = append(result, m)
		// 为缺少 tool_result 的 tool_call 插入占位消息。
		if m.Role == schema.RoleAssistant && len(m.ToolCalls) > 0 {
			for _, tc := range m.ToolCalls {
				if !resultIDs[tc.ID] {
					result = append(result, schema.Message{
						Role:       schema.RoleUser,
						Content:    "[工具结果不可用：上下文已被压缩]",
						ToolCallID: tc.ID,
					})
				}
			}
		}
	}
	return result
}
