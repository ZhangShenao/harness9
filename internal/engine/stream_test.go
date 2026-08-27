// stream_test.go covers RunStream and its streaming event pipeline.
// It reuses countingProvider and staticRegistry from agent_loop_test.go (same package).
package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/harness9/internal/hooks"
	"github.com/harness9/internal/provider/providertest"
	"github.com/harness9/internal/schema"
	"github.com/harness9/internal/tools"
)

// collectEvents drains a stream channel and returns all received events.
func collectEvents(stream <-chan Event) []Event {
	var events []Event
	for evt := range stream {
		events = append(events, evt)
	}
	return events
}

func TestRunStream_NormalCompletion_ReceivesEventDone(t *testing.T) {
	p := &countingProvider{
		responses: []func(tools []schema.ToolDefinition) *schema.Message{
			func(_ []schema.ToolDefinition) *schema.Message {
				return &schema.Message{Role: schema.RoleAssistant, Content: "finished"}
			},
		},
	}
	r := &staticRegistry{output: "ok"}
	eng := NewAgentEngine(p, r, "/test")

	stream, err := eng.RunStream(context.Background(), "hello")
	if err != nil {
		t.Fatalf("RunStream error: %v", err)
	}

	events := collectEvents(stream)
	if len(events) == 0 {
		t.Fatal("expected at least one event")
	}
	last := events[len(events)-1]
	if last.Type != EventDone {
		t.Errorf("expected last event to be EventDone, got %q", last.Type)
	}
}

// TestRunStream_EmitsAllEventTypes 验证携带工具调用的完整流程中各事件类型均被发出。
func TestRunStream_EmitsAllEventTypes(t *testing.T) {
	p := &countingProvider{
		responses: []func(tools []schema.ToolDefinition) *schema.Message{
			// Turn 1: Action with tool call
			func(_ []schema.ToolDefinition) *schema.Message {
				return &schema.Message{
					Role:    schema.RoleAssistant,
					Content: "calling tool",
					ToolCalls: []schema.ToolCall{
						{ID: "c1", Name: "bash", Arguments: []byte(`{"command":"ls"}`)},
					},
				}
			},
			// Turn 2: Final reply
			func(_ []schema.ToolDefinition) *schema.Message {
				return &schema.Message{Role: schema.RoleAssistant, Content: "all done"}
			},
		},
	}
	r := &staticRegistry{
		tools:  []schema.ToolDefinition{{Name: "bash"}},
		output: "main.go",
	}
	eng := NewAgentEngine(p, r, "/test")

	stream, err := eng.RunStream(context.Background(), "list files")
	if err != nil {
		t.Fatalf("RunStream error: %v", err)
	}

	counts := map[EventType]int{}
	for _, evt := range collectEvents(stream) {
		counts[evt.Type]++
	}

	checks := map[EventType]string{
		EventActionDelta: "at least one EventActionDelta",
		EventToolStart:   "at least one EventToolStart",
		EventToolResult:  "at least one EventToolResult",
		EventDone:        "EventDone at end",
	}
	for evtType, desc := range checks {
		if counts[evtType] == 0 {
			t.Errorf("expected %s, got counts=%v", desc, counts)
		}
	}
	if counts[EventError] != 0 {
		t.Errorf("expected no EventError, got %d", counts[EventError])
	}
}

func TestRunStream_ContextCancellation_ReceivesEventError(t *testing.T) {
	p := &countingProvider{
		responses: []func(tools []schema.ToolDefinition) *schema.Message{
			func(_ []schema.ToolDefinition) *schema.Message {
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

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	eng := NewAgentEngine(p, r, "/test")
	stream, err := eng.RunStream(ctx, "task")
	if err != nil {
		t.Fatalf("RunStream error: %v", err)
	}

	var sawError bool
	for _, evt := range collectEvents(stream) {
		if evt.Type == EventError {
			sawError = true
		}
	}
	if !sawError {
		t.Error("expected EventError when context is pre-cancelled")
	}
}

// TestRunStream_MaxTurns_ReceivesEventTerminated 验证 MaxTurns 属受控熔断：
// 流中应发出 EventTerminated（携带 reason=max_turns 与"最大"措辞的消息），
// 且不再补发 EventError——两者互斥，避免消费者对同一次终止渲染两条消息。
func TestRunStream_MaxTurns_ReceivesEventTerminated(t *testing.T) {
	p := &countingProvider{
		responses: []func(tools []schema.ToolDefinition) *schema.Message{
			func(_ []schema.ToolDefinition) *schema.Message {
				return &schema.Message{Role: schema.RoleAssistant, ToolCalls: []schema.ToolCall{{ID: "c1", Name: "bash", Arguments: []byte("{}")}}}
			},
			func(_ []schema.ToolDefinition) *schema.Message {
				return &schema.Message{Role: schema.RoleAssistant, ToolCalls: []schema.ToolCall{{ID: "c2", Name: "bash", Arguments: []byte("{}")}}}
			},
		},
	}
	r := &staticRegistry{output: "ok"}
	eng := NewAgentEngine(p, r, "/test", WithMaxTurns(1))

	stream, err := eng.RunStream(context.Background(), "loop")
	if err != nil {
		t.Fatalf("RunStream error: %v", err)
	}

	var termData TerminationData
	sawError := false
	for _, evt := range collectEvents(stream) {
		switch evt.Type {
		case EventTerminated:
			if d, ok := evt.Data.(TerminationData); ok {
				termData = d
			}
		case EventError:
			sawError = true
		}
	}
	if termData.Reason == "" {
		t.Fatal("expected EventTerminated when MaxTurns exceeded")
	}
	if termData.Reason != ReasonMaxTurns {
		t.Errorf("reason should be max_turns, got %q", termData.Reason)
	}
	if !strings.Contains(termData.Message, "最大") {
		t.Errorf("termination message should mention max turns, got: %q", termData.Message)
	}
	if sawError {
		t.Error("controlled termination must not emit EventError")
	}
}

func TestRunStream_ToolStartBeforeToolResult(t *testing.T) {
	p := &countingProvider{
		responses: []func(tools []schema.ToolDefinition) *schema.Message{
			func(_ []schema.ToolDefinition) *schema.Message {
				return &schema.Message{
					Role:      schema.RoleAssistant,
					ToolCalls: []schema.ToolCall{{ID: "c1", Name: "bash", Arguments: []byte(`{}`)}},
				}
			},
			func(_ []schema.ToolDefinition) *schema.Message {
				return &schema.Message{Role: schema.RoleAssistant, Content: "done"}
			},
		},
	}
	r := &staticRegistry{tools: []schema.ToolDefinition{{Name: "bash"}}, output: "hi"}
	eng := NewAgentEngine(p, r, "/test")

	stream, err := eng.RunStream(context.Background(), "test")
	if err != nil {
		t.Fatalf("RunStream error: %v", err)
	}

	var events []EventType
	for _, evt := range collectEvents(stream) {
		events = append(events, evt.Type)
	}

	startIdx, resultIdx := -1, -1
	for i, et := range events {
		if et == EventToolStart && startIdx == -1 {
			startIdx = i
		}
		if et == EventToolResult && resultIdx == -1 {
			resultIdx = i
		}
	}
	if startIdx == -1 {
		t.Fatal("expected EventToolStart in stream")
	}
	if resultIdx == -1 {
		t.Fatal("expected EventToolResult in stream")
	}
	if startIdx > resultIdx {
		t.Error("EventToolStart should appear before EventToolResult")
	}
}

func TestRunStream_EventTurnNumbering(t *testing.T) {
	p := &countingProvider{
		responses: []func(tools []schema.ToolDefinition) *schema.Message{
			func(_ []schema.ToolDefinition) *schema.Message {
				return &schema.Message{Role: schema.RoleAssistant, Content: "turn 1 done"}
			},
		},
	}
	r := &staticRegistry{output: "ok"}
	eng := NewAgentEngine(p, r, "/test")

	stream, err := eng.RunStream(context.Background(), "test")
	if err != nil {
		t.Fatalf("RunStream error: %v", err)
	}

	for _, evt := range collectEvents(stream) {
		if evt.Type == EventActionDelta && evt.Turn != 1 {
			t.Errorf("first action delta should have Turn=1, got %d", evt.Turn)
		}
	}
}

func TestRunStream_ToolResultDataIsToolResult(t *testing.T) {
	p := &countingProvider{
		responses: []func(tools []schema.ToolDefinition) *schema.Message{
			func(_ []schema.ToolDefinition) *schema.Message {
				return &schema.Message{
					Role:      schema.RoleAssistant,
					ToolCalls: []schema.ToolCall{{ID: "c1", Name: "bash", Arguments: []byte(`{}`)}},
				}
			},
			func(_ []schema.ToolDefinition) *schema.Message {
				return &schema.Message{Role: schema.RoleAssistant, Content: "done"}
			},
		},
	}
	r := &staticRegistry{tools: []schema.ToolDefinition{{Name: "bash"}}, output: "tool output here"}
	eng := NewAgentEngine(p, r, "/test")

	stream, err := eng.RunStream(context.Background(), "test")
	if err != nil {
		t.Fatalf("RunStream error: %v", err)
	}

	for _, evt := range collectEvents(stream) {
		if evt.Type == EventToolResult {
			data, ok := evt.Data.(ToolResultData)
			if !ok {
				t.Fatalf("EventToolResult.Data should be ToolResultData, got %T", evt.Data)
			}
			if data.Result.Output != "tool output here" {
				t.Errorf("expected tool output 'tool output here', got %q", data.Result.Output)
			}
			if data.Duration < 0 {
				t.Errorf("Duration should be non-negative, got %v", data.Duration)
			}
			return
		}
	}
	t.Fatal("expected EventToolResult event")
}

// thinkingProvider 是一个在流式响应中同时发送 thinking_delta 和 text_delta 的测试桩。
type thinkingProvider struct{}

// Generate 未在流式路径中使用，仅满足 LLMProvider 接口约束。
func (p *thinkingProvider) Generate(_ context.Context, _ []schema.Message, _ []schema.ToolDefinition) (*schema.Message, *schema.Usage, error) {
	return &schema.Message{Role: schema.RoleAssistant, Content: "result"}, nil, nil
}

func (p *thinkingProvider) GenerateStream(_ context.Context, _ []schema.Message, _ []schema.ToolDefinition) (<-chan schema.StreamChunk, error) {
	ch := make(chan schema.StreamChunk, 3)
	go func() {
		defer close(ch)
		ch <- schema.StreamChunk{Type: schema.StreamChunkThinkingDelta, Delta: "reasoning step"}
		ch <- schema.StreamChunk{Type: schema.StreamChunkTextDelta, Delta: "result"}
		msg := &schema.Message{Role: schema.RoleAssistant, Content: "result"}
		ch <- schema.StreamChunk{Type: schema.StreamChunkDone, Message: msg}
	}()
	return ch, nil
}

func TestRunStream_ThinkingDelta_EmitsEventThinkingDelta(t *testing.T) {
	p := &thinkingProvider{}
	r := &staticRegistry{output: "ok"}
	eng := NewAgentEngine(p, r, "/test")

	stream, err := eng.RunStream(context.Background(), "think")
	if err != nil {
		t.Fatalf("RunStream error: %v", err)
	}

	counts := map[EventType]int{}
	for _, evt := range collectEvents(stream) {
		counts[evt.Type]++
	}

	if counts[EventThinkingDelta] == 0 {
		t.Errorf("expected at least one EventThinkingDelta, got counts=%v", counts)
	}
	if counts[EventActionDelta] == 0 {
		t.Errorf("expected at least one EventActionDelta, got counts=%v", counts)
	}
}

func TestRunStream_ThinkingDelta_DataIsString(t *testing.T) {
	p := &thinkingProvider{}
	r := &staticRegistry{output: "ok"}
	eng := NewAgentEngine(p, r, "/test")

	stream, err := eng.RunStream(context.Background(), "think")
	if err != nil {
		t.Fatalf("RunStream error: %v", err)
	}

	for _, evt := range collectEvents(stream) {
		if evt.Type == EventThinkingDelta {
			s, ok := evt.Data.(string)
			if !ok {
				t.Fatalf("EventThinkingDelta.Data should be string, got %T", evt.Data)
			}
			if s != "reasoning step" {
				t.Errorf("expected %q, got %q", "reasoning step", s)
			}
			return
		}
	}
	t.Fatal("no EventThinkingDelta found")
}

// TestRunStream_ThinkingDelta_PrecedesActionDelta 验证 EventThinkingDelta
// 总在 EventActionDelta 之前到达（流中推理先于正文产生）。
func TestRunStream_ThinkingDelta_PrecedesActionDelta(t *testing.T) {
	p := &thinkingProvider{}
	r := &staticRegistry{output: "ok"}
	eng := NewAgentEngine(p, r, "/test")

	stream, err := eng.RunStream(context.Background(), "think")
	if err != nil {
		t.Fatalf("RunStream error: %v", err)
	}

	events := collectEvents(stream)
	thinkIdx, actionIdx := -1, -1
	for i, evt := range events {
		if evt.Type == EventThinkingDelta && thinkIdx == -1 {
			thinkIdx = i
		}
		if evt.Type == EventActionDelta && actionIdx == -1 {
			actionIdx = i
		}
	}
	if thinkIdx == -1 {
		t.Fatal("expected at least one EventThinkingDelta")
	}
	if actionIdx == -1 {
		t.Fatal("expected at least one EventActionDelta")
	}
	if thinkIdx > actionIdx {
		t.Errorf("EventThinkingDelta (idx=%d) should precede EventActionDelta (idx=%d)", thinkIdx, actionIdx)
	}
}

func TestRunStreamInjectsSubAgentProgress(t *testing.T) {
	reg := tools.NewRegistry()
	if err := reg.Register(&progressProbeTool{}); err != nil {
		t.Fatal(err)
	}
	eng := NewAgentEngine(providertest.NewMock(), reg, t.TempDir())

	stream, err := eng.RunStream(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	var sawSubAgent bool
	for evt := range stream {
		if evt.Type == EventSubAgent {
			u, ok := evt.Data.(schema.SubAgentUpdate)
			if ok && u.AgentName == "probe" {
				sawSubAgent = true
			}
		}
	}
	if !sawSubAgent {
		t.Fatal("期望收到 EventSubAgent 事件，未收到")
	}
}

// progressProbeTool 在 Execute 中触发一次子代理进度更新。
type progressProbeTool struct{}

func (p *progressProbeTool) Name() string { return "bash" }

func (p *progressProbeTool) Definition() schema.ToolDefinition {
	return schema.ToolDefinition{
		Name:        "bash",
		Description: "probe",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"command": map[string]any{"type": "string"},
		}},
	}
}

func (p *progressProbeTool) Execute(ctx context.Context, _ json.RawMessage) (string, error) {
	if fn := hooks.SubAgentProgressFromContext(ctx); fn != nil {
		fn(schema.SubAgentUpdate{AgentName: "probe", Kind: schema.SubAgentStart})
	}
	return "ok", nil
}
