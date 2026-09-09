package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/harness9/internal/schema"
)

// fakeProvider 是 countingProvider 的最小桩：可注入 usage、chunk 序列与错误。
type fakeProvider struct {
	genUsage  *schema.Usage
	genErr    error
	chunks    []schema.StreamChunk
	streamErr error

	mu          sync.Mutex
	genCalls    int
	streamCalls int
}

func (f *fakeProvider) Generate(ctx context.Context, messages []schema.Message, availableTools []schema.ToolDefinition) (*schema.Message, *schema.Usage, error) {
	f.mu.Lock()
	f.genCalls++
	f.mu.Unlock()
	return &schema.Message{Role: schema.RoleAssistant, Content: "ok"}, f.genUsage, f.genErr
}

func (f *fakeProvider) GenerateStream(ctx context.Context, messages []schema.Message, availableTools []schema.ToolDefinition) (<-chan schema.StreamChunk, error) {
	f.mu.Lock()
	f.streamCalls++
	f.mu.Unlock()
	if f.streamErr != nil {
		return nil, f.streamErr
	}
	ch := make(chan schema.StreamChunk, len(f.chunks))
	for _, c := range f.chunks {
		ch <- c
	}
	close(ch)
	return ch, nil
}

func drain(ch <-chan schema.StreamChunk) {
	for range ch {
	}
}

// TestCountingProviderGenerate 验证阻塞式调用的 token 累计与调用计数。
func TestCountingProviderGenerate(t *testing.T) {
	inner := &fakeProvider{genUsage: &schema.Usage{InputTokens: 100, OutputTokens: 20}}
	cp := newCountingProvider(inner)
	_, _, err := cp.Generate(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("Generate error: %v", err)
	}
	in, out, calls := cp.snapshot()
	if in != 100 || out != 20 || calls != 1 {
		t.Fatalf("want (100, 20, 1), got (%d, %d, %d)", in, out, calls)
	}
}

// TestCountingProviderNilUsage 验证 usage 缺失时只计调用数不计 token（账单口径容错）。
func TestCountingProviderNilUsage(t *testing.T) {
	cp := newCountingProvider(&fakeProvider{})
	_, _, _ = cp.Generate(context.Background(), nil, nil)
	in, out, calls := cp.snapshot()
	if in != 0 || out != 0 || calls != 1 {
		t.Fatalf("want (0, 0, 1), got (%d, %d, %d)", in, out, calls)
	}
}

// TestCountingProviderStream 验证流式调用拦截 StreamChunkDone 的 usage。
func TestCountingProviderStream(t *testing.T) {
	inner := &fakeProvider{chunks: []schema.StreamChunk{
		{Type: schema.StreamChunkTextDelta, Delta: "he"},
		{Type: schema.StreamChunkDone, Usage: &schema.Usage{InputTokens: 55, OutputTokens: 7}},
	}}
	cp := newCountingProvider(inner)
	ch, err := cp.GenerateStream(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("GenerateStream error: %v", err)
	}
	drain(ch)
	in, out, calls := cp.snapshot()
	if in != 55 || out != 7 || calls != 1 {
		t.Fatalf("want (55, 7, 1), got (%d, %d, %d)", in, out, calls)
	}
}

// TestCountingProviderStreamWithoutUsage 验证流式无 usage 时仅计调用。
func TestCountingProviderStreamWithoutUsage(t *testing.T) {
	inner := &fakeProvider{chunks: []schema.StreamChunk{
		{Type: schema.StreamChunkDone, Message: &schema.Message{Role: schema.RoleAssistant}},
	}}
	cp := newCountingProvider(inner)
	ch, err := cp.GenerateStream(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("GenerateStream error: %v", err)
	}
	drain(ch)
	in, out, calls := cp.snapshot()
	if in != 0 || out != 0 || calls != 1 {
		t.Fatalf("want (0, 0, 1), got (%d, %d, %d)", in, out, calls)
	}
}

// TestCountingProviderStreamError 验证建流失败也计入调用次数（请求发起口径）。
func TestCountingProviderStreamError(t *testing.T) {
	cp := newCountingProvider(&fakeProvider{streamErr: context.Canceled})
	_, err := cp.GenerateStream(context.Background(), nil, nil)
	if err == nil {
		t.Fatal("want error, got nil")
	}
	_, _, calls := cp.snapshot()
	if calls != 1 {
		t.Fatalf("want 1 call, got %d", calls)
	}
}

// TestCountingProviderConcurrent 验证并发调用的计数线程安全。
func TestCountingProviderConcurrent(t *testing.T) {
	inner := &fakeProvider{genUsage: &schema.Usage{InputTokens: 10, OutputTokens: 1}}
	cp := newCountingProvider(inner)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _ = cp.Generate(context.Background(), nil, nil)
		}()
	}
	wg.Wait()
	in, out, calls := cp.snapshot()
	if in != 200 || out != 20 || calls != 20 {
		t.Fatalf("want (200, 20, 20), got (%d, %d, %d)", in, out, calls)
	}
}

// TestAppendUsage 验证 usage.jsonl 的追加写与 JSON 字段完整性：两次写入后应有两行，
// 字段名与 spec §5.2 的 snake_case 契约一致。
func TestAppendUsage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	rec := UsageRecord{
		InstanceID:   "django__django-12908",
		InputTokens:  123456,
		OutputTokens: 7890,
		LLMCalls:     23,
		Turns:        12,
		PlanWrites:   1,
		VerifyGate:   false,
		RanTest:      true,
		DurationSec:  487,
	}
	if err := appendUsage(path, rec); err != nil {
		t.Fatalf("appendUsage error: %v", err)
	}
	if err := appendUsage(path, UsageRecord{InstanceID: "flask__flask-4992", Turns: 3, DurationSec: 61.4}); err != nil {
		t.Fatalf("appendUsage 第二次写入 error: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 usage.jsonl 失败: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d", len(lines))
	}
	var got UsageRecord
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatalf("第一行非法 JSON: %v", err)
	}
	if got != rec {
		t.Fatalf("roundtrip 不一致:\n got %+v\nwant %+v", got, rec)
	}
	// 字段名契约：锁定 snake_case JSON tag（compare.py 依赖）
	for _, key := range []string{"instance_id", "input_tokens", "output_tokens", "llm_calls", "turns", "plan_writes", "verify_gate", "ran_test", "duration_sec"} {
		if !strings.Contains(lines[0], `"`+key+`"`) {
			t.Errorf("usage.jsonl 缺少字段 %s", key)
		}
	}
}
