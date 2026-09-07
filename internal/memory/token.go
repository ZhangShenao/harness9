// Package memory — token 估算工具。
// 本文件提供基于字符数÷4 的轻量级 token 估算函数，与 HermesAgent / OpenCode 策略一致。
// 无外部依赖，略偏保守（实际 token 数通常低于估算），适合 80% 阈值触发压缩的场景。
package memory

import (
	"encoding/json"
	"fmt"

	"github.com/harness9/internal/schema"
)

// charsPerToken 是字符数到 token 数的估算比例（字符数÷4）。
// 与 HermesAgent、DeepAgents 和 OpenCode 的策略一致：简单、无外部依赖、略偏保守。
const charsPerToken = 4

// EstimateTokens 估算消息列表的总 token 数（字符数÷4）。
// 计算范围包含消息文本内容、工具调用参数（Arguments）和工具调用 ID（ToolCallID）。
func EstimateTokens(msgs []schema.Message) int {
	total := 0
	for _, m := range msgs {
		total += len(m.Content)
		for _, tc := range m.ToolCalls {
			total += len(tc.ID) + len(tc.Name) + len(tc.Arguments)
		}
		total += len(m.ToolCallID)
	}
	return total / charsPerToken
}

// EstimateToolTokens 估算工具定义列表的 token 用量（字符数÷4）。
// 工具 Schema 在工具数量较多时可能消耗 20-30K+ token，预飞检查（preflight）时必须纳入计算。
func EstimateToolTokens(tools []schema.ToolDefinition) int {
	total := 0
	for _, t := range tools {
		total += len(t.Name) + len(t.Description)
		if t.InputSchema != nil {
			if b, err := json.Marshal(t.InputSchema); err == nil {
				total += len(b)
			}
			// 序列化失败时静默跳过 schema 部分，宁可低估也不中断估算流程
		}
	}
	return total / charsPerToken
}

// FormatTokenCount 将 token 数格式化为人类可读字符串。
// 示例：45200 → "45.2K"，1200000 → "1.2M"，500 → "500"。
func FormatTokenCount(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fK", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}
