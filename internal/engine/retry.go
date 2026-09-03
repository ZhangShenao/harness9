// Package engine — LLM 生成调用的应用层重试策略。
//
// 设计动机：SDK 内置重试只覆盖"首字节到达前"的连接错误，流式响应中途断连
// 会以 StreamChunkError 形式逃逸到引擎层；一次瞬时抖动不应杀掉整条 agent 轨迹。
// 重试分两档独立预算：默认预算（业务层瞬时错误）与网络预算（TLS/DNS/连接建立），
// 后者更宽松，两者互不借用。
package engine

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/harness9/internal/logfmt"
	"github.com/harness9/internal/schema"
)

// maxGenerateRetryDelay 是生成重试指数退避的上限，避免退避时间吞掉整体预算。
const maxGenerateRetryDelay = 30 * time.Second

// maxNetworkRetryDelay 是网络传输层错误重试指数退避的上限，比 maxGenerateRetryDelay
// 更宽松——这类故障（TLS/DNS/连接建立）间歇性更强，值得多等一会儿而不是放弃整个 turn。
const maxNetworkRetryDelay = 60 * time.Second

// retryBudget 聚合一档重试预算的三要素：最大尝试次数、指数退避基准、退避上限。
// 默认预算与网络预算各持有一份，互不共享。
type retryBudget struct {
	maxAttempts int
	baseDelay   time.Duration
	capDelay    time.Duration
}

// generateWithRetry 在 em.generate 之上叠加有界指数退避重试。
//
// 重试策略：
//   - 成功立即返回；
//   - context 已取消/超时（ctx.Err()!=nil）不重试，原样返回（会话级终止信号）；
//   - 其余错误视为可能瞬时，按错误类别选用预算（网络错误用独立宽松预算），
//     退避后重试，直到耗尽 attempts；
//   - 退避期间感知 ctx 取消，避免无谓等待。
//
// logPrefix 用于重试日志前缀（阻塞模式 "engine"，流式模式 "engine-stream"），
// 与 runLoop 的其余日志保持同一前缀约定。
func (e *AgentEngine) generateWithRetry(ctx context.Context, em emitter, turn int, logPrefix string, history []schema.Message, toolDefs []schema.ToolDefinition) (*schema.Message, *schema.Usage, error) {
	defaultBudget := retryBudget{
		maxAttempts: max(e.generateRetries, 1),
		baseDelay:   orDefault(e.generateRetryBase, time.Second),
		capDelay:    maxGenerateRetryDelay,
	}
	networkBudget := retryBudget{
		maxAttempts: max(e.networkRetries, 1),
		baseDelay:   orDefault(e.networkRetryBase, 5*time.Second),
		capDelay:    maxNetworkRetryDelay,
	}

	var lastErr error
	for attempt := 1; ; attempt++ {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		msg, usage, err := em.generate(ctx, turn, history, toolDefs)
		if err == nil && msg == nil {
			// 防御：Provider 契约要求 err==nil 时 msg 非 nil。空响应若直接放行，
			// runLoop 对其解引用会 panic 导致整个实例崩溃；视为可重试的异常返回。
			err = fmt.Errorf("provider 返回了空响应（message 为 nil）")
		}
		if err == nil {
			return msg, usage, nil
		}
		lastErr = err
		// context 取消/超时不重试。
		if ctx.Err() != nil {
			return nil, nil, err
		}

		// 网络传输层错误（TLS/DNS/连接建立）间歇性更强，用独立、更宽松的预算，
		// 不与其他错误类别共享 attempts/base——分类基于本次失败的错误本身，
		// 不影响非网络错误仍然只用默认预算。
		budget := defaultBudget
		if isTransientNetworkError(err) {
			budget = networkBudget
		}
		if attempt >= budget.maxAttempts {
			break
		}

		delay := backoffDelay(budget.baseDelay, attempt, budget.capDelay)
		log.Print(logfmt.FormatMsg(logPrefix, fmt.Sprintf(
			"LLM 生成失败 (turn %d, 尝试 %d/%d)，%s 后重试: %v", turn, attempt, budget.maxAttempts, delay, err)))
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(delay):
		}
	}
	return nil, nil, lastErr
}

// backoffDelay 计算第 attempt 次失败后的指数退避时长：base << (attempt-1)，以 maxDelay 封顶。
//
// 必须封顶移位数：Go 中移位数 ≥ 类型位宽时结果为 0，若不处理，超大 attempt 会使
// 退避塌缩为 0，把有界指数退避退化为无间隔高频重试（撞击上游限流）。
// 封顶为 maxDelay 与原语义一致——正常路径下延迟总会先于移位溢出触及上限。
func backoffDelay(base time.Duration, attempt int, maxDelay time.Duration) time.Duration {
	shift := attempt - 1
	if shift < 0 {
		shift = 0
	}
	// 2^32 倍已远超任何合理上限（1ns 基准下也是 ~4398s），直接取上限即可。
	if shift > 32 {
		return maxDelay
	}
	if d := base << uint(shift); d > maxDelay || d <= 0 {
		return maxDelay
	} else {
		return d
	}
}

// orDefault 返回 d 本身（>0 时），否则回退到 fallback，避免调用方零值配置导致 0 延迟。
func orDefault(d, fallback time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return fallback
}

// isTransientNetworkError 判断 LLM 生成失败的根因是否为建立到 API 端点连接阶段的
// 瞬时网络故障（TLS/证书校验、DNS 解析、连接建立），而非业务层错误（如 4xx/5xx）。
// 这类故障间歇性更强，容错窗口需要比默认重试策略更宽——Terminal-Bench pilot 里
// 3 个任务在同一条 x509 证书错误上耗尽默认预算后直接放弃整个 turn，详见
// docs/技术调研/terminal-bench-轨迹分析-v1.md §2 R2。
//
// 采用字符串匹配而非 errors.As 类型断言：openai-go/anthropic-sdk-go 对底层
// net/http 错误的包装不保证保留可断言的具体类型，但错误消息里的关键字符串
// （x509:/tls:/no such host 等）是稳定、可观测的。
func isTransientNetworkError(err error) bool {
	msg := err.Error()
	for _, marker := range []string{
		"x509:", "tls:", "no such host", "connection refused",
		"connection reset", "i/o timeout", "dial tcp",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}
