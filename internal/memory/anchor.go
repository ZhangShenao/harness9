// Package memory - Anchor 类型与解析：ProgressiveCompactor 的结构化锚点系统。
// 本文件定义 AnchorType 枚举、Anchor 结构体，以及从 LLM 摘要输出中提取锚点与摘要正文的解析器。
// ParseAnchorsAndSummary 解析固定五类锚点（缺失填 N/A）与 Summary 段落；
// MergeAnchors 合并新旧锚点，新值覆盖旧值，N/A 视为缺失不覆盖。
package memory

import "strings"

// AnchorType 标识锚点类别。ProgressiveCompactor 以五类锚点结构化保存上下文精华。
type AnchorType string

const (
	AnchorUserIntent        AnchorType = "user_intent"
	AnchorExecutionProgress AnchorType = "execution_progress"
	AnchorKeyDecision       AnchorType = "key_decision"
	AnchorTriedSolution     AnchorType = "tried_solution"
	AnchorNextStep          AnchorType = "next_step"
)

// allAnchorTypes 规定锚点的固定输出顺序，确保解析与合并结果稳定。
var allAnchorTypes = []AnchorType{
	AnchorUserIntent, AnchorExecutionProgress, AnchorKeyDecision,
	AnchorTriedSolution, AnchorNextStep,
}

// anchorHeaderMap 将 Markdown 三级标题映射到 AnchorType。
var anchorHeaderMap = map[string]AnchorType{
	"User Intent":        AnchorUserIntent,
	"Execution Progress": AnchorExecutionProgress,
	"Key Decisions":      AnchorKeyDecision,
	"Tried Solutions":    AnchorTriedSolution,
	"Next Steps":         AnchorNextStep,
}

// Anchor 是单个结构化锚点，由 ProgressiveCompactor 在压缩时提取并跨轮保留。
type Anchor struct {
	Type    AnchorType `json:"type"`
	Content string     `json:"content"`
}

// compactionMarker 标识 ProgressiveCompactor 产生的压缩消息，供后续轮次识别并增量更新。
const compactionMarker = "[Context Compaction]"

// ParseAnchorsAndSummary 从 LLM 摘要文本中解析五类锚点与 Summary 段落。
// 缺失的锚点以 "N/A" 填充；始终返回长度为 5 的锚点切片，顺序固定。
// 解析遵循 "## Anchors" 下方的 "### <Header>" 子段与 "## Summary" 段。
func ParseAnchorsAndSummary(text string) ([]Anchor, string) {
	parsed := make(map[AnchorType]string)
	lines := strings.Split(text, "\n")
	var currentType AnchorType
	var currentLines []string
	var summary string
	inSummary := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "## Summary" {
			if currentType != "" {
				parsed[currentType] = strings.TrimSpace(strings.Join(currentLines, "\n"))
				currentType = ""
				currentLines = nil
			}
			inSummary = true
			continue
		}
		if inSummary {
			summary += line + "\n"
			continue
		}
		if strings.HasPrefix(trimmed, "### ") {
			if currentType != "" {
				parsed[currentType] = strings.TrimSpace(strings.Join(currentLines, "\n"))
				currentLines = nil
			}
			header := strings.TrimPrefix(trimmed, "### ")
			if at, ok := anchorHeaderMap[header]; ok {
				currentType = at
			} else {
				currentType = ""
			}
			continue
		}
		if currentType != "" {
			currentLines = append(currentLines, line)
		}
	}
	if currentType != "" {
		parsed[currentType] = strings.TrimSpace(strings.Join(currentLines, "\n"))
	}

	result := make([]Anchor, 0, len(allAnchorTypes))
	for _, at := range allAnchorTypes {
		content := parsed[at]
		if content == "" {
			content = "N/A"
		}
		result = append(result, Anchor{Type: at, Content: content})
	}
	return result, strings.TrimSpace(summary)
}

// MergeAnchors 合并新旧锚点：new 中非 "N/A" 的值覆盖 old 的同类型值。
// 始终返回按 allAnchorTypes 顺序排列、长度为 5 的锚点切片。
func MergeAnchors(old, new []Anchor) []Anchor {
	m := make(map[AnchorType]string)
	for _, a := range old {
		m[a.Type] = a.Content
	}
	for _, a := range new {
		if a.Content != "N/A" {
			m[a.Type] = a.Content
		}
	}
	result := make([]Anchor, 0, len(allAnchorTypes))
	for _, at := range allAnchorTypes {
		content := m[at]
		if content == "" {
			content = "N/A"
		}
		result = append(result, Anchor{Type: at, Content: content})
	}
	return result
}
