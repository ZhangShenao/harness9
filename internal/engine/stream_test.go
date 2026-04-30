// stream_test.go covers RunStream and its streaming event pipeline.
// It reuses countingProvider and staticRegistry from agent_loop_test.go (same package).
package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/harness9/internal/schema"
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
	eng := NewAgentEngine(p, r, "/test", false)

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

func TestRunStream_TwoStageReact_EmitsAllEventTypes(t *testing.T) {
	p := &countingProvider{
		responses: []func(tools []schema.ToolDefinition) *schema.Message{
			// Turn 1 Phase 1: Thinking
			func(_ []schema.ToolDefinition) *schema.Message {
				return &schema.Message{Role: schema.RoleAssistant, Content: "thinking"}
			},
			// Turn 1 Phase 2: Action with tool call
			func(_ []schema.ToolDefinition) *schema.Message {
				return &schema.Message{
					Role: schema.RoleAssistant,
					ToolCalls: []schema.ToolCall{
						{ID: "c1", Name: "bash", Arguments: []byte(`{"command":"ls"}`)},
					},
				}
			},
			// Turn 2 Phase 1: Thinking
			func(_ []schema.ToolDefinition) *schema.Message {
				return &schema.Message{Role: schema.RoleAssistant, Content: "thought"}
			},
			// Turn 2 Phase 2: Final reply
			func(_ []schema.ToolDefinition) *schema.Message {
				return &schema.Message{Role: schema.RoleAssistant, Content: "all done"}
			},
		},
	}
	r := &staticRegistry{
		tools:  []schema.ToolDefinition{{Name: "bash"}},
		output: "main.go",
	}
	eng := NewAgentEngine(p, r, "/test", true)

	stream, err := eng.RunStream(context.Background(), "list files")
	if err != nil {
		t.Fatalf("RunStream error: %v", err)
	}

	counts := map[EventType]int{}
	for _, evt := range collectEvents(stream) {
		counts[evt.Type]++
	}

	checks := map[EventType]string{
		EventThinkingDelta: "at least one EventThinkingDelta",
		EventActionDelta:   "at least one EventActionDelta",
		EventToolStart:     "at least one EventToolStart",
		EventToolResult:    "at least one EventToolResult",
		EventDone:          "EventDone at end",
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
	cancel() // pre-cancel

	eng := NewAgentEngine(p, r, "/test", false)
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

func TestRunStream_MaxTurns_ReceivesEventError(t *testing.T) {
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
			func(_ []schema.ToolDefinition) *schema.Message {
				return &schema.Message{
					Role: schema.RoleAssistant,
					ToolCalls: []schema.ToolCall{
						{ID: "c2", Name: "bash", Arguments: []byte("{}")},
					},
				}
			},
		},
	}
	r := &staticRegistry{output: "ok"}
	eng := NewAgentEngine(p, r, "/test", false, WithMaxTurns(1))

	stream, err := eng.RunStream(context.Background(), "loop")
	if err != nil {
		t.Fatalf("RunStream error: %v", err)
	}

	var errData string
	for _, evt := range collectEvents(stream) {
		if evt.Type == EventError {
			if s, ok := evt.Data.(string); ok {
				errData = s
			}
		}
	}
	if errData == "" {
		t.Fatal("expected EventError when MaxTurns exceeded")
	}
	if !strings.Contains(errData, "最大") {
		t.Errorf("error message should mention max turns, got: %q", errData)
	}
}

func TestRunStream_ToolStartBeforeToolResult(t *testing.T) {
	p := &countingProvider{
		responses: []func(tools []schema.ToolDefinition) *schema.Message{
			func(_ []schema.ToolDefinition) *schema.Message {
				return &schema.Message{
					Role: schema.RoleAssistant,
					ToolCalls: []schema.ToolCall{
						{ID: "c1", Name: "bash", Arguments: []byte(`{}`)},
					},
				}
			},
			func(_ []schema.ToolDefinition) *schema.Message {
				return &schema.Message{Role: schema.RoleAssistant, Content: "done"}
			},
		},
	}
	r := &staticRegistry{
		tools:  []schema.ToolDefinition{{Name: "bash"}},
		output: "hi",
	}
	eng := NewAgentEngine(p, r, "/test", false)

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
	eng := NewAgentEngine(p, r, "/test", false)

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
					Role: schema.RoleAssistant,
					ToolCalls: []schema.ToolCall{
						{ID: "c1", Name: "bash", Arguments: []byte(`{}`)},
					},
				}
			},
			func(_ []schema.ToolDefinition) *schema.Message {
				return &schema.Message{Role: schema.RoleAssistant, Content: "done"}
			},
		},
	}
	r := &staticRegistry{
		tools:  []schema.ToolDefinition{{Name: "bash"}},
		output: "tool output here",
	}
	eng := NewAgentEngine(p, r, "/test", false)

	stream, err := eng.RunStream(context.Background(), "test")
	if err != nil {
		t.Fatalf("RunStream error: %v", err)
	}

	for _, evt := range collectEvents(stream) {
		if evt.Type == EventToolResult {
			result, ok := evt.Data.(schema.ToolResult)
			if !ok {
				t.Fatalf("EventToolResult.Data should be schema.ToolResult, got %T", evt.Data)
			}
			if result.Output != "tool output here" {
				t.Errorf("expected tool output 'tool output here', got %q", result.Output)
			}
			return
		}
	}
	t.Fatal("expected EventToolResult event")
}
