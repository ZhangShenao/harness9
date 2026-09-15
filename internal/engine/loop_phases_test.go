// loop_phases_test.go 覆盖 runLoop 阶段化拆分后暴露出的可独立测试路径：
// 指数退避计算、provider 空响应防护、压缩视图与完整历史的隔离、Observation 注入。
package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
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
		{"移位溢出防护：attempt 恰在位宽边界", 1 * time.Second, 64, 60 * time.Second, 60 * time.Second},
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

	input := lc.prepareTurnInput()

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

// ctxMarkKey 是 ctx 传播回归测试的自定义 key（模拟 observer 注入的 Span 等值）。
type ctxMarkKey struct{}

// markingObserver 在 OnInteractionStart 向 ctx 注入标记，并检测 OnTurnStart
// 是否仍能读到。回归背景：runLoop 阶段化拆分时曾把原始 ctx 而非 obsCtx 传给
// beginTurn/saveTodos/saveHistory——OTELEngineObserver 的 interaction→turn
// Span 父子关系依赖 OnInteractionStart 注入 ctx 值，断链后所有 Span 退化为
// 根 Span，Langfuse trace 分组静默失效。
type markingObserver struct {
	noopObserver
	turnStartSawMark bool
}

func (m *markingObserver) OnInteractionStart(ctx context.Context, _, _ string) context.Context {
	return context.WithValue(ctx, ctxMarkKey{}, "marked")
}

func (m *markingObserver) OnTurnStart(ctx context.Context, _ int) context.Context {
	if _, ok := ctx.Value(ctxMarkKey{}).(string); ok {
		m.turnStartSawMark = true
	}
	return ctx
}

// TestRunLoop_ObserverCtxPropagation 验证 observer 不变量：OnInteractionStart
// 注入的 ctx 值必须在 OnTurnStart 阶段仍可读——runLoop 必须向所有阶段传递
// obsCtx 而非原始 ctx。
func TestRunLoop_ObserverCtxPropagation(t *testing.T) {
	p := &countingProvider{
		responses: []func([]schema.ToolDefinition) *schema.Message{
			func(_ []schema.ToolDefinition) *schema.Message {
				return &schema.Message{Role: schema.RoleAssistant, Content: "done"}
			},
		},
	}
	reg := &staticRegistry{output: "ok"}
	obs := &markingObserver{}
	eng := NewAgentEngine(p, reg, "/test", WithEngineObserver(obs))

	if err := eng.Run(context.Background(), "hello"); err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if !obs.turnStartSawMark {
		t.Error("OnTurnStart 应能读到 OnInteractionStart 注入的 ctx 值（obsCtx 必须贯穿所有阶段）")
	}
}

// TestGenerateRetry_UsesLogPrefix 验证重试日志使用调用方传入的 logPrefix
// （流式模式为 "engine-stream"），而非硬编码 "engine"。
func TestGenerateRetry_UsesLogPrefix(t *testing.T) {
	var buf bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) }()

	eng := NewAgentEngine(&countingProvider{}, &staticRegistry{output: "ok"}, "/test",
		WithGenerateRetry(2, time.Millisecond))
	calls := 0
	em := emitter{generate: func(context.Context, int, []schema.Message, []schema.ToolDefinition) (*schema.Message, *schema.Usage, error) {
		calls++
		if calls == 1 {
			return nil, nil, fmt.Errorf("transient failure")
		}
		return &schema.Message{Role: schema.RoleAssistant, Content: "ok"}, nil, nil
	}}

	msg, _, err := eng.generateWithRetry(context.Background(), em, 1, "engine-stream", nil, nil)
	if err != nil {
		t.Fatalf("generateWithRetry 应在重试后恢复: %v", err)
	}
	if msg == nil || msg.Content != "ok" {
		t.Errorf("应返回重试成功的响应，got %+v", msg)
	}
	if !strings.Contains(buf.String(), "engine-stream") {
		t.Errorf("重试日志应使用传入的 logPrefix %q，实际输出: %q", "engine-stream", buf.String())
	}
}

// TestPrepareTurnInput_InjectsActivePlan 验证 Plan 注入三要素（Spec §5.2）：
//  1. 活跃条目存在时，发送视图末尾追加 FormatPlan() 全文（原样，含标题行）
//  2. 注入只作用于当次发送副本，lc.history 不被污染（视图隔离）
//  3. 无活跃条目（全部 completed）或 nil store 时不注入
func TestPrepareTurnInput_InjectsActivePlan(t *testing.T) {
	newLC := func(store *planning.PlanStore) *loopContext {
		eng := &AgentEngine{registry: &staticRegistry{output: "ok"}}
		lc := &loopContext{engine: eng, planStore: store, em: noopEmitter(), history: []schema.Message{
			{Role: schema.RoleSystem, Content: "sys"},
			{Role: schema.RoleUser, Content: "user prompt"},
		}}
		return lc
	}

	// 场景 1：有活跃条目 → 注入（原样、含标题行、不含已完成条目）
	store := planning.NewPlanStore()
	store.Write([]planning.PlanItem{
		{ID: "1", Content: "创建 parser.go", Status: planning.PlanInProgress},
		{ID: "2", Content: "已完成步骤", Status: planning.PlanCompleted},
	})
	lc := newLC(store)
	in := lc.prepareTurnInput()
	last := in.history[len(in.history)-1]
	if !strings.Contains(last.Content, "当前执行计划") || !strings.Contains(last.Content, "创建 parser.go") {
		t.Errorf("active plan should be injected verbatim, got: %q", last.Content)
	}
	if strings.Contains(last.Content, "已完成步骤") {
		t.Error("completed items should not be injected")
	}
	// 视图隔离：lc.history 长度不变（仍为 system+user），注入消息不在其中
	if len(lc.history) != 2 {
		t.Errorf("lc.history must stay untouched, got %d msgs", len(lc.history))
	}
	if lc.history[len(lc.history)-1].Content != "user prompt" {
		t.Errorf("lc.history tail must stay the original user prompt, got: %q", lc.history[len(lc.history)-1].Content)
	}

	// 场景 2：无活跃条目 → 不注入
	empty := planning.NewPlanStore()
	empty.Write([]planning.PlanItem{{ID: "1", Content: "done", Status: planning.PlanCompleted}})
	lc2 := newLC(empty)
	in2 := lc2.prepareTurnInput()
	if last2 := in2.history[len(in2.history)-1]; strings.Contains(last2.Content, "当前执行计划") {
		t.Error("no injection expected when no active items")
	}

	// 场景 3：nil store → 不注入、不 panic
	lc3 := newLC(nil)
	in3 := lc3.prepareTurnInput()
	if last3 := in3.history[len(in3.history)-1]; strings.Contains(last3.Content, "当前执行计划") {
		t.Error("no injection expected for nil store")
	}
}

// failingAddSession 包装 MemorySession：首次 AddMessages 返回错误（模拟 DB 瞬时故障），
// 用于验证写回失败后的回滚路径（原始历史恢复落盘、引擎本地历史保持原样）。
type failingAddSession struct {
	memory.Session
	failFirstAdd bool
	adds         int
}

func (s *failingAddSession) AddMessages(ctx context.Context, msgs []schema.Message) error {
	s.adds++
	if s.failFirstAdd && s.adds == 1 {
		return errors.New("simulated db failure")
	}
	return s.Session.AddMessages(ctx, msgs)
}

// newWriteBackLC 构造带 n 条额外 user/assistant 消息的 loopContext：
// history = [system, user task, n×(assistant/user)]，startLen=2（task 及之后为本 Run 新增）。
func newWriteBackLC(comp memory.Compactor, sess memory.Session, n int) (*loopContext, []schema.Message) {
	eng := &AgentEngine{registry: noopRegistry{}}
	lc := &loopContext{engine: eng, comp: comp, sess: sess, em: noopEmitter()}
	lc.history = append(lc.history,
		schema.Message{Role: schema.RoleSystem, Content: "sys"},
		schema.Message{Role: schema.RoleUser, Content: "task"},
	)
	for i := 0; i < n; i++ {
		lc.history = append(lc.history,
			schema.Message{Role: schema.RoleAssistant, Content: fmt.Sprintf("a%d", i)},
			schema.Message{Role: schema.RoleUser, Content: fmt.Sprintf("q%d", i)},
		)
	}
	lc.startLen = 2
	orig := append([]schema.Message(nil), lc.history...)
	return lc, orig
}

// TestPrepareTurnInput_WriteBackReplacesHistory 验证写回式压缩（issue #117）：
// 压缩触发时压缩产物写回 lc.history（而非仅作当次视图），一次压缩持续生效——
// 下一轮基于已写回的历史重新判定，不再重复压缩。
func TestPrepareTurnInput_WriteBackReplacesHistory(t *testing.T) {
	lc, _ := newWriteBackLC(&fixedCompactor{keep: 2}, nil, 3) // 8 条消息

	in := lc.prepareTurnInput()

	// 写回：lc.history 被替换为压缩产物（system + 最近 2 条）。
	if len(lc.history) != 3 {
		t.Errorf("压缩产物应写回 lc.history（3 条），实际 %d 条", len(lc.history))
	}
	// 本轮 LLM 输入视图与写回后的历史一致。
	if len(in.history) != len(lc.history) {
		t.Errorf("发送视图应与写回后历史一致：%d vs %d", len(in.history), len(lc.history))
	}

	// 第二轮：历史已收缩，压缩器返回原样（MsgsAfter==MsgsBefore），不再写回/再缩减。
	before := append([]schema.Message(nil), lc.history...)
	lc.prepareTurnInput()
	if len(lc.history) != len(before) {
		t.Errorf("第二轮不应再次缩减历史：%d → %d", len(before), len(lc.history))
	}
	for i := range before {
		if before[i].Content != lc.history[i].Content {
			t.Errorf("第二轮历史第 %d 条被改动: %q → %q", i, before[i].Content, lc.history[i].Content)
		}
	}
}

// TestPrepareTurnInput_WriteBackPersistsToSession 验证写回的持久化语义：
// 压缩产物剥离 system 后整体替换 session 历史（Clear + AddMessages），
// startLen 前移使 saveHistory 只追加写回之后的新消息，不产生重复。
func TestPrepareTurnInput_WriteBackPersistsToSession(t *testing.T) {
	sess := memory.NewMemorySession("wb-persist")
	seed := []schema.Message{
		{Role: schema.RoleUser, Content: "old1"},
		{Role: schema.RoleAssistant, Content: "old2"},
		{Role: schema.RoleUser, Content: "old3"},
		{Role: schema.RoleAssistant, Content: "old4"},
	}
	if err := sess.AddMessages(context.Background(), seed); err != nil {
		t.Fatalf("AddMessages: %v", err)
	}

	comp := &fixedCompactor{keep: 2}
	lc, _ := newWriteBackLC(comp, sess, 0)
	lc.history = append([]schema.Message{{Role: schema.RoleSystem, Content: "sys"}}, seed...)
	lc.history = append(lc.history, schema.Message{Role: schema.RoleUser, Content: "task"})
	lc.startLen = len(seed) + 1

	lc.prepareTurnInput()

	got, err := sess.GetMessages(context.Background(), 0)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	// [sys, old1..old4, task] 6 条 → 保留 system + 最近 2 条 → session 存 2 条（old4, task）。
	if len(got) != 2 || got[0].Content != "old4" || got[1].Content != "task" {
		t.Fatalf("session 应为压缩产物（old4, task），实际 %d 条: %+v", len(got), got)
	}

	// 写回点之后的新消息追加持久化，无重复。
	lc.history = append(lc.history, schema.Message{Role: schema.RoleAssistant, Content: "resp"})
	lc.saveHistory(context.Background())
	got2, err := sess.GetMessages(context.Background(), 0)
	if err != nil {
		t.Fatalf("GetMessages after save: %v", err)
	}
	if len(got2) != 3 || got2[2].Content != "resp" {
		t.Errorf("session 应为压缩产物 + 新消息共 3 条，实际 %d 条", len(got2))
	}
}

// TestPrepareTurnInput_WriteBackRollbackOnSessionError 验证写回失败的回滚：
// AddMessages 失败时引擎本地历史保持原样（本轮仍以压缩视图作为 LLM 输入），
// 且原始历史已恢复落盘，不会因 Clear+写回失败丢数据。
func TestPrepareTurnInput_WriteBackRollbackOnSessionError(t *testing.T) {
	sess := &failingAddSession{
		Session:      memory.NewMemorySession("wb-rollback"),
		failFirstAdd: true,
	}
	seed := []schema.Message{
		{Role: schema.RoleUser, Content: "old1"},
		{Role: schema.RoleAssistant, Content: "old2"},
		{Role: schema.RoleUser, Content: "old3"},
		{Role: schema.RoleAssistant, Content: "old4"},
	}
	// 种子数据直接写入内部 session，让包装器的首次 AddMessages 正好是写回尝试。
	if err := sess.Session.AddMessages(context.Background(), seed); err != nil {
		t.Fatalf("seed AddMessages: %v", err)
	}

	lc, orig := newWriteBackLC(&fixedCompactor{keep: 2}, sess, 0)
	lc.history = append([]schema.Message{{Role: schema.RoleSystem, Content: "sys"}}, seed...)
	lc.history = append(lc.history, schema.Message{Role: schema.RoleUser, Content: "task"})
	lc.startLen = len(seed) + 1
	orig = append([]schema.Message(nil), lc.history...)

	lc.prepareTurnInput()

	// 引擎本地历史保持原样（写回失败不替换）。
	if len(lc.history) != len(orig) {
		t.Fatalf("写回失败后 lc.history 应保持原样（%d 条），实际 %d 条", len(orig), len(lc.history))
	}
	for i := range orig {
		if orig[i].Content != lc.history[i].Content {
			t.Errorf("写回失败后历史第 %d 条被改动", i)
		}
	}
	// 写回确实被尝试过：首次 AddMessages 失败 + 回滚成功，共 2 次调用。
	// （若压缩路径根本不写回，此断言失败——回滚逻辑就没有被真正执行到。）
	if sess.adds != 2 {
		t.Errorf("写回应尝试 1 次并回滚 1 次（共 2 次 AddMessages），实际 %d 次", sess.adds)
	}
	// session 内容恢复为原始历史（回滚 AddMessages 已执行）。
	got, err := sess.GetMessages(context.Background(), 0)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(got) != len(seed) {
		t.Errorf("session 应回滚为原始 %d 条，实际 %d 条", len(seed), len(got))
	}
}

// degradedRecorder 返回携带 Error 的降级压缩记录（模拟 LLM 摘要失败回退截断）。
type degradedRecorder struct{}

func (degradedRecorder) Compact(msgs []schema.Message) []schema.Message {
	if len(msgs) <= 2 {
		return msgs
	}
	return append([]schema.Message{msgs[0]}, msgs[len(msgs)-1:]...)
}

func (c degradedRecorder) CompactWithRecord(msgs []schema.Message) ([]schema.Message, memory.CompactionRecord) {
	result := c.Compact(msgs)
	return result, memory.CompactionRecord{
		TokensBefore: memory.EstimateTokens(msgs),
		TokensAfter:  memory.EstimateTokens(result),
		MsgsBefore:   len(msgs),
		MsgsAfter:    len(result),
		Error:        "LLM summary failed in TierSoft",
	}
}

// TestPrepareTurnInput_DegradedCompactionNotPersisted 验证降级压缩（Error 非空）
// 不写回：瞬时 LLM 失败触发的截断只作用于当次视图，下一轮可重试真正的摘要压缩，
// 避免把有损截断永久固化进历史。
func TestPrepareTurnInput_DegradedCompactionNotPersisted(t *testing.T) {
	lc, orig := newWriteBackLC(degradedRecorder{}, nil, 3)

	in := lc.prepareTurnInput()

	if len(lc.history) != len(orig) {
		t.Fatalf("降级压缩不应写回历史（%d 条），实际 %d 条", len(orig), len(lc.history))
	}
	// 但本轮 LLM 输入仍是降级截断后的视图（保命语义不变）。
	if len(in.history) >= len(orig) {
		t.Errorf("本轮视图应为降级截断结果，实际 %d 条（原始 %d 条）", len(in.history), len(orig))
	}
}
