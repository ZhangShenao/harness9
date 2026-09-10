// Package main — usage：SWE-bench runner 的实际 token 用量采集与落盘。
// countingProvider 以装饰器模式包装 LLMProvider，按实例累计真实账单口径的
// input/output token 与调用次数（含重试）；UsageRecord 落盘 usage.jsonl，
// 供效率分析（tokens/turns/时长）与 Planning 采用观察使用。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"

	"github.com/harness9/internal/provider"
	"github.com/harness9/internal/schema"
)

// countingProvider 包装内层 LLMProvider，按实例累计实际 token 用量与调用次数。
// 计数口径为真实账单：每次请求发起即计一次调用（含重试与建流失败）；
// token 仅在响应携带 usage 时累计，usage 缺失时静默跳过（不影响主流程）。
type countingProvider struct {
	inner provider.LLMProvider

	mu           sync.Mutex
	inputTokens  int64
	outputTokens int64
	llmCalls     int64
}

// newCountingProvider 构造用量计数装饰器。
func newCountingProvider(inner provider.LLMProvider) *countingProvider {
	return &countingProvider{inner: inner}
}

// snapshot 返回当前累计值的副本（结果落盘用）。
func (c *countingProvider) snapshot() (input, output, calls int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.inputTokens, c.outputTokens, c.llmCalls
}

// addTokens 在锁内累计 token。
func (c *countingProvider) addTokens(in, out int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inputTokens += in
	c.outputTokens += out
}

// bumpCalls 在锁内累计调用次数。
func (c *countingProvider) bumpCalls() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.llmCalls++
}

// Generate 透传阻塞式调用并累计 usage。
func (c *countingProvider) Generate(ctx context.Context, messages []schema.Message, availableTools []schema.ToolDefinition) (*schema.Message, *schema.Usage, error) {
	c.bumpCalls()
	msg, usage, err := c.inner.Generate(ctx, messages, availableTools)
	if usage != nil {
		c.addTokens(int64(usage.InputTokens), int64(usage.OutputTokens))
	}
	return msg, usage, err
}

// GenerateStream 透交流式调用，转发 chunk 的同时拦截 StreamChunkDone 的 usage。
// 转发 select 感知 ctx 取消：引擎停止消费（超时/中断）时不再阻塞；若内层流停滞且不关闭 channel，则依赖 provider 的 ctx 取消关流契约兜底。
func (c *countingProvider) GenerateStream(ctx context.Context, messages []schema.Message, availableTools []schema.ToolDefinition) (<-chan schema.StreamChunk, error) {
	c.bumpCalls()
	innerCh, err := c.inner.GenerateStream(ctx, messages, availableTools)
	if err != nil {
		return nil, err
	}
	out := make(chan schema.StreamChunk, 16)
	go func() {
		defer close(out)
		for chunk := range innerCh {
			if chunk.Type == schema.StreamChunkDone && chunk.Usage != nil {
				c.addTokens(int64(chunk.Usage.InputTokens), int64(chunk.Usage.OutputTokens))
			}
			select {
			case <-ctx.Done():
				return
			case out <- chunk:
			}
		}
	}()
	return out, nil
}

// UsageRecord 是 usage.jsonl 的一行：单实例的轮内观测指标，与 predictions.jsonl
// 逐条对应。JSON tag 为 snake_case 契约，compare.py 按字段名消费。
type UsageRecord struct {
	InstanceID          string  `json:"instance_id"`
	InputTokens         int64   `json:"input_tokens"`
	OutputTokens        int64   `json:"output_tokens"`
	LLMCalls            int64   `json:"llm_calls"`
	Turns               int     `json:"turns"`
	PlanWrites          int     `json:"plan_writes"`
	VerifyGate          bool    `json:"verify_gate"`
	RanTest             bool    `json:"ran_test"`
	FinalEditUnverified bool    `json:"final_edit_unverified"`
	DurationSec         float64 `json:"duration_sec"`
}

// appendUsage 将单条 UsageRecord 追加写入 usage.jsonl（立即落盘，
// 与 predictions.jsonl 同节奏——崩溃时最多丢当前实例，不丢已完成实例）。
func appendUsage(path string, r UsageRecord) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("打开 usage 文件失败：%w", err)
	}
	defer f.Close()
	data, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("序列化 usage 记录失败：%w", err)
	}
	if _, err := fmt.Fprintf(f, "%s\n", data); err != nil {
		return fmt.Errorf("写入 usage 记录失败：%w", err)
	}
	return nil
}
