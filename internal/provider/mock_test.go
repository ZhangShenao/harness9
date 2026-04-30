package provider

import (
	"context"
	"testing"

	"github.com/harness9/internal/schema"
)

func TestMockProvider_ThinkingPhase_ReturnsContent(t *testing.T) {
	p := NewMockProvider()
	msg, err := p.Generate(context.Background(), nil, nil) // tools=nil → thinking
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if msg.Role != schema.RoleAssistant {
		t.Errorf("expected RoleAssistant, got %q", msg.Role)
	}
	if msg.Content == "" {
		t.Error("thinking response should have content")
	}
	if len(msg.ToolCalls) != 0 {
		t.Error("thinking response should not have tool calls")
	}
}

func TestMockProvider_Turn1_RequestsToolCall(t *testing.T) {
	p := NewMockProvider()
	tools := []schema.ToolDefinition{{Name: "bash"}}

	msg, err := p.Generate(context.Background(), nil, tools)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(msg.ToolCalls) == 0 {
		t.Fatal("first action turn should request a tool call")
	}
	if msg.ToolCalls[0].Name != "bash" {
		t.Errorf("expected bash tool call, got %q", msg.ToolCalls[0].Name)
	}
}

func TestMockProvider_Turn2_ReturnsFinalReply(t *testing.T) {
	p := NewMockProvider()
	tools := []schema.ToolDefinition{{Name: "bash"}}

	if _, err := p.Generate(context.Background(), nil, tools); err != nil { // turn 1
		t.Fatalf("unexpected error on turn 1: %v", err)
	}
	msg, err := p.Generate(context.Background(), nil, tools) // turn 2
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(msg.ToolCalls) != 0 {
		t.Error("second turn should be final reply with no tool calls")
	}
	if msg.Content == "" {
		t.Error("final reply should have content")
	}
}

func TestMockProvider_ThinkingPhase_DoesNotIncrementTurnCounter(t *testing.T) {
	p := NewMockProvider()
	tools := []schema.ToolDefinition{{Name: "bash"}}

	// Multiple thinking calls (tools=nil) must not advance the action turn counter.
	for i := 0; i < 3; i++ {
		if _, err := p.Generate(context.Background(), nil, nil); err != nil {
			t.Fatalf("unexpected error on thinking call %d: %v", i, err)
		}
	}

	// First action call should still trigger turn 1 (tool call request).
	msg, err := p.Generate(context.Background(), nil, tools)
	if err != nil {
		t.Fatal(err)
	}
	if len(msg.ToolCalls) == 0 {
		t.Error("after thinking-only calls, first action turn should still request a tool call")
	}
}

func TestMockProvider_GenerateStream_EndsWithDoneChunk(t *testing.T) {
	p := NewMockProvider()
	tools := []schema.ToolDefinition{{Name: "bash"}}

	stream, err := p.GenerateStream(context.Background(), nil, tools)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var lastChunk schema.StreamChunk
	for chunk := range stream {
		lastChunk = chunk
	}

	if lastChunk.Type != schema.StreamChunkDone {
		t.Errorf("last chunk should be Done, got %q", lastChunk.Type)
	}
	if lastChunk.Message == nil {
		t.Fatal("Done chunk must carry complete message")
	}
}

func TestMockProvider_GenerateStream_ThinkingPhaseHasTextDelta(t *testing.T) {
	p := NewMockProvider()

	stream, err := p.GenerateStream(context.Background(), nil, nil) // thinking
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var sawTextDelta bool
	for chunk := range stream {
		if chunk.Type == schema.StreamChunkTextDelta {
			sawTextDelta = true
		}
	}
	if !sawTextDelta {
		t.Error("thinking phase should emit at least one TextDelta chunk")
	}
}

func TestMockProvider_GenerateStream_Turn1HasToolCallChunks(t *testing.T) {
	p := NewMockProvider()
	tools := []schema.ToolDefinition{{Name: "bash"}}

	stream, err := p.GenerateStream(context.Background(), nil, tools)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var sawToolCallStart bool
	for chunk := range stream {
		if chunk.Type == schema.StreamChunkToolCallStart {
			sawToolCallStart = true
		}
	}
	if !sawToolCallStart {
		t.Error("first action turn stream should contain ToolCallStart chunk")
	}
}
