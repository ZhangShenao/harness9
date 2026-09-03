// Package engine — 同一 Turn 内多个工具调用的并发调度。
package engine

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/harness9/internal/hooks"
	"github.com/harness9/internal/logfmt"
	"github.com/harness9/internal/schema"
)

// executeTools 并发执行所有工具调用，每个工具带有独立的超时控制。
//
// 并发模型（三重保障，见 AGENTS.md §3.4）：
//   - 预分配 results 切片 + 按 idx 写入，保证结果顺序与 toolCalls 一致；
//   - sync.WaitGroup 等待全部 goroutine 退出；
//   - maxConcurrentTools > 0 时以带缓冲 channel 作信号量限制并发上限。
//
// 每个工具从 ctx 派生独立子 context（WithTimeout）：单个工具超时不影响
// 同 Turn 内其他工具的执行；超时结果由 Registry 层转换为 IsError=true，
// LLM 可据此观察并重试（自愈）。
// toolStart / toolDone 回调在 per-tool goroutine 中并发调用，emitter 实现方需自行保证安全。
func (e *AgentEngine) executeTools(ctx context.Context, turn int, toolCalls []schema.ToolCall, logPrefix string, em emitter) []schema.ToolResult {
	log.Print(logfmt.FormatParallelTools(logPrefix, turn, len(toolCalls), e.maxConcurrentTools))

	results := make([]schema.ToolResult, len(toolCalls))
	var wg sync.WaitGroup

	// 信号量：仅在配置了并发上限时创建，nil 表示不限制。
	var sem chan struct{}
	if e.maxConcurrentTools > 0 {
		sem = make(chan struct{}, e.maxConcurrentTools)
	}

	for i, toolCall := range toolCalls {
		wg.Add(1)
		go func(idx int, tc schema.ToolCall) {
			defer wg.Done()

			if sem != nil {
				sem <- struct{}{}
				defer func() { <-sem }()
			}

			// 每工具独立超时：toolTimeout<=0 表示沿用 ctx 原始截止时间（不额外限制）。
			toolCtx := ctx
			var cancel context.CancelFunc
			if e.toolTimeout > 0 {
				toolCtx, cancel = context.WithTimeout(ctx, e.toolTimeout)
				defer cancel()
			}

			// 审批回调注入 context：RunStream 模式下由事件循环驱动人类审批；
			// 使用会话级 ctx 而非 toolCtx，确保人类决策时间不计入工具超时。
			if em.approval != nil {
				toolCtx = hooks.WithApprovalFn(toolCtx, em.approval)
			}

			em.toolStart(turn, tc)

			start := time.Now()
			results[idx] = e.registry.Execute(toolCtx, tc)
			em.toolDone(turn, tc, results[idx], time.Since(start))
		}(i, toolCall)
	}

	wg.Wait()
	return results
}
