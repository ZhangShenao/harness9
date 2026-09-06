// Package memory — SummarizationCompactor：LLM 摘要压缩策略。
// 本文件实现基于 LLM 调用的摘要压缩器，是 harness9 默认的上下文压缩策略（优于纯截断）。
// 相关类型：Summarizer 接口（使用者侧定义）、TodoInjector 接口、SummarizationCompactor。
package memory

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/harness9/internal/schema"
)

// Summarizer 抽象了摘要压缩所需的 LLM 调用能力。
// 接口定义在使用者侧（memory 包），任何实现了 Generate 方法的 provider 均满足此接口。
type Summarizer interface {
	Generate(ctx context.Context, messages []schema.Message, availableTools []schema.ToolDefinition) (*schema.Message, *schema.Usage, error)
}

// TodoInjector 由 planning.PlanStore 实现，将活跃计划注入上下文压缩摘要。
// 定义在 memory 包（使用者侧），符合 Go 接口定义惯例。
type TodoInjector interface {
	FormatPlan() string
}

// MemoryExtractor 由 ltm.Extractor 实现，在上下文压缩前从即将被摘要的消息中
// 提取持久事实写入长期记忆。定义在 memory 包（使用者侧），避免 memory 依赖 ltm。
type MemoryExtractor interface {
	Extract(msgs []schema.Message)
}

const (
	// summaryMarker 用于标识摘要消息，支持在下次压缩时识别并增量更新。
	summaryMarker = "[Conversation Summary]"

	summarySystemPrompt = `You are a conversation summarizer. Produce a concise structured summary that preserves essential context for continuing the conversation. Output only the summary — no preamble, no explanation.`

	// summaryTemplate 用于首次摘要请求。
	summaryTemplate = "Summarize the following conversation into this structure:\n\n" +
		"**Goal:** What the user is trying to accomplish.\n" +
		"**Progress:** Key actions taken and their results.\n" +
		"**Key Decisions:** Important choices and rationale.\n" +
		"**Next Steps:** What was planned or pending when this segment ends.\n" +
		"**Critical Context:** Facts, file paths, variable names, or constraints the agent must remember.\n\n" +
		"Conversation:\n%s"

	// incrementalTemplate 用于在已有摘要的基础上进行增量更新。
	incrementalTemplate = "Update the existing summary by merging in new conversation content. " +
		"Output the merged summary in the same structure — no preamble.\n\n" +
		"<previous-summary>\n%s\n</previous-summary>\n\n" +
		"New conversation to merge:\n%s"
)

// SummarizationCompactor 使用 LLM 将旧消息压缩为结构化摘要，
// 在大幅减少 token 用量的同时保留任务关键上下文。
//
// 压缩策略：
//  1. token 估算值 ≤ MaxTokens → 直接返回（无需压缩）
//  2. 消息分割：system | head（旧消息） | tail（最近 MinTailMessages 条）
//  3. 调用 Provider 摘要 head → 单条摘要消息替代整个 head
//  4. Provider 调用失败 → 回退到 Fallback Compactor（默认 TokenBudgetCompactor）
//  5. 压缩后执行双向孤立工具对修复
//
// 增量更新：若 head 中已含摘要消息（以 summaryMarker 开头），
// 则向 LLM 提供旧摘要 + 新对话，请求合并更新，避免信息叠加丢失。
type SummarizationCompactor struct {
	Provider        Summarizer
	MaxTokens       int
	MinTailMessages int
	// Fallback 在 Provider 调用失败时使用。若为 nil，则创建同配置的 TokenBudgetCompactor。
	Fallback Compactor
	// TodoInjector 若非 nil，在每次摘要末尾注入活跃任务列表。
	TodoInjector TodoInjector
	// extractor 若非 nil，在压缩摘要前从 head 消息中提取长期记忆（fail-open）。
	extractor MemoryExtractor
}

// CompactorOption 是 NewSummarizationCompactor 的函数选项。
type CompactorOption func(*SummarizationCompactor)

// WithTodoInjector 在摘要末尾注入活跃任务列表，防止 LLM 在上下文压缩后遗忘未完成任务。
func WithTodoInjector(ti TodoInjector) CompactorOption {
	return func(c *SummarizationCompactor) { c.TodoInjector = ti }
}

// WithMemoryExtractor 注入长期记忆提取器，在每次压缩摘要前从 head 消息提取持久事实。
func WithMemoryExtractor(ex MemoryExtractor) CompactorOption {
	return func(c *SummarizationCompactor) { c.extractor = ex }
}

// NewSummarizationCompactor 创建针对指定 context window 大小的 SummarizationCompactor。
// MaxTokens 自动设为 contextWindow 的 80%；MinTailMessages 默认 6。
func NewSummarizationCompactor(p Summarizer, contextWindow int, opts ...CompactorOption) *SummarizationCompactor {
	c := &SummarizationCompactor{
		Provider:        p,
		MaxTokens:       contextWindow * 80 / 100,
		MinTailMessages: 6,
		Fallback:        NewTokenBudgetCompactor(contextWindow),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Compact 在 token 超出预算时调用 LLM 摘要旧消息，返回压缩后的消息列表。
func (c *SummarizationCompactor) Compact(msgs []schema.Message) []schema.Message {
	if EstimateTokens(msgs) <= c.maxTokens() {
		return msgs
	}
	if len(msgs) == 0 || msgs[0].Role != schema.RoleSystem {
		return msgs
	}

	minTail := c.minTail()
	rest := msgs[1:] // non-system messages

	if len(rest) <= minTail {
		return msgs
	}

	headEnd := len(rest) - minTail
	head := rest[:headEnd]
	tail := rest[headEnd:]

	// 压缩前提取：在 head 被摘要抹除前，先提取持久事实到长期记忆（fail-open）。
	if c.extractor != nil {
		c.extractor.Extract(head)
	}

	summary, err := c.summarize(head)
	if err != nil {
		return c.fallback().Compact(msgs)
	}

	return c.buildCompactedResult(msgs[0], summary, tail)
}

// CompactForce 强制压缩，跳过 token 预算阈值检查，总是尝试 LLM 摘要。
// 用于手动触发的 /compact 命令：无论当前 token 用量多少，都执行摘要压缩。
// 与 Compact 的区别：minTail 固定为 1，允许在消息数量很少时也能压缩（只保留最新 1 条）。
func (c *SummarizationCompactor) CompactForce(msgs []schema.Message) []schema.Message {
	if len(msgs) == 0 || msgs[0].Role != schema.RoleSystem {
		return msgs
	}
	const forceMinTail = 1 // 强制压缩时只保留最近 1 条消息作为 tail
	rest := msgs[1:]
	if len(rest) <= forceMinTail {
		return msgs // 只有 system + 1 条消息，无可摘要的头部
	}
	headEnd := len(rest) - forceMinTail
	head := rest[:headEnd]
	tail := rest[headEnd:]
	if c.extractor != nil {
		c.extractor.Extract(head)
	}
	summary, err := c.summarize(head)
	if err != nil {
		// 摘要失败时回退到 TokenBudgetCompactor 的强制压缩
		if fb, ok := c.fallback().(ForceCompactor); ok {
			return fb.CompactForce(msgs)
		}
		return c.fallback().Compact(msgs)
	}
	return c.buildCompactedResult(msgs[0], summary, tail)
}

// buildCompactedResult 将 system 消息、LLM 摘要文本和尾部消息组合为压缩后的消息列表。
// Compact 和 CompactForce 共用此方法，避免重复拼装逻辑。
func (c *SummarizationCompactor) buildCompactedResult(systemMsg schema.Message, summary string, tail []schema.Message) []schema.Message {
	summaryContent := summaryMarker + "\n" + summary
	if c.TodoInjector != nil {
		if planText := c.TodoInjector.FormatPlan(); planText != "" {
			summaryContent += "\n\n## Active Tasks\n" + planText
		}
	}
	summaryMsg := schema.Message{
		Role:    schema.RoleUser,
		Content: summaryContent,
	}
	result := make([]schema.Message, 0, 2+len(tail))
	result = append(result, systemMsg)  // system 始终保留
	result = append(result, summaryMsg) // LLM 生成的摘要
	result = append(result, tail...)    // 最近消息原样保留
	return repairOrphanedToolPairs(result)
}

// summarize 调用 LLM 对 head 消息生成结构化摘要字符串。
// 若 head 中已包含上次摘要（summaryMarker），则执行增量更新。
func (c *SummarizationCompactor) summarize(head []schema.Message) (string, error) {
	if c.Provider == nil {
		return "", fmt.Errorf("SummarizationCompactor: Provider is nil")
	}
	var prevSummary string
	var lines []string

	for _, m := range head {
		// 检测上次摘要消息，提取内容用于增量更新，不将其加入对话文本。
		if strings.HasPrefix(m.Content, summaryMarker) {
			prevSummary = strings.TrimPrefix(m.Content, summaryMarker+"\n")
			continue
		}
		// 工具执行结果（Observation）
		if m.ToolCallID != "" {
			lines = append(lines, fmt.Sprintf("[tool_result %s]: %s", m.ToolCallID, m.Content))
			continue
		}
		// 文本内容
		if m.Content != "" {
			lines = append(lines, fmt.Sprintf("[%s]: %s", m.Role, m.Content))
		}
		// 工具调用请求
		for _, tc := range m.ToolCalls {
			lines = append(lines, fmt.Sprintf("[tool_call %s(%s)]: %s", tc.Name, tc.ID, string(tc.Arguments)))
		}
	}

	conversationText := strings.Join(lines, "\n")

	var userContent string
	if prevSummary != "" {
		userContent = fmt.Sprintf(incrementalTemplate, prevSummary, conversationText)
	} else {
		userContent = fmt.Sprintf(summaryTemplate, conversationText)
	}

	sysMsg := schema.Message{Role: schema.RoleSystem, Content: summarySystemPrompt}
	userMsg := schema.Message{Role: schema.RoleUser, Content: userContent}

	// 为摘要 LLM 调用设置独立超时（60 秒）。
	// Compact 接口不传递外层 context，因此此处无法感知外层取消；
	// 超时可防止摘要请求无限阻塞，确保 runLoop 在网络异常时能够回退到 Fallback 压缩器。
	summaryCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	resp, _, err := c.Provider.Generate(summaryCtx, []schema.Message{sysMsg, userMsg}, nil)
	if err != nil {
		return "", err
	}
	if resp == nil {
		return "", fmt.Errorf("summarizer returned nil message")
	}
	return resp.Content, nil
}

func (c *SummarizationCompactor) maxTokens() int {
	if c.MaxTokens <= 0 {
		return 160_000
	}
	return c.MaxTokens
}

func (c *SummarizationCompactor) minTail() int {
	if c.MinTailMessages <= 0 {
		return 6
	}
	return c.MinTailMessages
}

func (c *SummarizationCompactor) fallback() Compactor {
	if c.Fallback != nil {
		return c.Fallback
	}
	return &TokenBudgetCompactor{
		MaxTokens:       c.maxTokens(),
		MinTailMessages: c.minTail(),
	}
}
