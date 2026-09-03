// Package engine — 会话历史的加载、持久化与压缩适配。
//
// 该文件集中了 runLoop 与 Session（持久化层）、Compactor（压缩层）之间的全部交互，
// 使主循环本身不感知存储细节。核心约定：
//   - system prompt 不持久化到 DB，每次 Run 时重新注入；
//   - 压缩是"逐轮视图"而非"就地修改"——压缩结果仅作用于当次 LLM 调用的输入，
//     引擎本地的完整历史不受影响（详见 loop_phases.go 的 prepareTurnInput）。
package engine

import (
	"context"
	"fmt"
	"log"

	"github.com/harness9/internal/logfmt"
	"github.com/harness9/internal/memory"
	"github.com/harness9/internal/schema"
)

// buildSystemPrompt 返回 system prompt 字符串。
// 若设置了 PromptBuilder 则委托给它，否则回退到内置默认文案。
func (e *AgentEngine) buildSystemPrompt() string {
	if e.promptBuilder != nil {
		return e.promptBuilder.Build()
	}
	return fmt.Sprintf(`你的名字是 harness9。请始终以 "harness9" 自称 — 不要使用 "AI 助手"、"语言模型" 或任何其他通称。

harness9 是一个通用 AI Agent，可完全访问用户的计算机。

能力：
- 执行 Shell 命令：运行程序、管理进程、安装软件包、与操作系统交互
- 读取、写入和编辑文件系统中的文件
- 将多个工具串联使用，自主完成复杂的多步骤任务

工作目录：%s

工作准则：
- 先调查后行动：优先读取文件并运行诊断命令
- 小步可验证地推进：每次重要操作后检查结果
- 命令失败时，诊断根本原因而非猜测
- 优先局部修改而非整体重写；保持现有风格和约定
- 任务描述模糊时，选择最合理的解释后直接推进`, e.workDir)
}

// loadHistoryWith 从 sess 加载历史消息，注入 system prompt 和当前用户输入。
// sess 为 nil 时退化为原有行为（全新 contextHistory）。
// 返回完整历史切片和新消息的起始索引（用于 saveHistoryWith）。
func (e *AgentEngine) loadHistoryWith(ctx context.Context, userPrompt string, sess memory.Session, logPrefix string) ([]schema.Message, int) {
	var history []schema.Message
	if sess != nil {
		msgs, err := sess.GetMessages(ctx, 0)
		if err != nil {
			// 历史加载失败不终止 Run：降级为全新会话（自愈），仅记录告警。
			log.Print(logfmt.FormatMsg(logPrefix, fmt.Sprintf("加载会话历史失败: %v", err)))
		} else {
			history = msgs
		}
	}
	// system prompt 不持久化到 DB，每次调用时重新注入
	if len(history) == 0 || history[0].Role != schema.RoleSystem {
		history = append([]schema.Message{{Role: schema.RoleSystem, Content: e.buildSystemPrompt()}}, history...)
	}
	startLen := len(history) // 新消息从此处开始；system prompt 不计入持久化范围
	history = append(history, schema.Message{Role: schema.RoleUser, Content: userPrompt})
	return history, startLen
}

// saveHistoryWith 将本次 Run 新增的消息（msgs[startLen:]）写回 sess。
// sess 为 nil 时为 no-op；失败仅打 warning 日志，不中断主流程。
func (e *AgentEngine) saveHistoryWith(ctx context.Context, sess memory.Session, msgs []schema.Message, startLen int, logPrefix string) {
	if sess == nil || startLen >= len(msgs) {
		return
	}
	newMsgs := msgs[startLen:]
	if err := sess.AddMessages(ctx, newMsgs); err != nil {
		log.Print(logfmt.FormatMsg(logPrefix, fmt.Sprintf("保存会话历史失败: %v", err)))
	}
}

// applyCompactionWith 对消息列表应用压缩策略。comp 为 nil 时原样返回。
// 若 compactor 实现 RecordedCompactor 接口，同时返回压缩审计记录（含 tier、锚点、外存条目等）。
func (e *AgentEngine) applyCompactionWith(comp memory.Compactor, msgs []schema.Message) ([]schema.Message, *memory.CompactionRecord) {
	if comp == nil {
		return msgs, nil
	}
	if rc, ok := comp.(memory.RecordedCompactor); ok {
		result, record := rc.CompactWithRecord(msgs)
		return result, &record
	}
	return comp.Compact(msgs), nil
}
