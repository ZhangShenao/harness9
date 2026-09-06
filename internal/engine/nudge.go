// Package engine — nudge 注入与进展工具集判定。
//
// 本文件承载两类"仅作用于发送副本"的辅助逻辑（原 planmode.go 迁移而来，
// Plan Mode 已移除，规划成为 Agent 原生能力）：
//   - appendUserNudge：向历史副本末尾追加 user 消息（记忆/停滞 nudge 与 Plan 注入共用）
//   - progressToolNames/hasProgressTool：停滞检测的"实质进展"工具集判定
package engine

import (
	"github.com/harness9/internal/schema"
)

// progressToolNames 是被视为"取得实质进展"的工具集合（用于 WithStallNudge 停滞检测）。
// 调用其一即重置停滞计数：它们改变工作区状态（写入/编辑文件），是 Agent 真正推进任务的信号。
// 只读探索（read_file/bash grep 等）不计入进展，连续多轮只读即被判定为停滞。
var progressToolNames = map[string]bool{
	"write_file": true,
	"edit_file":  true,
}

// hasProgressTool 判断本轮工具调用中是否包含进展工具。
func hasProgressTool(calls []schema.ToolCall) bool {
	for _, tc := range calls {
		if progressToolNames[tc.Name] {
			return true
		}
	}
	return false
}

// appendUserNudge 返回在历史副本末尾追加一条 user nudge 消息的新切片。
// 不修改入参、不持久化——nudge 仅对当轮发送给 LLM 的临时副本可见。
func appendUserNudge(history []schema.Message, text string) []schema.Message {
	withNudge := make([]schema.Message, len(history), len(history)+1)
	copy(withNudge, history)
	return append(withNudge, schema.Message{Role: schema.RoleUser, Content: text})
}
