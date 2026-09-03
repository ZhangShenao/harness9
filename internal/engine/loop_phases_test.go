// loop_phases_test.go 覆盖 runLoop 阶段化拆分后暴露出的可独立测试路径：
// 指数退避计算、provider 空响应防护、压缩视图与完整历史的隔离、Observation 注入。
package engine

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/harness9/internal/memory"
	"github.com/harness9/internal/planning"
	"github.com/harness9/internal/schema"
)

// noopEmitter 返回全回调为空实现的 emitter，供直接调用阶段方法的测试使用
// （runLoop 真实路径中 emitter 各回调恒非 nil，此处仅为满足调用前提）。
func noopEmitter() emitter {
	return emitter{
		generate: func(context.Context, int, []schema.Message, []schema.ToolDefinition) (*schema.Message, *schema.Usage, error) {
			return nil, nil, nil
		},
		toolStart:   func(int, schema.ToolCall) {},
		toolDone:    func(int, schema.ToolCall, schema.ToolResult, time.Duration) {},
		tokenUpdate: func(int, int) {},
		compaction:  func(memory.CompactionRecord) {},
	}
}

// TestBackoffDelay 验证指数退避计算的封顶与移位溢出防护。
// 回归背景：旧实现 `base << (attempt-1)` 未封顶移位数，attempt ≥ 65 时
// Go 移位结果为 0，退避塌缩为 0、退化为无间隔高频重试。
func TestBackoffDelay(t *testing.T) {
	tests := []struct {
		name     string
		base     time.Duration
		attempt  int
		maxDelay time.Duration
		want     time.Duration
	}{
		{"第 1 次失败按基准退避", 1 * time.Second, 1, 30 * time.Second, 1 * time.Second},
		{"第 2 次失败翻倍", 1 * time.Second, 2, 30 * time.Second, 2 * time.Second},
		{"第 4 次失败 8 倍", 1 * time.Second, 4, 30 * time.Second, 8 * time.Second},
		{"超过上限按上限封顶", 1 * time.Second, 10, 30 * time.Second, 30 * time.Second},
		{"移位溢出防护：超大 attempt 取上限而非 0", 1 * time.Second, 100, 30 * time.Second, 30 * time.Second},
		{"移位溢出防护： attempt 恰在位宽边界", 1 * time.Second, 64, 60 * time.Second, 60 * time.Second},
		{"负数 attempt 按第 1 次处理", 500 * time.Millisecond, -3, 30 * time.Second, 500 * time.Millisecond},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := backoffDelay(tt.base, tt.attempt, tt.maxDelay); got != tt.want {
				t.Errorf("backoffDelay(%v, %d, %v) = %v, want %v", tt.base, tt.attempt, tt.maxDelay, got, tt.want)
			}
		})
	}
}

// nilOnceProvider 首次调用返回 (nil, nil, nil)——违反 Provider 契约的空响应，
// 其后正常返回，用于验证 generateWithRetry 的空响应防护（应视为可重试错误）。
type nilOnceProvider struct {
	mu    sync.Mutex
	calls int
}

func (p *nilOnceProvider) Generate(_ context.Context, _ []schema.Message, _ []schema.ToolDefinition) (*schema.Message, *schema.Usage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if p.calls == 1 {
		return nil, nil, nil
	}
	return &schema.Message{Role: schema.RoleAssistant, Content: "ok"}, nil, nil
}

func (p *nilOnceProvider) GenerateStream(ctx context.Context, msgs []schema.Message, td []schema.ToolDefinition) (<-chan schema.StreamChunk, error) {
	msg, _, err := p.Generate(ctx, msgs, td)
	if err != nil {
		return nil, err
	}
	ch := make(chan schema.StreamChunk, 1)
	go func() {
		defer close(ch)
		ch <- schema.StreamChunk{Type: schema.StreamChunkDone, Message: msg}
	}()
	return ch, nil
}

// TestGenerateRetry_NilMessageTreatedAsRetryable 验证：Provider 返回空响应（nil message
// 且 nil error）不会 panic，而是作为可重试错误处理；重试成功后 Run 正常收敛。
// 防护缺失时 runLoop 对 nil 解引用会直接崩溃整个实例。
func TestGenerateRetry_NilMessageTreatedAsRetryable(t *testing.T) {
	p := &nilOnceProvider{}
	r := &staticRegistry{output: "ok"}
	eng := NewAgentEngine(p, r, "/test", WithGenerateRetry(3, time.Millisecond))

	if err := eng.Run(context.Background(), "task"); err != nil {
		t.Fatalf("空响应应触发重试并恢复，got: %v", err)
	}
	if p.calls != 2 {
		t.Errorf("应尝试 2 次（空响应 + 正常响应），实际 %d", p.calls)
	}
}

// TestPrepareTurnInput_CompactionDoesNotMutateHistory 验证压缩视图隔离不变量：
// prepareTurnInput 返回的 input.history 是压缩后的临时视图，引擎本地的
// lc.history 必须保持完整——这是后续轮次能看到全部上下文的前提。
func TestPrepareTurnInput_CompactionDoesNotMutateHistory(t *testing.T) {
	p := &countingProvider{
		responses: []func([]schema.ToolDefinition) *schema.Message{
			func(_ []schema.ToolDefinition) *schema.Message {
				return &schema.Message{Role: schema.RoleAssistant, Content: "done"}
			},
		},
	}
	reg := &staticRegistry{tools: []schema.ToolDefinition{{Name: "bash"}}, output: "ok"}
	eng := NewAgentEngine(p, reg, "/test",
		WithCompactor(&memory.SlidingWindowCompactor{MaxMessages: 3}),
	)

	// prepareTurnInput 会调用 emitter 回调（压缩通知 / token 上报），
	// 测试场景使用 no-op 实现。
	lc := eng.beginInteraction(context.Background(), "hello", "engine", noopEmitter())
	// 构造超过压缩阈值的历史（system + 5 条 + user = 7 条）。
	for i := 0; i < 5; i++ {
		lc.history = append(lc.history,
			schema.Message{Role: schema.RoleUser, Content: fmt.Sprintf("q%d", i)},
			schema.Message{Role: schema.RoleAssistant, Content: fmt.Sprintf("a%d", i)},
		)
	}
	before := append([]schema.Message(nil), lc.history...)

	input, _ := lc.prepareTurnInput()

	if len(input.history) > 3 {
		t.Errorf("发送视图应被压缩到 ≤3 条，实际 %d", len(input.history))
	}
	if len(lc.history) != len(before) {
		t.Errorf("完整历史不应被压缩修改：before=%d after=%d", len(before), len(lc.history))
	}
	for i := range before {
		if before[i].Content != lc.history[i].Content {
			t.Errorf("完整历史第 %d 条被修改: %q → %q", i, before[i].Content, lc.history[i].Content)
		}
	}
}

// TestInjectObservations 验证 Observation 注入的三条不变量：
// user 角色 + ToolCallID 关联、空输出兜底为占位文案、IsError 透传（驱动 Provider
// 设置 tool_result.is_error，强化自愈信号）。
func TestInjectObservations(t *testing.T) {
	calls := []schema.ToolCall{
		{ID: "c1", Name: "bash", Arguments: []byte(`{}`)},
		{ID: "c2", Name: "read_file", Arguments: []byte(`{}`)},
	}
	results := []schema.ToolResult{
		{ToolCallID: "c1", Output: "file list"},       // 正常输出
		{ToolCallID: "c2", Output: "", IsError: true}, // 空输出 + 错误标记
	}

	history := injectObservations([]schema.Message{{Role: schema.RoleSystem, Content: "sys"}}, calls, results)

	if len(history) != 3 {
		t.Fatalf("应注入 2 条 Observation，实际 %d 条", len(history)-1)
	}
	obs1 := history[1]
	if obs1.Role != schema.RoleUser || obs1.ToolCallID != "c1" {
		t.Errorf("Observation 应为 user 角色且携带 ToolCallID，got %+v", obs1)
	}
	if obs1.Content != "file list" {
		t.Errorf("正常输出应原样注入，got %q", obs1.Content)
	}
	obs2 := history[2]
	if obs2.Content != "[工具执行完成，无输出]" {
		t.Errorf("空输出应兜底为占位文案，got %q", obs2.Content)
	}
	if !obs2.IsError {
		t.Error("IsError 应透传到 Observation 消息")
	}
}

// TestBeginTurn_RejectsOverMaxTurns 验证 beginTurn 阶段的 MaxTurns 判定与
// interactionErr 记录（OnInteractionEnd 依赖该字段上报错误）。
func TestBeginTurn_RejectsOverMaxTurns(t *testing.T) {
	p := &countingProvider{}
	reg := &staticRegistry{output: "ok"}
	eng := NewAgentEngine(p, reg, "/test", WithMaxTurns(2))

	lc := eng.beginInteraction(context.Background(), "hello", "engine", emitter{})
	lc.turns = 2 // 模拟已完成 2 轮

	_, err := lc.beginTurn(context.Background())
	if err == nil {
		t.Fatal("超过 MaxTurns 应返回错误")
	}
	if lc.interactionErr == nil {
		t.Error("interactionErr 应被记录，供 OnInteractionEnd 上报")
	}
}

// TestApplyPlanModePrefix 验证 Plan Mode 前缀注入的开关行为：
// Plan 模式加前缀且保留原始 prompt，其余模式原样返回。
func TestApplyPlanModePrefix(t *testing.T) {
	const prompt = "implement feature X"

	if got := applyPlanModePrefix(planning.PlanModeDefault, prompt); got != prompt {
		t.Errorf("Default 模式不应注入前缀，got %q", got)
	}
	if got := applyPlanModePrefix(planning.PlanModeAutoEdit, prompt); got != prompt {
		t.Errorf("AutoEdit 模式不应注入前缀，got %q", got)
	}
	plan := applyPlanModePrefix(planning.PlanModePlan, prompt)
	if plan == prompt {
		t.Error("Plan 模式应注入规划前缀")
	}
	if len(plan) <= len(prompt) || plan[len(plan)-len(prompt):] != prompt {
		t.Error("原始 prompt 应保留在前缀之后")
	}
}
