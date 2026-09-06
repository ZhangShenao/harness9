// Package memory - ProgressiveCompactor：分级渐进式上下文压缩器。
// 本文件实现 ProgressiveCompactor，按上下文占用比例分级触发压缩：
//   - TierNone（<60%）：无操作
//   - TierWarn（60%-70%）：仅 offload 超大工具结果，不调用 LLM
//   - TierSoft（70%-80%）：摘要头部旧消息的一半 + offload，保留较多尾部
//   - TierFull（80%-95%）：摘要全部头部 + 锚点提取 + offload，仅保留最小尾部
//   - TierEmergency（≥95%）：跳过 LLM，直接回退到强制截断保命
//
// ProgressiveCompactor 实现 Compactor + ForceCompactor + RecordedCompactor 三个接口，
// 返回 CompactionRecord 携带锚点、外存条目、压缩比等审计信息，供 engine 消费与持久化。
// 通过 SetLastSummary / SetLastAnchors 支持跨轮增量更新，避免信息叠加丢失。
package memory

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/harness9/internal/logfmt"
	"github.com/harness9/internal/schema"
)

const (
	compactionSummarySystemPrompt = `You are a context compaction engine. Analyze the conversation and produce a structured compaction preserving essential context. Output only the compaction - no preamble.`

	compactionFirstTemplate = `Produce a structured compaction of the following conversation in this exact format:

## Anchors

### User Intent
<one concise sentence>

### Execution Progress
- <key milestone>

### Key Decisions
- <decision: rationale>

### Tried Solutions
- <approach: outcome>

### Next Steps
- <pending task>

## Summary
<supplementary context not captured in anchors>

Rules:
- Each anchor section MUST be present (use "- N/A" if nothing applies)
- Be concise: each item one line
- [offloaded: ...] entries indicate large outputs saved to files

Conversation:
%s`

	compactionIncrementalTemplate = `Update the existing compaction by merging in new conversation content. Output the merged compaction in the same format - no preamble.

<previous-compaction>
%s
</previous-compaction>

New conversation to merge:
%s`
)

// ProgressiveCompactor 按 token 占用比例分级触发压缩，编排 offload、锚点提取、
// LLM 摘要与截断回退，是 harness9 渐进式上下文压缩的核心组件。
//
// 字段说明：
//   - Provider / Fallback / extractor / offloader / recordStore：
//     通过构造选项注入的协作组件，nil 时各 tier 自行降级处理。
//   - lastSummary / lastAnchors：跨轮增量更新状态，由 SetLastSummary / SetLastAnchors
//     或成功的 TierSoft/TierFull 调用更新。
//   - 阈值字段：分级触发档位，默认 0.60/0.70/0.80/0.95，可按模型特性调整。
type ProgressiveCompactor struct {
	Provider        Summarizer
	ContextWindow   int
	MinTailMessages int
	Fallback        Compactor
	extractor       MemoryExtractor
	offloader       *CompactionOffloader
	sessionID       string
	recordStore     RecordStore

	lastSummary string
	lastAnchors []Anchor

	WarnThreshold      float64
	SoftThreshold      float64
	FullThreshold      float64
	EmergencyThreshold float64
	OffloadThreshold   int
}

// ProgressiveOption 是 NewProgressiveCompactor 的函数选项。
type ProgressiveOption func(*ProgressiveCompactor)

// WithProgressiveMemoryExtractor 注入长期记忆提取器，在 head 被摘要抹除前提取持久事实。
func WithProgressiveMemoryExtractor(ex MemoryExtractor) ProgressiveOption {
	return func(c *ProgressiveCompactor) { c.extractor = ex }
}

// WithProgressiveRecordStore 注入压缩记录持久化存储，每次非 TierNone 压缩后追加一条记录。
func WithProgressiveRecordStore(rs RecordStore) ProgressiveOption {
	return func(c *ProgressiveCompactor) { c.recordStore = rs }
}

// WithProgressiveOffloader 注入工具结果外存器，在压缩期将超大 tool_result 写入文件系统。
func WithProgressiveOffloader(o *CompactionOffloader) ProgressiveOption {
	return func(c *ProgressiveCompactor) { c.offloader = o }
}

// WithProgressiveSessionID 设置会话 ID，用于压缩记录与外存文件的归属关联。
func WithProgressiveSessionID(id string) ProgressiveOption {
	return func(c *ProgressiveCompactor) { c.sessionID = id }
}

// NewProgressiveCompactor 创建针对指定 context window 大小的 ProgressiveCompactor。
// 阈值默认 0.60/0.70/0.80/0.95，MinTailMessages 默认 6，Fallback 默认同配置的 TokenBudgetCompactor。
func NewProgressiveCompactor(p Summarizer, contextWindow int, opts ...ProgressiveOption) *ProgressiveCompactor {
	c := &ProgressiveCompactor{
		Provider:           p,
		ContextWindow:      contextWindow,
		MinTailMessages:    6,
		Fallback:           NewTokenBudgetCompactor(contextWindow),
		WarnThreshold:      0.60,
		SoftThreshold:      0.70,
		FullThreshold:      0.80,
		EmergencyThreshold: 0.95,
		OffloadThreshold:   4000,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// SetLastSummary 设置上次压缩的摘要文本，用于下次压缩的增量更新。
func (c *ProgressiveCompactor) SetLastSummary(s string) { c.lastSummary = s }

// SetLastAnchors 设置上次压缩的锚点列表，用于下次压缩的增量合并。
func (c *ProgressiveCompactor) SetLastAnchors(a []Anchor) { c.lastAnchors = a }

// Compact 实现 Compactor 接口，委托给 CompactWithRecord 并丢弃记录。
// 供仅需要压缩结果、不关心审计记录的调用方使用。
func (c *ProgressiveCompactor) Compact(msgs []schema.Message) []schema.Message {
	result, _ := c.CompactWithRecord(msgs)
	return result
}

// CompactForce 实现 ForceCompactor 接口，跳过阈值检查直接执行紧急截断压缩。
// 用于手动触发的 /compact 命令：无论当前 token 用量多少，都执行激进压缩保命。
func (c *ProgressiveCompactor) CompactForce(msgs []schema.Message) []schema.Message {
	result, _ := c.tierEmergency(msgs)
	return result
}

// CompactWithRecord 实现 RecordedCompactor 接口，按分级策略压缩并返回完整审计记录。
// 记录中的 ID/SessionID/Timestamp/Tier/Tokens/Msgs/Duration 等字段在此处统一填充；
// tier 特有的 Anchors/Offloaded/Summarized/SummaryText/Error 字段由各 tier 方法设置。
func (c *ProgressiveCompactor) CompactWithRecord(msgs []schema.Message) ([]schema.Message, CompactionRecord) {
	start := time.Now()
	tier := c.determineTier(msgs)
	tokensBefore := EstimateTokens(msgs)
	msgsBefore := len(msgs)

	var result []schema.Message
	var record CompactionRecord

	switch tier {
	case TierNone:
		result = msgs
	case TierWarn:
		result, record = c.tierWarn(msgs)
	case TierSoft:
		result, record = c.tierSoft(msgs)
	case TierFull:
		result, record = c.tierFull(msgs)
	case TierEmergency:
		result, record = c.tierEmergency(msgs)
	}

	record.ID = newUUID()
	record.SessionID = c.sessionID
	record.Timestamp = time.Now()
	record.Tier = tier
	record.TokensBefore = tokensBefore
	record.TokensAfter = EstimateTokens(result)
	record.MsgsBefore = msgsBefore
	record.MsgsAfter = len(result)
	record.Duration = time.Since(start)
	if tokensBefore > 0 {
		record.CompressionRatio = float64(record.TokensAfter) / float64(tokensBefore)
	}

	if c.recordStore != nil && tier != TierNone {
		if err := c.recordStore.Append(record); err != nil {
			log.Print(logfmt.FormatMsg("compactor",
				fmt.Sprintf("压缩记录持久化失败: %v", err)))
		}
	}

	return result, record
}

// determineTier 按 token 占用比例（EstimateTokens/ContextWindow）选择压缩档位。
// ContextWindow<=0 时视为无预算，返回 TierNone。
func (c *ProgressiveCompactor) determineTier(msgs []schema.Message) CompactionTier {
	if c.ContextWindow <= 0 {
		return TierNone
	}
	ratio := float64(EstimateTokens(msgs)) / float64(c.ContextWindow)
	switch {
	case ratio >= c.EmergencyThreshold:
		return TierEmergency
	case ratio >= c.FullThreshold:
		return TierFull
	case ratio >= c.SoftThreshold:
		return TierSoft
	case ratio >= c.WarnThreshold:
		return TierWarn
	default:
		return TierNone
	}
}

// splitHeadTail 将 msgs 分为 [system][head...][tail...] 三段，返回 head 与 tail。
// head 为可被压缩的旧消息，tail 为必须原样保留的最近消息。
// 首条消息不是 system 时返回 nil head（调用方据此跳过压缩）。
// 当非 system 消息数量 ≤ MinTailMessages 时也返回 nil head（无可压缩头部）。
func (c *ProgressiveCompactor) splitHeadTail(msgs []schema.Message) ([]schema.Message, []schema.Message) {
	if len(msgs) == 0 || msgs[0].Role != schema.RoleSystem {
		return nil, msgs
	}
	minTail := c.minTail()
	rest := msgs[1:]
	if len(rest) <= minTail {
		return nil, rest
	}
	headEnd := len(rest) - minTail
	return rest[:headEnd], rest[headEnd:]
}

// tierWarn 执行预警档压缩：仅 offload head 中的超大工具结果，不调用 LLM。
// head 与 tail 原样保留，仅替换超大 tool_result 内容为占位符。
func (c *ProgressiveCompactor) tierWarn(msgs []schema.Message) ([]schema.Message, CompactionRecord) {
	head, tail := c.splitHeadTail(msgs)
	if head == nil {
		return msgs, CompactionRecord{}
	}
	offloaded := c.offloadHead(head)
	result := make([]schema.Message, 0, 1+len(head)+len(tail))
	result = append(result, msgs[0])
	result = append(result, head...)
	result = append(result, tail...)
	result = repairOrphanedToolPairs(result)
	return result, CompactionRecord{Offloaded: offloaded, PreservedTail: len(tail)}
}

// tierSoft 执行软压缩：将 head 一分为二，仅摘要前半部分（headOldest），
// 保留后半部分（headRecent）原文，兼顾压缩率与上下文连续性。
// 失败时回退到 Fallback Compactor 并记录错误原因。
func (c *ProgressiveCompactor) tierSoft(msgs []schema.Message) ([]schema.Message, CompactionRecord) {
	head, tail := c.splitHeadTail(msgs)
	if head == nil {
		return msgs, CompactionRecord{}
	}
	mid := len(head) / 2
	headOldest := head[:mid]
	headRecent := head[mid:]

	if c.extractor != nil {
		c.extractor.Extract(headOldest)
	}
	offloaded := c.offloadHead(headOldest)

	summary, anchors, err := c.summarizeAndExtract(headOldest)
	if err != nil {
		return c.fallbackCompact(msgs, "LLM summary failed in TierSoft")
	}

	compactionMsg := c.buildCompactionMsg(anchors, summary, offloaded)
	result := make([]schema.Message, 0, 2+len(headRecent)+len(tail))
	result = append(result, msgs[0])
	result = append(result, compactionMsg)
	result = append(result, headRecent...)
	result = append(result, tail...)
	result = repairOrphanedToolPairs(result)

	c.updateLastState(summary, anchors)
	return result, CompactionRecord{
		Anchors:       anchors,
		Offloaded:     offloaded,
		Summarized:    len(headOldest),
		PreservedTail: len(tail) + len(headRecent),
		SummaryText:   summary,
	}
}

// tierFull 执行全量压缩：摘要整个 head + 锚点提取 + offload，仅保留最小尾部。
// 失败时回退到 Fallback Compactor 并记录错误原因。
func (c *ProgressiveCompactor) tierFull(msgs []schema.Message) ([]schema.Message, CompactionRecord) {
	head, tail := c.splitHeadTail(msgs)
	if head == nil {
		return msgs, CompactionRecord{}
	}

	if c.extractor != nil {
		c.extractor.Extract(head)
	}
	offloaded := c.offloadHead(head)

	summary, anchors, err := c.summarizeAndExtract(head)
	if err != nil {
		return c.fallbackCompact(msgs, "LLM summary failed in TierFull")
	}

	compactionMsg := c.buildCompactionMsg(anchors, summary, offloaded)
	result := make([]schema.Message, 0, 2+len(tail))
	result = append(result, msgs[0])
	result = append(result, compactionMsg)
	result = append(result, tail...)
	result = repairOrphanedToolPairs(result)

	c.updateLastState(summary, anchors)
	return result, CompactionRecord{
		Anchors:       anchors,
		Offloaded:     offloaded,
		Summarized:    len(head),
		PreservedTail: len(tail),
		SummaryText:   summary,
	}
}

// tierEmergency 执行紧急压缩：跳过 LLM 摘要，直接回退到 Fallback 的强制截断。
// 若 Fallback 实现 ForceCompactor 接口则调用 CompactForce，否则调用 Compact。
// 始终在记录中设置 Error 字段标识紧急降级。
func (c *ProgressiveCompactor) tierEmergency(msgs []schema.Message) ([]schema.Message, CompactionRecord) {
	var result []schema.Message
	if fc, ok := c.fallback().(ForceCompactor); ok {
		result = fc.CompactForce(msgs)
	} else {
		result = c.fallback().Compact(msgs)
	}
	return result, CompactionRecord{
		PreservedTail: len(result) - 1,
		Error:         "emergency fallback: forced truncation",
	}
}

// offloadHead 遍历 head 中的 tool_result 消息，将超过 OffloadThreshold 的内容
// 通过 offloader 写入文件系统并替换为带预览的占位符。
// offloader 为 nil 时直接返回 nil（无外存能力，跳过）。
// 写入失败的单条消息会被静默跳过（fail-open），不影响整体压缩流程。
func (c *ProgressiveCompactor) offloadHead(head []schema.Message) []OffloadEntry {
	if c.offloader == nil {
		return nil
	}
	var entries []OffloadEntry
	for i, msg := range head {
		if msg.ToolCallID == "" || len(msg.Content) <= c.OffloadThreshold {
			continue
		}
		entry, placeholder, err := c.offloader.OffloadToolResult(msg)
		if err != nil {
			continue
		}
		head[i].Content = placeholder
		entries = append(entries, entry)
	}
	return entries
}

// summarizeAndExtract 调用 LLM 对 head 生成结构化压缩（锚点 + 摘要正文）。
// 若 lastSummary 非空，则使用增量更新模板（合并旧压缩 + 新对话）；
// 否则使用首次压缩模板。lastAnchors 非空时与 LLM 输出锚点执行 MergeAnchors。
// Provider 为 nil 或返回 nil 消息时返回 error，触发上层回退。
func (c *ProgressiveCompactor) summarizeAndExtract(head []schema.Message) (string, []Anchor, error) {
	if c.Provider == nil {
		return "", nil, fmt.Errorf("provider is nil")
	}

	var lines []string
	for _, m := range head {
		if m.ToolCallID != "" {
			lines = append(lines, fmt.Sprintf("[tool_result %s]: %s", m.ToolCallID, m.Content))
			continue
		}
		if m.Content != "" {
			lines = append(lines, fmt.Sprintf("[%s]: %s", m.Role, m.Content))
		}
		for _, tc := range m.ToolCalls {
			lines = append(lines, fmt.Sprintf("[tool_call %s(%s)]: %s", tc.Name, tc.ID, string(tc.Arguments)))
		}
	}
	conversationText := strings.Join(lines, "\n")

	var userContent string
	if c.lastSummary != "" {
		prevCompaction := c.lastSummary
		if len(c.lastAnchors) > 0 {
			prevCompaction = c.formatAnchors(c.lastAnchors) + "\n\n## Summary\n" + c.lastSummary
		}
		userContent = fmt.Sprintf(compactionIncrementalTemplate, prevCompaction, conversationText)
	} else {
		userContent = fmt.Sprintf(compactionFirstTemplate, conversationText)
	}

	sysMsg := schema.Message{Role: schema.RoleSystem, Content: compactionSummarySystemPrompt}
	userMsg := schema.Message{Role: schema.RoleUser, Content: userContent}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	resp, _, err := c.Provider.Generate(ctx, []schema.Message{sysMsg, userMsg}, nil)
	if err != nil {
		return "", nil, err
	}
	if resp == nil {
		return "", nil, fmt.Errorf("summarizer returned nil message")
	}

	anchors, summary := ParseAnchorsAndSummary(resp.Content)
	if c.lastAnchors != nil {
		anchors = MergeAnchors(c.lastAnchors, anchors)
	}
	return summary, anchors, nil
}

// buildCompactionMsg 组装压缩后的替代消息（RoleUser），结构为：
//
//	[Context Compaction]
//	## Anchors
//	<formatAnchors 输出>
//	## Summary
//	<summary>
//	## Offloaded References (若有)
func (c *ProgressiveCompactor) buildCompactionMsg(anchors []Anchor, summary string, offloaded []OffloadEntry) schema.Message {
	var sb strings.Builder
	sb.WriteString(compactionMarker)
	sb.WriteString("\n## Anchors\n\n")
	sb.WriteString(c.formatAnchors(anchors))
	sb.WriteString("\n\n## Summary\n")
	sb.WriteString(summary)
	if len(offloaded) > 0 {
		sb.WriteString("\n\n## Offloaded References\n")
		for _, e := range offloaded {
			sb.WriteString(fmt.Sprintf("- %s (%d行) - offloaded tool result\n", e.FilePath, e.Lines))
		}
	}
	return schema.Message{Role: schema.RoleUser, Content: sb.String()}
}

// formatAnchors 将锚点列表渲染为 Markdown 三级标题段，顺序遵循 allAnchorTypes。
func (c *ProgressiveCompactor) formatAnchors(anchors []Anchor) string {
	var sb strings.Builder
	for _, a := range anchors {
		header := ""
		for h, at := range anchorHeaderMap {
			if at == a.Type {
				header = h
				break
			}
		}
		sb.WriteString("### ")
		sb.WriteString(header)
		sb.WriteString("\n")
		sb.WriteString(a.Content)
		sb.WriteString("\n\n")
	}
	return strings.TrimSpace(sb.String())
}

// updateLastState 更新跨轮增量状态，供下次压缩的增量更新模板使用。
func (c *ProgressiveCompactor) updateLastState(summary string, anchors []Anchor) {
	c.lastSummary = summary
	c.lastAnchors = anchors
}

// fallbackCompact 在 LLM 摘要失败时回退到 Fallback Compactor 并记录错误原因。
func (c *ProgressiveCompactor) fallbackCompact(msgs []schema.Message, reason string) ([]schema.Message, CompactionRecord) {
	result := c.fallback().Compact(msgs)
	return result, CompactionRecord{
		PreservedTail: len(result) - 1,
		Error:         reason,
	}
}

// fallback 返回 Fallback Compactor；为 nil 时即时构造同配置的 TokenBudgetCompactor。
func (c *ProgressiveCompactor) fallback() Compactor {
	if c.Fallback != nil {
		return c.Fallback
	}
	return &TokenBudgetCompactor{
		MaxTokens:       c.ContextWindow * 80 / 100,
		MinTailMessages: c.minTail(),
	}
}

// minTail 返回 MinTailMessages，<=0 时使用默认值 6。
func (c *ProgressiveCompactor) minTail() int {
	if c.MinTailMessages <= 0 {
		return 6
	}
	return c.MinTailMessages
}
