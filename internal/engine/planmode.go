// Package engine — Plan Mode 与 nudge 相关的工具集判定逻辑。
//
// Plan Mode 的核心原则：工具层硬约束优先于 prompt 层软约束。
// 写操作工具直接从传给 LLM 的工具列表中移除（filterReadOnlyTools），
// 确保模型无论处于何种上下文状态都无法调用它们；prompt 前缀只补充
// 无法在工具层表达的行为规则（如 bash 只读命令约束）。
package engine

import (
	"github.com/harness9/internal/planning"
	"github.com/harness9/internal/schema"
)

// planModePromptPrefix 是 Plan Mode 下注入到用户 prompt 前的规划指令前缀。
// 要求计划条目一一对应可执行动作，避免产出"需求澄清/方案设计"等无法落地的空计划。
const planModePromptPrefix = "分析以下请求，用 todo_write 输出一份可直接执行的实现计划，然后用纯文字简述计划后停止。\n" +
	"todo 项要求：每条对应一个具体的实现动作（例如：创建某文件、实现某函数、运行某命令），\n" +
	"而非高层规划描述（禁止写\"需求澄清\"、\"方案设计\"之类无法直接执行的条目）。\n" +
	"如需了解当前代码库，可使用 read_file 或 bash（只读命令：ls、cat、find、grep）。\n" +
	"不要创建文件、执行 build/install 或做任何实际修改。\n\n"

// planModeWhitelist 是 Plan Mode 下允许 LLM 调用的工具名称白名单（read-only map，安全并发读）。
//
// 白名单的设计意图：
//   - read_file / bash：允许探索代码库，但 prompt 层约束 bash 只使用只读命令（ls/cat/find/grep）
//   - todo_write：Plan Mode 的核心产出——LLM 通过此工具输出结构化实现计划
//   - use_skill：允许加载 Skills 获取项目规范文档
//   - write_file / edit_file：不在白名单，从工具列表中硬性移除（工具层硬约束，优于 prompt 层软约束）
//   - web_search / web_fetch：不在白名单；Plan Mode 专注于本地代码库探索，
//     网络访问不属于规划阶段所需能力，如需联网研究请使用 Default/AutoEdit 模式
//   - memory_search / memory_write：不在白名单；Plan Mode 是轻量规划阶段，不触发记忆读写，
//     避免规划过程产生副作用（如写入错误的记忆条目）；记忆操作在 Default/AutoEdit 模式下可用
//   - task：不在白名单；Plan Mode 仅生成计划，不触发子代理委派（委派是执行阶段的动作）
var planModeWhitelist = map[string]bool{
	"read_file":  true,
	"bash":       true,
	"use_skill":  true,
	"todo_write": true,
}

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

// filterReadOnlyTools 从工具定义列表中过滤出 planModeWhitelist 中的子集，
// 在 Plan Mode 下替代完整工具列表传递给 LLM。
// 使用工具层硬约束而非 prompt 层软约束，确保 LLM 无论在何种上下文状态下都无法访问被过滤的工具。
func filterReadOnlyTools(tools []schema.ToolDefinition) []schema.ToolDefinition {
	// 预分配容量为全量列表长度：白名单场景下通常保留大多数工具，
	// 一次分配优于多次 append 扩容。
	result := make([]schema.ToolDefinition, 0, len(tools))
	for _, t := range tools {
		if planModeWhitelist[t.Name] {
			result = append(result, t)
		}
	}
	return result
}

// applyPlanModePrefix 在 Plan Mode 下为用户 prompt 注入规划指令前缀。
func applyPlanModePrefix(mode planning.PlanMode, userPrompt string) string {
	if mode != planning.PlanModePlan {
		return userPrompt
	}
	return planModePromptPrefix + userPrompt
}
