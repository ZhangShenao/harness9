// engine 包的单元测试。
// 使用可编程的 Mock Provider 和预设 Registry，在不依赖真实 LLM API 的情况下
// 对 Agent Loop 的核心行为进行端到端验证。
//
// 测试覆盖范围：
//   - 基础 ReAct 流程（Action → Observation 循环）
//   - MaxTurns 安全阀机制
//   - Context 取消传播
//   - System Prompt 中 WorkDir 注入
//   - 工具错误结果回传
//   - 工具执行超时
package engine

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/harness9/internal/planning"
	"github.com/harness9/internal/provider"
	"github.com/harness9/internal/provider/providertest"
	"github.com/harness9/internal/schema"
	"github.com/harness9/internal/tools"
)

// countingProvider 是一个可编程的 LLM Provider 桩实现，
// 按预设的响应序列依次返回结果，并记录所有 Generate 调用的参数。
type countingProvider struct {
	mu        sync.Mutex
	responses []func(tools []schema.ToolDefinition) *schema.Message
	calls     []providerCall
}

// providerCall 记录一次 Generate 调用的完整参数。
type providerCall struct {
	messages []schema.Message
	tools    []schema.ToolDefinition
}

func (p *countingProvider) Generate(_ context.Context, messages []schema.Message, tools []schema.ToolDefinition) (*schema.Message, *schema.Usage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.calls = append(p.calls, providerCall{messages: messages, tools: tools})

	if len(p.responses) == 0 {
		return &schema.Message{Role: schema.RoleAssistant, Content: "no more responses"}, nil, nil
	}
	fn := p.responses[0]
	p.responses = p.responses[1:]
	return fn(tools), nil, nil
}

func (p *countingProvider) GenerateStream(ctx context.Context, messages []schema.Message, tools []schema.ToolDefinition) (<-chan schema.StreamChunk, error) {
	msg, _, err := p.Generate(ctx, messages, tools)
	if err != nil {
		return nil, err
	}
	ch := make(chan schema.StreamChunk, 2)
	go func() {
		defer close(ch)
		if msg.Content != "" {
			ch <- schema.StreamChunk{Type: schema.StreamChunkTextDelta, Delta: msg.Content}
		}
		ch <- schema.StreamChunk{Type: schema.StreamChunkDone, Message: msg}
	}()
	return ch, nil
}

// staticRegistry 返回固定工具列表，对任何 Execute 调用都返回预设的成功结果。
type staticRegistry struct {
	tools  []schema.ToolDefinition
	output string
}

func (r *staticRegistry) Register(_ tools.BaseTool) error            { return nil }
func (r *staticRegistry) GetAvailableTools() []schema.ToolDefinition { return r.tools }
func (r *staticRegistry) Execute(_ context.Context, call schema.ToolCall) schema.ToolResult {
	return schema.ToolResult{ToolCallID: call.ID, Output: r.output}
}

// errorRegistry 对任何 Execute 调用都返回预设的错误结果。
type errorRegistry struct {
	tools []schema.ToolDefinition
}

func (r *errorRegistry) Register(_ tools.BaseTool) error            { return nil }
func (r *errorRegistry) GetAvailableTools() []schema.ToolDefinition { return r.tools }
func (r *errorRegistry) Execute(_ context.Context, call schema.ToolCall) schema.ToolResult {
	return schema.ToolResult{ToolCallID: call.ID, Output: "command not found", IsError: true}
}

// flakyProvider 在前 failFirst 次调用返回错误，其后正常返回 done，用于验证引擎重试。
type flakyProvider struct {
	mu        sync.Mutex
	calls     int
	failFirst int
	err       error
}

func (p *flakyProvider) Generate(_ context.Context, _ []schema.Message, _ []schema.ToolDefinition) (*schema.Message, *schema.Usage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if p.calls <= p.failFirst {
		return nil, nil, p.err
	}
	return &schema.Message{Role: schema.RoleAssistant, Content: "done!"}, nil, nil
}

func (p *flakyProvider) GenerateStream(ctx context.Context, msgs []schema.Message, td []schema.ToolDefinition) (<-chan schema.StreamChunk, error) {
	msg, _, err := p.Generate(ctx, msgs, td)
	if err != nil {
		return nil, err
	}
	ch := make(chan schema.StreamChunk, 2)
	go func() {
		defer close(ch)
		ch <- schema.StreamChunk{Type: schema.StreamChunkDone, Message: msg}
	}()
	return ch, nil
}

// TestGenerateRetry_RecoversFromTransientError 验证：瞬时 LLM 错误经有界重试后恢复，
// 整条轨迹不被一次抖动杀死。
func TestGenerateRetry_RecoversFromTransientError(t *testing.T) {
	p := &flakyProvider{failFirst: 2, err: fmt.Errorf("temporary stream reset")}
	r := &staticRegistry{output: "ok"}
	// 3 次尝试、极小退避（避免拖慢测试）。
	eng := NewAgentEngine(p, r, "/test", WithGenerateRetry(3, time.Millisecond))

	if err := eng.Run(context.Background(), "task"); err != nil {
		t.Fatalf("应在重试后成功，got: %v", err)
	}
	if p.calls != 3 {
		t.Errorf("应尝试 3 次（前 2 次失败 + 第 3 次成功），实际 %d", p.calls)
	}
}

// TestGenerateRetry_ExhaustsThenFails 验证：持续失败时耗尽重试后返回错误（有界）。
func TestGenerateRetry_ExhaustsThenFails(t *testing.T) {
	p := &flakyProvider{failFirst: 100, err: fmt.Errorf("persistent failure")}
	r := &staticRegistry{output: "ok"}
	eng := NewAgentEngine(p, r, "/test", WithGenerateRetry(3, time.Millisecond))

	err := eng.Run(context.Background(), "task")
	if err == nil {
		t.Fatal("持续失败应最终返回错误")
	}
	if p.calls != 3 {
		t.Errorf("应恰好尝试 3 次后放弃，实际 %d", p.calls)
	}
}

// TestGenerateRetry_DoesNotRetryOnContextCancel 验证：context 取消不触发重试。
func TestGenerateRetry_DoesNotRetryOnContextCancel(t *testing.T) {
	p := &flakyProvider{failFirst: 100, err: fmt.Errorf("should not be seen")}
	r := &staticRegistry{output: "ok"}
	eng := NewAgentEngine(p, r, "/test", WithGenerateRetry(5, time.Second))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = eng.Run(ctx, "task")
	// ctx 已取消：要么在 turn 入口的 ctx.Done 检查就退出（calls==0），
	// 要么首次 generate 后因 ctx.Err()!=nil 立即返回（calls<=1）。绝不应多次重试。
	if p.calls > 1 {
		t.Errorf("context 取消不应重试，calls=%d", p.calls)
	}
}

// TestGenerateRetry_NetworkErrorGetsExtendedBudget 验证网络传输层错误（TLS/DNS/连接建立）
// 使用独立、更宽松的重试预算，即使默认 generateRetries 已耗尽也能继续重试直到恢复。
// 复现 docs/技术调研/terminal-bench-轨迹分析-v1.md §2 R2:3 个任务在 Turn 1 就命中同一条
// x509 证书错误，默认 3 次/总计 3s 退避窗口耗尽后直接放弃整个 turn。
// mock 错误只保留分类器 isTransientNetworkError 依赖的标准库子串（`tls:`、`x509:`），
// 不复制 provider 层的消息措辞，避免生产文案调整时测试失真——分类器按子串匹配，不影响分类结果。
func TestGenerateRetry_NetworkErrorGetsExtendedBudget(t *testing.T) {
	p := &flakyProvider{failFirst: 5, err: fmt.Errorf(
		`Post "https://openrouter.ai/api/v1/chat/completions": ` +
			`tls: failed to verify certificate: x509: certificate signed by unknown authority`)}
	r := &staticRegistry{output: "ok"}
	// 默认重试预算（3 次）明显不够跑到第 6 次才成功；网络重试预算给够（6 次），
	// 极小退避避免拖慢测试。
	eng := NewAgentEngine(p, r, "/test",
		WithGenerateRetry(3, time.Millisecond),
		WithNetworkRetry(6, time.Millisecond))

	if err := eng.Run(context.Background(), "task"); err != nil {
		t.Fatalf("网络类错误应在扩展预算内恢复，got: %v", err)
	}
	if p.calls != 6 {
		t.Errorf("应尝试 6 次（前 5 次失败 + 第 6 次成功），实际 %d", p.calls)
	}
}

// TestGenerateRetry_NonNetworkErrorUsesNormalBudget 验证非网络错误依然只用默认（更小的）
// 重试预算，即使网络重试预算配置得更大也不会被"借用"——分类必须双向生效。
func TestGenerateRetry_NonNetworkErrorUsesNormalBudget(t *testing.T) {
	p := &flakyProvider{failFirst: 5, err: fmt.Errorf("persistent failure")}
	r := &staticRegistry{output: "ok"}
	eng := NewAgentEngine(p, r, "/test",
		WithGenerateRetry(3, time.Millisecond),
		WithNetworkRetry(6, time.Millisecond))

	err := eng.Run(context.Background(), "task")
	if err == nil {
		t.Fatal("非网络类持续失败应在默认预算耗尽后返回错误，不应借用网络重试预算")
	}
	if p.calls != 3 {
		t.Errorf("非网络错误应恰好尝试 3 次后放弃（默认预算），实际 %d", p.calls)
	}
}

// TestIsTransientNetworkError 验证网络传输层错误分类的正负样本。
func TestIsTransientNetworkError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"x509 证书错误", fmt.Errorf("tls: failed to verify certificate: x509: certificate signed by unknown authority"), true},
		{"DNS 解析失败", fmt.Errorf(`dial tcp: lookup api.openai.com: no such host`), true},
		{"连接被拒绝", fmt.Errorf("dial tcp 127.0.0.1:443: connect: connection refused"), true},
		{"连接被重置", fmt.Errorf("read: connection reset by peer"), true},
		{"IO 超时", fmt.Errorf("dial tcp: i/o timeout"), true},
		{"429 限流不算网络错误", fmt.Errorf("OpenAI 兼容 API 请求失败：429 Too Many Requests"), false},
		{"5xx 服务端错误不算网络错误", fmt.Errorf("OpenAI 兼容 API 返回了空的 Choices"), false},
		{"普通业务错误", fmt.Errorf("persistent failure"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isTransientNetworkError(tt.err); got != tt.want {
				t.Errorf("isTransientNetworkError(%q) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestReact_BasicFlow 验证单阶段 ReAct 的完整流程：
//
//	Turn 1: LLM Action（携带工具调用）→ Observation
//	Turn 2: LLM Action（最终回复，无工具调用）→ 终止
func TestReact_BasicFlow(t *testing.T) {
	p := &countingProvider{
		responses: []func(tools []schema.ToolDefinition) *schema.Message{
			func(tools []schema.ToolDefinition) *schema.Message {
				if len(tools) == 0 {
					t.Error("Action 调用应收到可用工具列表")
				}
				return &schema.Message{
					Role: schema.RoleAssistant,
					ToolCalls: []schema.ToolCall{
						{ID: "c1", Name: "bash", Arguments: []byte(`{"command":"ls"}`)},
					},
				}
			},
			func(tools []schema.ToolDefinition) *schema.Message {
				return &schema.Message{Role: schema.RoleAssistant, Content: "done!"}
			},
		},
	}
	r := &staticRegistry{
		tools:  []schema.ToolDefinition{{Name: "bash"}},
		output: "main.go",
	}

	eng := NewAgentEngine(p, r, "/test")
	if err := eng.Run(context.Background(), "list files"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(p.calls) != 2 {
		t.Fatalf("expected 2 Generate calls, got %d", len(p.calls))
	}
	// 两次调用都应携带工具列表
	for i, call := range p.calls {
		if len(call.tools) == 0 {
			t.Errorf("call %d should have tools", i)
		}
	}
}

// TestMaxTurnsLimit 验证 MaxTurns 安全阀：LLM 持续发起工具调用时，达到上限后强制退出。
func TestMaxTurnsLimit(t *testing.T) {
	p := &countingProvider{
		responses: []func(tools []schema.ToolDefinition) *schema.Message{
			func(_ []schema.ToolDefinition) *schema.Message {
				return &schema.Message{Role: schema.RoleAssistant, ToolCalls: []schema.ToolCall{{ID: "c1", Name: "bash", Arguments: []byte("{}")}}}
			},
			func(_ []schema.ToolDefinition) *schema.Message {
				return &schema.Message{Role: schema.RoleAssistant, ToolCalls: []schema.ToolCall{{ID: "c2", Name: "bash", Arguments: []byte("{}")}}}
			},
			func(_ []schema.ToolDefinition) *schema.Message {
				return &schema.Message{Role: schema.RoleAssistant, ToolCalls: []schema.ToolCall{{ID: "c3", Name: "bash", Arguments: []byte("{}")}}}
			},
		},
	}
	r := &staticRegistry{output: "ok"}
	eng := NewAgentEngine(p, r, "/test", WithMaxTurns(2))

	err := eng.Run(context.Background(), "loop forever")
	if err == nil {
		t.Fatal("expected MaxTurns error")
	}
	if !strings.Contains(err.Error(), "最大 Turn 数") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

// TestContextCancellation 验证 Context 取消信号的传播。
func TestContextCancellation(t *testing.T) {
	p := &countingProvider{
		responses: []func(tools []schema.ToolDefinition) *schema.Message{
			func(_ []schema.ToolDefinition) *schema.Message {
				return &schema.Message{Role: schema.RoleAssistant, ToolCalls: []schema.ToolCall{{ID: "c1", Name: "bash", Arguments: []byte("{}")}}}
			},
		},
	}
	r := &staticRegistry{output: "ok"}
	eng := NewAgentEngine(p, r, "/test")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 立即取消

	err := eng.Run(ctx, "cancelled task")
	if err == nil {
		t.Fatal("expected context cancelled error")
	}
	if !strings.Contains(err.Error(), "context 已取消") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestWorkDirInSystemPrompt 验证 System Prompt 中正确注入了工作区路径。
func TestWorkDirInSystemPrompt(t *testing.T) {
	p := &countingProvider{
		responses: []func(tools []schema.ToolDefinition) *schema.Message{
			func(_ []schema.ToolDefinition) *schema.Message {
				return &schema.Message{Role: schema.RoleAssistant, Content: "ok"}
			},
		},
	}
	r := &staticRegistry{output: "ok"}
	eng := NewAgentEngine(p, r, "/my/custom/path")

	_ = eng.Run(context.Background(), "test")

	if len(p.calls) == 0 {
		t.Fatal("expected at least 1 Generate call")
	}
	firstMsg := p.calls[0].messages[0]
	if firstMsg.Role != schema.RoleSystem {
		t.Fatal("first message should be system")
	}
	if !strings.Contains(firstMsg.Content, "/my/custom/path") {
		t.Fatalf("system prompt should contain WorkDir, got: %s", firstMsg.Content)
	}
}

// TestToolErrorResult 验证工具执行失败时，错误结果作为 Observation 正确回传给 LLM。
func TestToolErrorResult(t *testing.T) {
	p := &countingProvider{
		responses: []func(tools []schema.ToolDefinition) *schema.Message{
			func(_ []schema.ToolDefinition) *schema.Message {
				return &schema.Message{Role: schema.RoleAssistant, ToolCalls: []schema.ToolCall{{ID: "c1", Name: "bash", Arguments: []byte("{}")}}}
			},
			func(_ []schema.ToolDefinition) *schema.Message {
				return &schema.Message{Role: schema.RoleAssistant, Content: "retry"}
			},
		},
	}
	r := &errorRegistry{}
	eng := NewAgentEngine(p, r, "/test")

	if err := eng.Run(context.Background(), "test error"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(p.calls) < 2 {
		t.Fatal("expected 2 calls")
	}
	lastMsg := p.calls[1].messages[len(p.calls[1].messages)-1]
	if lastMsg.Role != schema.RoleUser {
		t.Fatal("observation should be user role")
	}
	if !strings.Contains(lastMsg.Content, "command not found") {
		t.Fatal("observation should contain error output")
	}
}

// TestToolTimeout 验证 WithToolTimeout 选项为每个工具执行创建带超时的子 context。
func TestToolTimeout(t *testing.T) {
	p := &countingProvider{
		responses: []func(tools []schema.ToolDefinition) *schema.Message{
			func(_ []schema.ToolDefinition) *schema.Message {
				return &schema.Message{Role: schema.RoleAssistant, Content: "done"}
			},
		},
	}
	timeoutRegistry := &timeoutCheckRegistry{}
	eng := NewAgentEngine(p, timeoutRegistry, "/test", WithToolTimeout(100*time.Millisecond))

	_ = eng.Run(context.Background(), "test")
}

// timeoutCheckRegistry 验证 Execute 收到的 context 有 deadline 设置。
type timeoutCheckRegistry struct{}

func (r *timeoutCheckRegistry) Register(_ tools.BaseTool) error            { return nil }
func (r *timeoutCheckRegistry) GetAvailableTools() []schema.ToolDefinition { return nil }
func (r *timeoutCheckRegistry) Execute(ctx context.Context, call schema.ToolCall) schema.ToolResult {
	if _, ok := ctx.Deadline(); !ok {
		panic("expected context to have a deadline set by WithToolTimeout")
	}
	return schema.ToolResult{ToolCallID: call.ID, Output: "ok"}
}

// mockPromptBuilder 是 PromptBuilder 接口的测试桩。
type mockPromptBuilder struct {
	content string
}

func (m *mockPromptBuilder) Build() string { return m.content }

// TestWithPromptBuilder_CustomContent 验证 WithPromptBuilder 使自定义内容作为 system prompt 传入。
func TestWithPromptBuilder_CustomContent(t *testing.T) {
	customPrompt := "custom system prompt for testing"
	p := &countingProvider{
		responses: []func(tools []schema.ToolDefinition) *schema.Message{
			func(_ []schema.ToolDefinition) *schema.Message {
				return &schema.Message{Role: schema.RoleAssistant, Content: "done"}
			},
		},
	}
	r := &staticRegistry{}
	eng := NewAgentEngine(p, r, "/test", WithPromptBuilder(&mockPromptBuilder{content: customPrompt}))

	_ = eng.Run(context.Background(), "test")

	if len(p.calls) == 0 {
		t.Fatal("expected at least 1 Generate call")
	}
	systemMsg := p.calls[0].messages[0]
	if systemMsg.Role != schema.RoleSystem {
		t.Fatalf("first message should be system role, got %q", systemMsg.Role)
	}
	if systemMsg.Content != customPrompt {
		t.Errorf("system prompt: got %q, want %q", systemMsg.Content, customPrompt)
	}
}

// TestWithPromptBuilder_FallbackDefault 验证未设置 PromptBuilder 时使用内置默认文案。
func TestWithPromptBuilder_FallbackDefault(t *testing.T) {
	p := &countingProvider{
		responses: []func(tools []schema.ToolDefinition) *schema.Message{
			func(_ []schema.ToolDefinition) *schema.Message {
				return &schema.Message{Role: schema.RoleAssistant, Content: "done"}
			},
		},
	}
	r := &staticRegistry{}
	eng := NewAgentEngine(p, r, "/fallback/path")

	_ = eng.Run(context.Background(), "test")

	systemMsg := p.calls[0].messages[0]
	if !strings.Contains(systemMsg.Content, "/fallback/path") {
		t.Errorf("default prompt should contain workDir, got: %s", systemMsg.Content)
	}
	if !strings.Contains(systemMsg.Content, "harness9") {
		t.Errorf("default prompt should contain 'harness9', got: %s", systemMsg.Content)
	}
}

// TestRunLoop_WithPlanStore_RestoresAndSaves 验证 WithPlanStore 的跨会话持久化行为：
//  1. runLoop 启动时从 Session 恢复 PlanStore 状态（Session 是持久化的 source of truth）
//  2. runLoop 结束时通过 defer 将最终状态保存回 Session
func TestRunLoop_WithPlanStore_RestoresAndSaves(t *testing.T) {
	p := &countingProvider{
		responses: []func([]schema.ToolDefinition) *schema.Message{
			func(_ []schema.ToolDefinition) *schema.Message {
				return &schema.Message{Role: schema.RoleAssistant, Content: "done"}
			},
		},
	}
	reg := &staticRegistry{output: "ok"}

	// 准备一个已有持久化 plans 的 Session（模拟上次 Run 结束后保存的状态）。
	sess := newMemorySessionForTest("sess-1")
	if err := sess.SavePlan(context.Background(), []planning.PlanItem{
		{ID: "t1", Content: "pre-existing task", Status: planning.PlanPending},
	}); err != nil {
		t.Fatalf("SavePlan setup error: %v", err)
	}

	planStore := planning.NewPlanStore()

	eng := NewAgentEngine(p, reg, t.TempDir(),
		WithPlanStore(planStore),
		WithSession(sess),
	)
	if err := eng.Run(context.Background(), "test"); err != nil {
		t.Fatalf("Run error: %v", err)
	}

	// 验证：runLoop 启动时应从 Session 恢复 plans 到 planStore。
	// 恢复后 planStore 中应有 t1（在 Run 开始时加载）。
	// Run 结束时 defer 将（恢复后、未被修改的）plans 重新保存到 Session。
	saved, err := sess.GetPlan(context.Background())
	if err != nil {
		t.Fatalf("GetPlan error: %v", err)
	}
	if len(saved) != 1 || saved[0].ID != "t1" {
		t.Errorf("expected saved plans to contain t1, got %+v", saved)
	}
}

// TestRunLoop_PlanMode_InjectsPlanPrefix 验证 Plan Mode 下用户 prompt 被注入了规划前缀。
func TestRunLoop_PlanMode_InjectsPlanPrefix(t *testing.T) {
	var receivedUserPrompt string
	p := &countingProvider{
		responses: []func([]schema.ToolDefinition) *schema.Message{
			func(_ []schema.ToolDefinition) *schema.Message {
				return &schema.Message{Role: schema.RoleAssistant, Content: "done"}
			},
		},
	}
	// 注入一个回调 provider 捕获历史消息内容
	capturing := &capturingProvider{inner: p, onGenerate: func(msgs []schema.Message) {
		for _, m := range msgs {
			if m.Role == schema.RoleUser && receivedUserPrompt == "" {
				receivedUserPrompt = m.Content
			}
		}
	}}
	reg := &staticRegistry{output: "ok"}
	eng := NewAgentEngine(capturing, reg, t.TempDir(),
		WithPlanMode(planning.PlanModePlan),
	)
	if err := eng.Run(context.Background(), "implement feature X"); err != nil {
		t.Fatalf("Run error: %v", err)
	}
	// Plan Mode 前缀必须存在于用户消息中
	if !strings.Contains(receivedUserPrompt, "plan_write") {
		t.Errorf("Plan Mode should inject planning prefix mentioning plan_write, got: %q", receivedUserPrompt)
	}
	if !strings.Contains(receivedUserPrompt, "implement feature X") {
		t.Errorf("original user prompt should be preserved, got: %q", receivedUserPrompt)
	}
}

// ---- 测试辅助类型 ----

// memorySessionForTest 是支持真实 GetPlan/SavePlan 持久化语义的 Session 桩。
// 注意：memory.MemorySession 的 SavePlan 是 no-op、GetPlan 始终返回空列表，
// 无法用于验证跨 runLoop 的 plan restore/save 行为，因此此处独立实现。
type memorySessionForTest struct {
	id   string
	msgs []schema.Message
	plan []planning.PlanItem
}

func newMemorySessionForTest(id string) *memorySessionForTest {
	return &memorySessionForTest{id: id}
}

func (s *memorySessionForTest) SessionID() string { return s.id }
func (s *memorySessionForTest) GetMessages(_ context.Context, _ int) ([]schema.Message, error) {
	return append([]schema.Message(nil), s.msgs...), nil
}
func (s *memorySessionForTest) AddMessages(_ context.Context, msgs []schema.Message) error {
	s.msgs = append(s.msgs, msgs...)
	return nil
}
func (s *memorySessionForTest) PopMessage(_ context.Context) (*schema.Message, error) {
	if len(s.msgs) == 0 {
		return nil, nil
	}
	m := s.msgs[len(s.msgs)-1]
	s.msgs = s.msgs[:len(s.msgs)-1]
	return &m, nil
}
func (s *memorySessionForTest) Clear(_ context.Context) error { s.msgs = nil; return nil }
func (s *memorySessionForTest) GetPlan(_ context.Context) ([]planning.PlanItem, error) {
	return append([]planning.PlanItem(nil), s.plan...), nil
}
func (s *memorySessionForTest) SavePlan(_ context.Context, items []planning.PlanItem) error {
	s.plan = append([]planning.PlanItem(nil), items...)
	return nil
}

// capturingProvider 包装另一个 Provider，在每次 Generate 调用时执行 onGenerate 回调。
type capturingProvider struct {
	inner      provider.LLMProvider
	onGenerate func(msgs []schema.Message)
}

func (p *capturingProvider) Generate(ctx context.Context, msgs []schema.Message, tools []schema.ToolDefinition) (*schema.Message, *schema.Usage, error) {
	p.onGenerate(msgs)
	return p.inner.Generate(ctx, msgs, tools)
}
func (p *capturingProvider) GenerateStream(ctx context.Context, msgs []schema.Message, tools []schema.ToolDefinition) (<-chan schema.StreamChunk, error) {
	p.onGenerate(msgs)
	return p.inner.GenerateStream(ctx, msgs, tools)
}

// TestMemoryNudgeInjectedEveryNTurns 验证 WithMemoryNudge 在指定 turn 间隔向 LLM 历史注入 nudge 提示。
func TestMemoryNudgeInjectedEveryNTurns(t *testing.T) {
	var captured [][]schema.Message
	mock := providertest.NewMockWithCallback(func(msgs []schema.Message, _ []schema.ToolDefinition) schema.Message {
		// 拷贝快照，返回无工具调用的终止响应。
		snap := make([]schema.Message, len(msgs))
		copy(snap, msgs)
		captured = append(captured, snap)
		return schema.Message{Role: schema.RoleAssistant, Content: "完成"}
	})
	reg := tools.NewRegistry()
	eng := NewAgentEngine(mock, reg, t.TempDir(),
		WithMemoryNudge(1, "【记忆提示】如有值得长期保留的信息，请调用 memory_write。"),
	)
	if err := eng.Run(context.Background(), "hi"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(captured) == 0 {
		t.Fatal("provider 未被调用")
	}
	found := false
	for _, m := range captured[0] {
		if strings.Contains(m.Content, "【记忆提示】") {
			found = true
		}
	}
	if !found {
		t.Error("turn 1 的历史应注入 nudge 提示")
	}
}

// TestMemoryNudgeNotPersisted 验证 nudge 仅注入每轮发送给 LLM 的临时副本，
// 绝不写入持久化的 Session（contextHistory 不被污染）。
func TestMemoryNudgeNotPersisted(t *testing.T) {
	const nudge = "【记忆提示】请勿持久化此行"
	mock := providertest.NewMockWithCallback(func(_ []schema.Message, _ []schema.ToolDefinition) schema.Message {
		return schema.Message{Role: schema.RoleAssistant, Content: "done"}
	})
	sess := newMemorySessionForTest("nudge-sess")
	eng := NewAgentEngine(mock, tools.NewRegistry(), t.TempDir(),
		WithMemoryNudge(1, nudge),
		WithSession(sess),
	)
	if err := eng.Run(context.Background(), "hi"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// 读回持久化的会话消息，确认 nudge 文本从未落盘。
	msgs, err := sess.GetMessages(context.Background(), 0)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(msgs) == 0 {
		t.Fatal("会话应至少持久化用户输入与助手回复")
	}
	for _, m := range msgs {
		if strings.Contains(m.Content, nudge) {
			t.Errorf("nudge 提示不应被持久化到会话，却出现在: %q", m.Content)
		}
	}
}

// TestEngineObserver_CallSequence 验证 EngineObserver 的回调顺序和参数正确性。
func TestEngineObserver_CallSequence(t *testing.T) {
	var (
		interactionStarts int
		interactionEnds   int
		turnStarts        []int
		turnEnds          []int
		endErr            error
	)

	obs := &testObserver{
		onStart: func(ctx context.Context, sid, prompt string) context.Context {
			interactionStarts++
			return ctx
		},
		onEnd: func(ctx context.Context, turns int, err error) {
			interactionEnds++
			endErr = err
		},
		onTurnStart: func(ctx context.Context, turn int) context.Context {
			turnStarts = append(turnStarts, turn)
			return ctx
		},
		onTurnEnd: func(ctx context.Context, turn int, hasTools bool) {
			turnEnds = append(turnEnds, turn)
		},
	}

	mock := &countingProvider{
		responses: []func(tools []schema.ToolDefinition) *schema.Message{
			func(_ []schema.ToolDefinition) *schema.Message {
				return &schema.Message{Role: schema.RoleAssistant, Content: "done"}
			},
		},
	}
	reg := &staticRegistry{output: "ok"}
	eng := NewAgentEngine(mock, reg, t.TempDir(),
		WithMaxTurns(10),
		WithEngineObserver(obs),
	)

	if err := eng.Run(context.Background(), "hello"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if interactionStarts != 1 {
		t.Errorf("OnInteractionStart called %d times, want 1", interactionStarts)
	}
	if interactionEnds != 1 {
		t.Errorf("OnInteractionEnd called %d times, want 1", interactionEnds)
	}
	if endErr != nil {
		t.Errorf("OnInteractionEnd err = %v, want nil", endErr)
	}
	if len(turnStarts) == 0 || turnStarts[0] != 1 {
		t.Errorf("OnTurnStart first call got turn %v, want [1]", turnStarts)
	}
	if len(turnEnds) != len(turnStarts) {
		t.Errorf("OnTurnEnd called %d times, OnTurnStart called %d times", len(turnEnds), len(turnStarts))
	}
}

// testObserver 是用于测试的 EngineObserver 实现。
type testObserver struct {
	onStart     func(ctx context.Context, sid, prompt string) context.Context
	onEnd       func(ctx context.Context, turns int, err error)
	onTurnStart func(ctx context.Context, turn int) context.Context
	onTurnEnd   func(ctx context.Context, turn int, hasTools bool)
}

func (o *testObserver) OnInteractionStart(ctx context.Context, sid, prompt string) context.Context {
	if o.onStart != nil {
		return o.onStart(ctx, sid, prompt)
	}
	return ctx
}
func (o *testObserver) OnInteractionEnd(ctx context.Context, turns int, err error) {
	if o.onEnd != nil {
		o.onEnd(ctx, turns, err)
	}
}
func (o *testObserver) OnTurnStart(ctx context.Context, turn int) context.Context {
	if o.onTurnStart != nil {
		return o.onTurnStart(ctx, turn)
	}
	return ctx
}
func (o *testObserver) OnTurnEnd(ctx context.Context, turn int, hasTools bool) {
	if o.onTurnEnd != nil {
		o.onTurnEnd(ctx, turn, hasTools)
	}
}

// TestEngineObserver_NilFallback 验证不注入 observer 时 nil 自动降级为 noop 不会 panic。
func TestEngineObserver_NilFallback(t *testing.T) {
	mock := &countingProvider{
		responses: []func([]schema.ToolDefinition) *schema.Message{
			func(_ []schema.ToolDefinition) *schema.Message {
				return &schema.Message{Role: schema.RoleAssistant, Content: "done"}
			},
		},
	}
	reg := &staticRegistry{output: "ok"}
	// 不调用 WithEngineObserver，observer 为 nil
	eng := NewAgentEngine(mock, reg, t.TempDir(), WithMaxTurns(10))
	if err := eng.Run(context.Background(), "hello"); err != nil {
		t.Fatalf("Run with nil observer: %v", err)
	}
}

// TestRunLoop_PlanMode_FiltersWriteTools 验证 Plan Mode 下写操作工具被过滤，只读工具保留。
func TestRunLoop_PlanMode_FiltersWriteTools(t *testing.T) {
	p := &countingProvider{
		responses: []func([]schema.ToolDefinition) *schema.Message{
			func(tools []schema.ToolDefinition) *schema.Message {
				return &schema.Message{Role: schema.RoleAssistant, Content: "done"}
			},
		},
	}

	allTools := []schema.ToolDefinition{
		{Name: "read_file", Description: "read"},
		{Name: "write_file", Description: "write"},
		{Name: "bash", Description: "bash"},
		{Name: "edit_file", Description: "edit"},
		{Name: "plan_write", Description: "plan"},
	}
	reg := &staticRegistry{tools: allTools, output: "ok"}

	eng := NewAgentEngine(p, reg, t.TempDir(),
		WithMaxTurns(1),
		WithPlanMode(planning.PlanModePlan),
	)
	err := eng.Run(context.Background(), "plan this")
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}

	if len(p.calls) != 1 {
		t.Fatalf("expected 1 LLM call, got %d", len(p.calls))
	}
	visibleTools := p.calls[0].tools
	for _, tool := range visibleTools {
		switch tool.Name {
		case "write_file", "edit_file":
			t.Errorf("tool %q should be filtered in PlanMode, but was visible", tool.Name)
		}
	}
	found := make(map[string]bool)
	for _, tool := range visibleTools {
		found[tool.Name] = true
	}
	if !found["read_file"] {
		t.Error("read_file should be visible in PlanMode")
	}
	if !found["bash"] {
		t.Error("bash should be visible in PlanMode")
	}
	if !found["plan_write"] {
		t.Error("plan_write should be visible in PlanMode (needed to write the plan)")
	}
}

// readToolCall 返回一个调用只读工具 read_file 的助手响应（非"进展"工具）。
func readToolCall(_ []schema.ToolDefinition) *schema.Message {
	return &schema.Message{
		Role:      schema.RoleAssistant,
		ToolCalls: []schema.ToolCall{{ID: "c", Name: "read_file", Arguments: []byte(`{}`)}},
	}
}

// editToolCall 返回一个调用 edit_file 的助手响应（"进展"工具，应重置停滞计数）。
func editToolCall(_ []schema.ToolDefinition) *schema.Message {
	return &schema.Message{
		Role:      schema.RoleAssistant,
		ToolCalls: []schema.ToolCall{{ID: "c", Name: "edit_file", Arguments: []byte(`{}`)}},
	}
}

func finalText(_ []schema.ToolDefinition) *schema.Message {
	return &schema.Message{Role: schema.RoleAssistant, Content: "done"}
}

func nudgeAppeared(prov *countingProvider, text string) bool {
	prov.mu.Lock()
	defer prov.mu.Unlock()
	for _, c := range prov.calls {
		for _, m := range c.messages {
			if strings.Contains(m.Content, text) {
				return true
			}
		}
	}
	return false
}

// TestStallNudge_InjectedAfterWindowWithoutProgress 验证：连续 window 轮没有任何"进展工具"
// （edit_file/write_file）调用后，引擎向发送给 LLM 的历史副本注入一次停滞提示。
func TestStallNudge_InjectedAfterWindowWithoutProgress(t *testing.T) {
	const nudge = "【停滞提示】你已多轮未做改动，请运行测试或定稿。"
	prov := &countingProvider{responses: []func([]schema.ToolDefinition) *schema.Message{
		readToolCall, readToolCall, readToolCall, readToolCall, finalText,
	}}
	reg := &staticRegistry{tools: []schema.ToolDefinition{{Name: "read_file"}, {Name: "edit_file"}}, output: "ok"}
	eng := NewAgentEngine(prov, reg, "/tmp", WithStallNudge(3, nudge))

	if err := eng.Run(context.Background(), "go"); err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if !nudgeAppeared(prov, nudge) {
		t.Error("连续多轮无进展工具后应注入停滞提示，但历史中未出现")
	}
}

// TestStallNudge_ResetByProgressTool 验证：只要在 window 内调用过进展工具（edit_file），
// 停滞计数被重置，不会触发停滞提示。
func TestStallNudge_ResetByProgressTool(t *testing.T) {
	const nudge = "【停滞提示】不应出现"
	prov := &countingProvider{responses: []func([]schema.ToolDefinition) *schema.Message{
		readToolCall, editToolCall, readToolCall, editToolCall, readToolCall, finalText,
	}}
	reg := &staticRegistry{tools: []schema.ToolDefinition{{Name: "read_file"}, {Name: "edit_file"}}, output: "ok"}
	eng := NewAgentEngine(prov, reg, "/tmp", WithStallNudge(3, nudge))

	if err := eng.Run(context.Background(), "go"); err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if nudgeAppeared(prov, nudge) {
		t.Error("进展工具应重置停滞计数，不应注入停滞提示")
	}
}

// TestStallNudge_NotPersisted 验证停滞提示只注入临时副本，绝不持久化到会话。
func TestStallNudge_NotPersisted(t *testing.T) {
	const nudge = "【停滞提示】请勿落盘"
	prov := &countingProvider{responses: []func([]schema.ToolDefinition) *schema.Message{
		readToolCall, readToolCall, readToolCall, readToolCall, finalText,
	}}
	reg := &staticRegistry{tools: []schema.ToolDefinition{{Name: "read_file"}, {Name: "edit_file"}}, output: "ok"}
	sess := newMemorySessionForTest("stall-sess")
	eng := NewAgentEngine(prov, reg, "/tmp", WithStallNudge(3, nudge), WithSession(sess))

	if err := eng.Run(context.Background(), "go"); err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	msgs, err := sess.GetMessages(context.Background(), 0)
	if err != nil {
		t.Fatalf("GetMessages 失败: %v", err)
	}
	for _, m := range msgs {
		if strings.Contains(m.Content, nudge) {
			t.Errorf("停滞提示不应被持久化，却出现在: %q", m.Content)
		}
	}
}
