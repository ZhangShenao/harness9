// Package engine — 手动上下文压缩支持。
//
// Compact 方法允许调用方（如 TUI /compact 命令）在不触发 LLM 推理循环的情况下，
// 对当前 session 的历史消息执行一次强制压缩，跳过常规的 80% 阈值检查。
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

// Compact 对当前 session 的历史消息执行一次强制压缩，跳过 80% 阈值检查。
//
// 行为说明：
//   - compactor 为 nil 时：立即返回零值 CompactionRecord 和 nil error（no-op）
//   - session 为 nil 时：立即返回零值 CompactionRecord 和 nil error（no-op）
//   - 否则：读取完整历史 → 注入 system prompt（与 runLoop 路径一致）→ 压缩 →
//     剥离 system prompt → 写回 session
//
// 压缩策略选择（按优先级）：
//   - RecordedCompactor：返回完整审计记录（ProgressiveCompactor 走此路径）
//   - ForceCompactor：跳过阈值检查的强制压缩
//   - 普通 Compactor：标准压缩
//
// 注意：system prompt 不持久化到 DB（与 loadHistoryWith 一致），Compact 在调用
// compactor 前注入 system prompt，写回时再剥离，保证 compactor 的 msgs[0].Role==System 前提。
//
// 返回的 CompactionRecord 包含压缩前后的 token 数、消息条数、tier 等审计信息，
// 供 TUI 在对话流中展示压缩通知消息。当 compactor 未实现 RecordedCompactor 时，
// 返回的 record 仅含零值字段（Tier=TierNone）。
//
// 线程安全：通过读锁快照 session 和 compactor，与 TUI goroutine 并发安全。
func (e *AgentEngine) Compact(ctx context.Context) (memory.CompactionRecord, error) {
	e.mu.RLock()
	sess := e.session
	comp := e.compactor
	e.mu.RUnlock()

	if comp == nil || sess == nil {
		return memory.CompactionRecord{}, nil
	}

	msgs, err := sess.GetMessages(ctx, 0)
	if err != nil {
		return memory.CompactionRecord{}, fmt.Errorf("compact: load history: %w", err)
	}
	if len(msgs) == 0 {
		return memory.CompactionRecord{}, nil
	}

	withSystem := make([]schema.Message, 0, len(msgs)+1)
	withSystem = append(withSystem, schema.Message{Role: schema.RoleSystem, Content: e.buildSystemPrompt()})
	withSystem = append(withSystem, msgs...)

	var compactedWithSystem []schema.Message
	var record memory.CompactionRecord
	if rc, ok := comp.(memory.RecordedCompactor); ok {
		compactedWithSystem, record = rc.CompactWithRecord(withSystem)
	} else if fc, ok := comp.(memory.ForceCompactor); ok {
		compactedWithSystem = fc.CompactForce(withSystem)
	} else {
		compactedWithSystem = comp.Compact(withSystem)
	}

	compacted := compactedWithSystem
	if len(compacted) > 0 && compacted[0].Role == schema.RoleSystem {
		compacted = compacted[1:]
	}

	if err := sess.Clear(ctx); err != nil {
		return record, fmt.Errorf("compact: clear session: %w", err)
	}
	if err := sess.AddMessages(ctx, compacted); err != nil {
		// 写回失败时尽力回滚原始历史：Clear 已执行、compacted 未落盘，
		// 若不恢复原始消息，一次瞬时 DB 错误就会导致整条会话历史永久丢失。
		// 回滚使用独立 ctx：写回失败若恰恰源于当前 ctx 取消/超时，复用原 ctx
		// 的回滚必然同样失败；给 5s 独立窗口，让瞬时 DB 故障下的恢复成为可能。
		// 回滚本身也可能失败（如 DB 持续不可用），此时只能记录告警，无法挽留数据。
		restoreCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if restoreErr := sess.AddMessages(restoreCtx, msgs); restoreErr != nil {
			log.Print(logfmt.FormatMsg("engine", fmt.Sprintf(
				"compact: 写回压缩历史失败且回滚也失败（会话数据可能丢失）: 写回=%v 回滚=%v", err, restoreErr)))
		}
		cancel()
		return record, fmt.Errorf("compact: write compacted messages: %w", err)
	}

	return record, nil
}
