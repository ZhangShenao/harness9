// Package engine — 会话历史的加载、持久化与压缩适配。
//
// 该文件集中了 runLoop 与 Session（持久化层）、Compactor（压缩层）之间的全部交互，
// 使主循环本身不感知存储细节。核心约定：
//   - system prompt 不持久化到 DB，每次 Run 时重新注入；
//   - 压缩采用"写回式"：Recorded 压缩器真正生效时，压缩产物写回 lc.history 并
//     持久化到 Session，一次压缩持续生效多轮（详见 prepareTurnInput 与
//     writeBackCompaction）；非 Recorded 压缩器与降级截断仍为纯视图语义。
package engine

import (
	"context"
	"fmt"
	"log"
	"time"

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

// writeBackCompaction 将自动压缩产物写回引擎本地历史与 Session（写回式压缩）。
//
// 背景（issue #117）：压缩曾以"逐轮视图"方式工作——压缩结果仅作当次 LLM 输入，
// lc.history 从不改写，占比过阈后每轮都重跑压缩：Soft/Full 档每轮多一次 LLM 摘要
// 调用，且"增量"模板每轮重喂全部 head 原文。写回式压缩在压缩真正生效时把产物
// 写回 lc.history 并持久化到 Session，一次压缩持续生效多轮，各 tier 也能按预期
// 递进触发；同时压缩产物与原始历史不再共享底层数组，别名改写风险随之消除。
//
// 写回时机与门控：
//   - 仅 Recorded 压缩器走写回（生产默认 ProgressiveCompactor）；非 Recorded
//     压缩器保持纯视图语义；
//   - TierEmergency 写回：真性容量溢出下全量历史已无法通过 API 发送，写回截断
//     视图是唯一恢复路径——下一轮占比收敛，Soft/Full 摘要可重新工作（若不写回，
//     每轮都会重新 Emergency，实测会陷入 200 轮级别的截断循环）；
//   - 其余降级压缩（LLM 摘要失败的回退截断，Soft/Full + Error）不写回——瞬时
//     故障，下一轮可重试真正的摘要压缩，避免把有损截断固化成永久截断；
//   - 无实际削减（消息数与 token 均未减少且无 offload）不写回，避免无意义的
//     Session 重写。
//
// Session 侧与手动 /compact（compact.go）一致：Clear + AddMessages，写回失败时
// 用独立 ctx 尽力回滚原始历史；差异在于失败后的引擎侧处理——手动路径直接报错
// 返回，这里保留原始 lc.history（本轮仍以压缩视图作为 LLM 输入），下一轮重新判定。
func (lc *loopContext) writeBackCompaction(compacted []schema.Message, record memory.CompactionRecord) {
	// Emergency（真性容量溢出）写回；其余带 Error 的降级压缩（LLM 摘要失败的
	// 瞬时回退）不写回，下一轮重试真正的摘要。
	if record.Error != "" && record.Tier != memory.TierEmergency {
		return
	}
	if len(compacted) == 0 {
		return
	}
	// 有效性：token 收缩（Emergency 裁巨型消息 / offload）或消息数减少（Soft/Full
	// 摘要）任一成立即视为真正削减。
	effective := record.MsgsAfter < record.MsgsBefore ||
		record.TokensAfter < record.TokensBefore ||
		len(record.Offloaded) > 0
	if !effective {
		return
	}

	orig := lc.history
	if lc.sess != nil {
		// 回滚负载取持久化边界内的原始历史（orig[:startLen]）：写回失败时 Session
		// 必须精确还原到写回前的状态，不能把尚未持久化的新消息（本次 Run 的
		// prompt 等）顺手落盘。
		persistedPrefix := orig
		if lc.startLen < len(persistedPrefix) {
			persistedPrefix = persistedPrefix[:lc.startLen]
		}
		if err := lc.persistCompacted(compacted, persistedPrefix); err != nil {
			log.Print(logfmt.FormatMsg(lc.logPrefix, fmt.Sprintf(
				"压缩写回失败，保留原始历史（本轮仍使用压缩视图）: %v", err)))
			return
		}
	}
	lc.history = compacted
	// 写回点即持久化边界：压缩产物已整体落盘，之后新增的消息才由 saveHistory 追加。
	lc.startLen = len(compacted)
}

// persistCompacted 将压缩产物（剥离 system prompt）整体替换 Session 历史，
// 失败时尽力回滚 restoreSeed（写回前的已持久化前缀）。使用 obsCtx：与 savePlan
// 同源，携带 interaction Span。
func (lc *loopContext) persistCompacted(compacted, restoreSeed []schema.Message) error {
	compactedNoSys := stripSystemMessage(compacted)
	restoreNoSys := stripSystemMessage(restoreSeed)

	if err := lc.sess.Clear(lc.obsCtx); err != nil {
		return fmt.Errorf("clear session: %w", err)
	}
	if err := lc.sess.AddMessages(lc.obsCtx, compactedNoSys); err != nil {
		// 写回失败时尽力回滚原始历史：Clear 已执行、compacted 未落盘，若不恢复
		// 原始消息，一次瞬时 DB 错误就会导致整条会话历史永久丢失。回滚使用独立
		// ctx：写回失败若恰恰源于当前 ctx 取消/超时，复用原 ctx 的回滚必然同样
		// 失败；给 5s 独立窗口，让瞬时 DB 故障下的恢复成为可能。
		restoreCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if restoreErr := lc.sess.AddMessages(restoreCtx, restoreNoSys); restoreErr != nil {
			log.Print(logfmt.FormatMsg(lc.logPrefix, fmt.Sprintf(
				"压缩写回失败且回滚也失败（会话数据可能丢失）: 写回=%v 回滚=%v", err, restoreErr)))
		}
		cancel()
		return fmt.Errorf("write compacted messages: %w", err)
	}
	return nil
}

// stripSystemMessage 剥离消息列表首部的 system prompt（system 不持久化到 DB，
// 与 loadHistoryWith / compact.go 的约定一致）。
func stripSystemMessage(msgs []schema.Message) []schema.Message {
	if len(msgs) > 0 && msgs[0].Role == schema.RoleSystem {
		return msgs[1:]
	}
	return msgs
}
