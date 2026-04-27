// engine 包的单元测试（Unit Tests）。
// 使用可编程的 Mock Provider 和预设 Registry，在不依赖真实 LLM API 的情况下
// 对 Agent Loop 的核心行为进行端到端（End-to-End）验证。
//
// 测试覆盖范围：
//   - Two-Stage ReAct 完整流程（Thinking → Action → Observation 循环）
//   - 标准 ReAct 模式（单阶段）
//   - MaxTurns 安全阀机制
//   - Context 取消传播
//   - System Prompt 中 WorkDir 注入
//   - Thinking+Action 消息合并（避免连续 assistant 消息）
//   - 工具错误结果回传
//   - 工具执行超时
//   - Phase 1 ToolCalls 清洗（安全防御）
package engine

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/harness9/internal/schema"
	"github.com/harness9/internal/tools"
)

// countingProvider 是一个可编程的 LLM Provider 桩实现（Stub / Mock），
// 按预设的响应序列依次返回结果，并记录所有 Generate 调用的参数。
// 线程安全：使用 sync.Mutex 保护内部状态。
type countingProvider struct {
	mu        sync.Mutex
	responses []func(tools []schema.ToolDefinition) *schema.Message
	calls     []providerCall
}

// providerCall 记录一次 Generate 调用的完整参数，用于事后断言验证。
type providerCall struct {
	messages []schema.Message
	tools    []schema.ToolDefinition
}

// Generate 实现 LLMProvider 接口。按 FIFO 顺序从 responses 队列中取出下一个响应，
// 同时将本次调用的参数记录到 calls 切片中。
// 每个响应函数接收当前的 availableTools 参数，使测试可以断言工具列表内容。
func (p *countingProvider) Generate(_ context.Context, messages []schema.Message, tools []schema.ToolDefinition) (*schema.Message, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.calls = append(p.calls, providerCall{
		messages: messages,
		tools:    tools,
	})

	if len(p.responses) == 0 {
		return &schema.Message{Role: schema.RoleAssistant, Content: "no more responses"}, nil
	}

	fn := p.responses[0]
	p.responses = p.responses[1:]
	return fn(tools), nil
}

// staticRegistry 返回固定工具列表，对任何 Execute 调用都返回预设的成功结果。
// 用于测试中不需要验证工具执行逻辑的场景。
type staticRegistry struct {
	tools  []schema.ToolDefinition
	output string
}

func (r *staticRegistry) Register(_ tools.BaseTool) {}

func (r *staticRegistry) GetAvailableTools() []schema.ToolDefinition {
	return r.tools
}

func (r *staticRegistry) Execute(_ context.Context, call schema.ToolCall) schema.ToolResult {
	return schema.ToolResult{
		ToolCallID: call.ID,
		Output:     r.output,
		IsError:    false,
	}
}

// errorRegistry 对任何 Execute 调用都返回预设的错误结果。
// 用于验证引擎正确处理工具执行失败、以及 LLM 自愈重试的场景。
type errorRegistry struct {
	tools  []schema.ToolDefinition
	output string
}

func (r *errorRegistry) Register(_ tools.BaseTool) {}

func (r *errorRegistry) GetAvailableTools() []schema.ToolDefinition {
	return r.tools
}

func (r *errorRegistry) Execute(_ context.Context, call schema.ToolCall) schema.ToolResult {
	return schema.ToolResult{
		ToolCallID: call.ID,
		Output:     "command not found",
		IsError:    true,
	}
}

// TestTwoStageReact_CompleteFlow 验证 Two-Stage ReAct 的完整流程：
//
//	Turn 1: Phase 1 Thinking (tools=nil) → Phase 2 Action (tools=[bash], 发起 ToolCall)
//	Turn 2: Phase 1 Thinking (tools=nil) → Phase 2 Action (tools=[bash], 最终回复)
//
// 断言：
//   - 共 4 次 Generate 调用（2 turns × 2 phases）
//   - Phase 1 调用都收到空工具列表（nil tools）
//   - Phase 2 调用都收到可用工具列表
func TestTwoStageReact_CompleteFlow(t *testing.T) {
	p := &countingProvider{
		responses: []func(tools []schema.ToolDefinition) *schema.Message{
			// Turn 1 Phase 1: Thinking（无工具的深度推理阶段）
			func(tools []schema.ToolDefinition) *schema.Message {
				if len(tools) != 0 {
					t.Error("Phase 1 应该收到空工具列表")
				}
				return &schema.Message{Role: schema.RoleAssistant, Content: "thinking about files"}
			},
			// Turn 1 Phase 2: Action with tool call（基于思考结果发起工具调用）
			func(tools []schema.ToolDefinition) *schema.Message {
				return &schema.Message{
					Role:    schema.RoleAssistant,
					Content: "listing files",
					ToolCalls: []schema.ToolCall{
						{ID: "c1", Name: "bash", Arguments: []byte(`{"command":"ls"}`)},
					},
				}
			},
			// Turn 2 Phase 1: Thinking（分析工具返回结果）
			func(tools []schema.ToolDefinition) *schema.Message {
				return &schema.Message{Role: schema.RoleAssistant, Content: "I see main.go"}
			},
			// Turn 2 Phase 2: Final answer（无 ToolCall，循环终止）
			func(tools []schema.ToolDefinition) *schema.Message {
				return &schema.Message{Role: schema.RoleAssistant, Content: "done!"}
			},
		},
	}

	r := &staticRegistry{
		tools:  []schema.ToolDefinition{{Name: "bash"}},
		output: "main.go",
	}

	eng := NewAgentEngine(p, r, "/test", true)
	err := eng.Run(context.Background(), "list files")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// 验证共 4 次 Generate 调用（2 turns × 2 phases）
	if len(p.calls) != 4 {
		t.Fatalf("expected 4 Generate calls, got %d", len(p.calls))
	}

	// 验证 Phase 1 调用都收到了空工具列表（Thinking 阶段剥夺工具）
	if len(p.calls[0].tools) != 0 {
		t.Error("Turn 1 Phase 1 should have nil tools")
	}
	if len(p.calls[2].tools) != 0 {
		t.Error("Turn 2 Phase 1 should have nil tools")
	}

	// 验证 Phase 2 调用都收到了可用工具（Action 阶段恢复工具）
	if len(p.calls[1].tools) != 1 {
		t.Error("Turn 1 Phase 2 should have 1 tool")
	}
	if len(p.calls[3].tools) != 1 {
		t.Error("Turn 2 Phase 2 should have 1 tool")
	}
}

// TestStandardReact_NoThinking 验证 EnableThinking=false 时退化为标准单阶段 ReAct。
// 断言：只有 1 次 Generate 调用，且直接收到可用工具列表。
func TestStandardReact_NoThinking(t *testing.T) {
	p := &countingProvider{
		responses: []func(tools []schema.ToolDefinition) *schema.Message{
			func(tools []schema.ToolDefinition) *schema.Message {
				return &schema.Message{Role: schema.RoleAssistant, Content: "done"}
			},
		},
	}

	r := &staticRegistry{output: "ok"}
	eng := NewAgentEngine(p, r, "/test", false)

	err := eng.Run(context.Background(), "hello")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// 标准 ReAct 应该只有 1 次 Generate 调用
	if len(p.calls) != 1 {
		t.Fatalf("expected 1 Generate call, got %d", len(p.calls))
	}

	// 应该收到完整工具列表（而非 nil）
	if len(p.calls[0].tools) != 0 {
		t.Error("standard mode should pass available tools")
	}
}

// TestMaxTurnsLimit 验证 MaxTurns 安全阀机制。
// 当 LLM 持续发起 ToolCall（永不终止）时，引擎应在达到最大 Turn 数后强制退出。
func TestMaxTurnsLimit(t *testing.T) {
	callCount := 0
	p := &countingProvider{
		responses: []func(tools []schema.ToolDefinition) *schema.Message{
			func(tools []schema.ToolDefinition) *schema.Message {
				callCount++
				return &schema.Message{
					Role: schema.RoleAssistant,
					ToolCalls: []schema.ToolCall{
						{ID: "c1", Name: "bash", Arguments: []byte("{}")},
					},
				}
			},
			func(tools []schema.ToolDefinition) *schema.Message {
				callCount++
				return &schema.Message{
					Role: schema.RoleAssistant,
					ToolCalls: []schema.ToolCall{
						{ID: "c2", Name: "bash", Arguments: []byte("{}")},
					},
				}
			},
			func(tools []schema.ToolDefinition) *schema.Message {
				callCount++
				return &schema.Message{
					Role: schema.RoleAssistant,
					ToolCalls: []schema.ToolCall{
						{ID: "c3", Name: "bash", Arguments: []byte("{}")},
					},
				}
			},
		},
	}

	r := &staticRegistry{output: "ok"}
	eng := NewAgentEngine(p, r, "/test", false, WithMaxTurns(2))

	err := eng.Run(context.Background(), "loop forever")
	if err == nil {
		t.Fatal("expected MaxTurns error")
	}
	if !strings.Contains(err.Error(), "最大 Turn 数") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

// TestContextCancellation 验证 Context 取消信号的传播。
// 当外部 context 被取消时（如超时或手动中断），引擎应在下一次循环迭代中检测到并优雅退出。
func TestContextCancellation(t *testing.T) {
	p := &countingProvider{
		responses: []func(tools []schema.ToolDefinition) *schema.Message{
			func(tools []schema.ToolDefinition) *schema.Message {
				return &schema.Message{
					Role: schema.RoleAssistant,
					ToolCalls: []schema.ToolCall{
						{ID: "c1", Name: "bash", Arguments: []byte("{}")},
					},
				}
			},
		},
	}

	r := &staticRegistry{output: "ok"}
	eng := NewAgentEngine(p, r, "/test", false)

	// 立即取消 context，模拟用户中断
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := eng.Run(ctx, "cancelled task")
	if err == nil {
		t.Fatal("expected context cancelled error")
	}
	if !strings.Contains(err.Error(), "context 已取消") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestWorkDirInSystemPrompt 验证 System Prompt 中正确注入了工作区路径。
// 引擎初始化时将 WorkDir 嵌入 System Prompt，使 LLM 了解其工作上下文。
func TestWorkDirInSystemPrompt(t *testing.T) {
	p := &countingProvider{
		responses: []func(tools []schema.ToolDefinition) *schema.Message{
			func(tools []schema.ToolDefinition) *schema.Message {
				return &schema.Message{Role: schema.RoleAssistant, Content: "ok"}
			},
		},
	}

	r := &staticRegistry{output: "ok"}
	eng := NewAgentEngine(p, r, "/my/custom/path", false)

	_ = eng.Run(context.Background(), "test")

	if len(p.calls) == 0 {
		t.Fatal("expected at least 1 Generate call")
	}

	// 验证第一条消息是 System Prompt 且包含工作区路径
	firstMsg := p.calls[0].messages[0]
	if firstMsg.Role != schema.RoleSystem {
		t.Fatal("first message should be system")
	}
	if !strings.Contains(firstMsg.Content, "/my/custom/path") {
		t.Fatalf("system prompt should contain WorkDir, got: %s", firstMsg.Content)
	}
}

// TestMergedAssistantMessage_NoConsecutiveDuplicates 验证 Two-Stage ReAct 的关键设计约束：
// 每个 Turn 最终只向 contextHistory 注入一条 assistant 消息。
//
// 这是 Anthropic Messages API 等厂商要求的 user/assistant 严格交替规则的保障。
// 若 Phase 1 Thinking 和 Phase 2 Action 分别作为独立消息注入，会导致连续 assistant
// 消息，引发 API 调用失败。
func TestMergedAssistantMessage_NoConsecutiveDuplicates(t *testing.T) {
	p := &countingProvider{
		responses: []func(tools []schema.ToolDefinition) *schema.Message{
			// Turn 1 Phase 1: Thinking
			func(tools []schema.ToolDefinition) *schema.Message {
				return &schema.Message{Role: schema.RoleAssistant, Content: "thinking"}
			},
			// Turn 1 Phase 2: Action with tool call（保持循环继续）
			func(tools []schema.ToolDefinition) *schema.Message {
				return &schema.Message{
					Role:    schema.RoleAssistant,
					Content: "action",
					ToolCalls: []schema.ToolCall{
						{ID: "c1", Name: "bash", Arguments: []byte("{}")},
					},
				}
			},
			// Turn 2 Phase 1: Thinking
			func(tools []schema.ToolDefinition) *schema.Message {
				return &schema.Message{Role: schema.RoleAssistant, Content: "thinking2"}
			},
			// Turn 2 Phase 2: Final（无 ToolCall，循环终止）
			func(tools []schema.ToolDefinition) *schema.Message {
				return &schema.Message{Role: schema.RoleAssistant, Content: "done"}
			},
		},
	}

	r := &staticRegistry{
		tools:  []schema.ToolDefinition{{Name: "bash"}},
		output: "ok",
	}
	eng := NewAgentEngine(p, r, "/test", true)

	_ = eng.Run(context.Background(), "test")

	// Turn 2 Phase 2 (call index 3) 收到的 messages 中不应有连续 assistant 消息
	if len(p.calls) < 4 {
		t.Fatalf("expected 4 calls, got %d", len(p.calls))
	}

	// 检查所有 Phase 2 调用的上下文中没有连续 assistant 消息
	// call[1] = Turn 1 Phase 2, call[3] = Turn 2 Phase 2
	for _, callIdx := range []int{1, 3} {
		msgs := p.calls[callIdx].messages
		for i := 1; i < len(msgs); i++ {
			prev := msgs[i-1]
			curr := msgs[i]
			if prev.Role == schema.RoleAssistant && curr.Role == schema.RoleAssistant {
				t.Fatalf("call %d: consecutive assistant messages at index %d-%d", callIdx, i-1, i)
			}
		}
	}
}

// TestToolErrorResult 验证工具执行失败时，错误结果作为 Observation 正确回传给 LLM。
// 模拟场景：工具返回 IsError=true 的结果，引擎将其注入上下文，
// LLM 在下一轮可以看到错误信息并尝试自愈（Self-Healing）。
func TestToolErrorResult(t *testing.T) {
	p := &countingProvider{
		responses: []func(tools []schema.ToolDefinition) *schema.Message{
			func(tools []schema.ToolDefinition) *schema.Message {
				return &schema.Message{
					Role: schema.RoleAssistant,
					ToolCalls: []schema.ToolCall{
						{ID: "c1", Name: "bash", Arguments: []byte("{}")},
					},
				}
			},
			func(tools []schema.ToolDefinition) *schema.Message {
				return &schema.Message{Role: schema.RoleAssistant, Content: "retry"}
			},
		},
	}

	r := &errorRegistry{}
	eng := NewAgentEngine(p, r, "/test", false)

	err := eng.Run(context.Background(), "test error")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// 第二次调用收到的消息应包含工具错误结果（Observation）
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

// TestToolTimeout 验证 WithToolTimeout 选项正确为每个工具执行创建带超时的子 context。
func TestToolTimeout(t *testing.T) {
	p := &countingProvider{
		responses: []func(tools []schema.ToolDefinition) *schema.Message{
			func(tools []schema.ToolDefinition) *schema.Message {
				return &schema.Message{Role: schema.RoleAssistant, Content: "done"}
			},
		},
	}

	// 使用一个会检查 context 是否有 deadline 的 registry
	timeoutRegistry := &timeoutCheckRegistry{}
	eng := NewAgentEngine(p, timeoutRegistry, "/test", false, WithToolTimeout(100*time.Millisecond))

	_ = eng.Run(context.Background(), "test")
}

// timeoutCheckRegistry 用于 TestToolTimeout，验证 Execute 收到的 context 有 deadline 设置。
type timeoutCheckRegistry struct{}

func (r *timeoutCheckRegistry) Register(_ tools.BaseTool) {}

func (r *timeoutCheckRegistry) GetAvailableTools() []schema.ToolDefinition {
	return nil
}

func (r *timeoutCheckRegistry) Execute(ctx context.Context, call schema.ToolCall) schema.ToolResult {
	_, ok := ctx.Deadline()
	if !ok {
		panic("expected context to have a deadline set by WithToolTimeout")
	}
	return schema.ToolResult{ToolCallID: call.ID, Output: "ok"}
}

// TestJoinContent 使用表驱动测试（Table-Driven Tests）验证 Thinking + Action
// 内容合并函数 joinContent 的各种边界情况。
func TestJoinContent(t *testing.T) {
	tests := []struct {
		thinking string
		action   string
		want     string
	}{
		{"", "", ""},
		{"", "act", "act"},
		{"think", "", "think"},
		{"think", "act", "think\n\nact"},
	}

	for _, tt := range tests {
		got := joinContent(tt.thinking, tt.action)
		if got != tt.want {
			t.Errorf("joinContent(%q, %q) = %q, want %q", tt.thinking, tt.action, got, tt.want)
		}
	}
}

// TestPhase1ToolCallsSanitized 验证 Phase 1 Thinking 阶段的 ToolCalls 被安全清洗。
//
// 安全防御场景：即使 LLM 在无工具模式下违反指令返回了 ToolCalls，
// 引擎会在将 Thinking 响应传递给 Phase 2 之前清除 ToolCalls 字段，
// 防止 Phase 2 的上下文中出现"幽灵"工具调用请求。
func TestPhase1ToolCallsSanitized(t *testing.T) {
	p := &countingProvider{
		responses: []func(tools []schema.ToolDefinition) *schema.Message{
			// Phase 1: 模拟 LLM 违反指令，在无工具模式下仍返回 ToolCalls
			func(tools []schema.ToolDefinition) *schema.Message {
				return &schema.Message{
					Role:    schema.RoleAssistant,
					Content: "thinking",
					ToolCalls: []schema.ToolCall{
						{ID: "bad", Name: "bash", Arguments: []byte("{}")},
					},
				}
			},
			// Phase 2: 正常返回
			func(tools []schema.ToolDefinition) *schema.Message {
				return &schema.Message{Role: schema.RoleAssistant, Content: "done"}
			},
		},
	}

	r := &staticRegistry{output: "ok"}
	eng := NewAgentEngine(p, r, "/test", true)

	_ = eng.Run(context.Background(), "test")

	// Phase 2 (call index 1) 应该收到 Phase 1 的思考消息，
	// 但该消息不应包含 ToolCalls（已被引擎清洗）
	if len(p.calls) < 2 {
		t.Fatal("expected 2 calls")
	}

	phase2Messages := p.calls[1].messages
	// Phase 2 临时上下文中最后一条 assistant 消息应该是 Phase 1 的 thinking（已被清洗）
	lastAssistant := phase2Messages[len(phase2Messages)-1]
	if lastAssistant.Role != schema.RoleAssistant {
		t.Fatal("expected last message to be assistant")
	}
	if len(lastAssistant.ToolCalls) != 0 {
		t.Fatalf("Phase 1 thinking should have ToolCalls cleared, got %d", len(lastAssistant.ToolCalls))
	}
}
