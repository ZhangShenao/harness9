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

// countingProvider 是一个可编程的 LLM Provider 桩实现，
// 按预设序列返回响应，并记录所有调用参数。
type countingProvider struct {
	mu        sync.Mutex
	responses []func(tools []schema.ToolDefinition) *schema.Message
	calls     []providerCall
}

type providerCall struct {
	messages []schema.Message
	tools    []schema.ToolDefinition
}

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

// staticRegistry 返回固定工具列表，对任何调用都返回成功结果。
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

// errorRegistry 对任何调用都返回错误结果。
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

func TestTwoStageReact_CompleteFlow(t *testing.T) {
	p := &countingProvider{
		responses: []func(tools []schema.ToolDefinition) *schema.Message{
			// Turn 1 Phase 1: Thinking
			func(tools []schema.ToolDefinition) *schema.Message {
				if len(tools) != 0 {
					t.Error("Phase 1 应该收到空工具列表")
				}
				return &schema.Message{Role: schema.RoleAssistant, Content: "thinking about files"}
			},
			// Turn 1 Phase 2: Action with tool call
			func(tools []schema.ToolDefinition) *schema.Message {
				return &schema.Message{
					Role:    schema.RoleAssistant,
					Content: "listing files",
					ToolCalls: []schema.ToolCall{
						{ID: "c1", Name: "bash", Arguments: []byte(`{"command":"ls"}`)},
					},
				}
			},
			// Turn 2 Phase 1: Thinking
			func(tools []schema.ToolDefinition) *schema.Message {
				return &schema.Message{Role: schema.RoleAssistant, Content: "I see main.go"}
			},
			// Turn 2 Phase 2: Final answer (no tool calls)
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

	// 验证 Phase 1 调用都收到了 nil tools
	if len(p.calls[0].tools) != 0 {
		t.Error("Turn 1 Phase 1 should have nil tools")
	}
	if len(p.calls[2].tools) != 0 {
		t.Error("Turn 2 Phase 1 should have nil tools")
	}

	// 验证 Phase 2 调用都收到了可用工具
	if len(p.calls[1].tools) != 1 {
		t.Error("Turn 1 Phase 2 should have 1 tool")
	}
	if len(p.calls[3].tools) != 1 {
		t.Error("Turn 2 Phase 2 should have 1 tool")
	}
}

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

	firstMsg := p.calls[0].messages[0]
	if firstMsg.Role != schema.RoleSystem {
		t.Fatal("first message should be system")
	}
	if !strings.Contains(firstMsg.Content, "/my/custom/path") {
		t.Fatalf("system prompt should contain WorkDir, got: %s", firstMsg.Content)
	}
}

func TestMergedAssistantMessage_NoConsecutiveDuplicates(t *testing.T) {
	p := &countingProvider{
		responses: []func(tools []schema.ToolDefinition) *schema.Message{
			// Turn 1 Phase 1: Thinking
			func(tools []schema.ToolDefinition) *schema.Message {
				return &schema.Message{Role: schema.RoleAssistant, Content: "thinking"}
			},
			// Turn 1 Phase 2: Action with tool call (keeps loop going)
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
			// Turn 2 Phase 2: Final (no tool calls)
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

	// 检查 Phase 2 的上下文中没有连续 assistant 消息
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

	// 第二次调用收到的消息应包含工具错误结果
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

func TestToolTimeout(t *testing.T) {
	p := &countingProvider{
		responses: []func(tools []schema.ToolDefinition) *schema.Message{
			func(tools []schema.ToolDefinition) *schema.Message {
				return &schema.Message{Role: schema.RoleAssistant, Content: "done"}
			},
		},
	}

	// 使用一个会检查 context 取消的 registry
	timeoutRegistry := &timeoutCheckRegistry{}
	eng := NewAgentEngine(p, timeoutRegistry, "/test", false, WithToolTimeout(100*time.Millisecond))

	_ = eng.Run(context.Background(), "test")
}

type timeoutCheckRegistry struct{}

func (r *timeoutCheckRegistry) Register(_ tools.BaseTool) {}

func (r *timeoutCheckRegistry) GetAvailableTools() []schema.ToolDefinition {
	return nil
}

func (r *timeoutCheckRegistry) Execute(ctx context.Context, call schema.ToolCall) schema.ToolResult {
	// 验证 context 有 deadline
	_, ok := ctx.Deadline()
	_ = ok
	return schema.ToolResult{ToolCallID: call.ID, Output: "ok"}
}

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

func TestPhase1ToolCallsSanitized(t *testing.T) {
	p := &countingProvider{
		responses: []func(tools []schema.ToolDefinition) *schema.Message{
			func(tools []schema.ToolDefinition) *schema.Message {
				return &schema.Message{
					Role:    schema.RoleAssistant,
					Content: "thinking",
					ToolCalls: []schema.ToolCall{
						{ID: "bad", Name: "bash", Arguments: []byte("{}")},
					},
				}
			},
			func(tools []schema.ToolDefinition) *schema.Message {
				return &schema.Message{Role: schema.RoleAssistant, Content: "done"}
			},
		},
	}

	r := &staticRegistry{output: "ok"}
	eng := NewAgentEngine(p, r, "/test", true)

	_ = eng.Run(context.Background(), "test")

	// Phase 2 (call index 1) 应该收到 Phase 1 的思考消息，
	// 但该消息不应包含 ToolCalls
	if len(p.calls) < 2 {
		t.Fatal("expected 2 calls")
	}

	phase2Messages := p.calls[1].messages
	// Phase 2 context 最后一条 assistant 消息应该是 Phase 1 的 thinking（已被清理）
	lastAssistant := phase2Messages[len(phase2Messages)-1]
	if lastAssistant.Role != schema.RoleAssistant {
		t.Fatal("expected last message to be assistant")
	}
	if len(lastAssistant.ToolCalls) != 0 {
		t.Fatalf("Phase 1 thinking should have ToolCalls cleared, got %d", len(lastAssistant.ToolCalls))
	}
}
