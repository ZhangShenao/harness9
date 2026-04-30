package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/harness9/internal/schema"
)

// testTool is a configurable BaseTool stub for registry tests.
type testTool struct {
	name   string
	output string
	err    error
}

func (t *testTool) Name() string { return t.name }

func (t *testTool) Definition() schema.ToolDefinition {
	return schema.ToolDefinition{Name: t.name, Description: "test tool"}
}

func (t *testTool) Execute(_ context.Context, _ json.RawMessage) (string, error) {
	return t.output, t.err
}

func TestNewRegistry_IsEmpty(t *testing.T) {
	r := NewRegistry()
	if defs := r.GetAvailableTools(); len(defs) != 0 {
		t.Fatalf("new registry should be empty, got %d tools", len(defs))
	}
}

func TestRegistry_Register_AddsDefinition(t *testing.T) {
	r := NewRegistry()
	r.Register(&testTool{name: "mytool"})
	defs := r.GetAvailableTools()
	if len(defs) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(defs))
	}
	if defs[0].Name != "mytool" {
		t.Errorf("expected 'mytool', got %q", defs[0].Name)
	}
}

func TestRegistry_Register_Overwrite(t *testing.T) {
	r := NewRegistry()
	r.Register(&testTool{name: "tool", output: "first"})
	r.Register(&testTool{name: "tool", output: "second"})

	result := r.Execute(context.Background(), schema.ToolCall{
		ID: "1", Name: "tool", Arguments: json.RawMessage(`{}`),
	})
	if result.Output != "second" {
		t.Errorf("overwrite: expected 'second', got %q", result.Output)
	}
}

func TestRegistry_GetAvailableTools_MultipleTools(t *testing.T) {
	r := NewRegistry()
	r.Register(&testTool{name: "bash"})
	r.Register(&testTool{name: "read_file"})
	r.Register(&testTool{name: "write_file"})
	if got := len(r.GetAvailableTools()); got != 3 {
		t.Fatalf("expected 3 tools, got %d", got)
	}
}

func TestRegistry_Execute_Success(t *testing.T) {
	r := NewRegistry()
	r.Register(&testTool{name: "echo", output: "hello"})

	result := r.Execute(context.Background(), schema.ToolCall{
		ID: "call_1", Name: "echo", Arguments: json.RawMessage(`{}`),
	})

	if result.IsError {
		t.Fatalf("expected no error, got: %s", result.Output)
	}
	if result.Output != "hello" {
		t.Errorf("expected 'hello', got %q", result.Output)
	}
	if result.ToolCallID != "call_1" {
		t.Errorf("expected ToolCallID 'call_1', got %q", result.ToolCallID)
	}
}

func TestRegistry_Execute_ToolNotFound(t *testing.T) {
	r := NewRegistry()

	result := r.Execute(context.Background(), schema.ToolCall{
		ID: "call_1", Name: "ghost_tool",
	})

	if !result.IsError {
		t.Fatal("expected IsError=true for nonexistent tool")
	}
	if !strings.Contains(result.Output, "ghost_tool") {
		t.Errorf("error message should mention tool name, got: %s", result.Output)
	}
	if result.ToolCallID != "call_1" {
		t.Errorf("ToolCallID should be preserved, got %q", result.ToolCallID)
	}
}

func TestRegistry_Execute_ToolExecutionError(t *testing.T) {
	r := NewRegistry()
	r.Register(&testTool{name: "broken", err: fmt.Errorf("disk full")})

	result := r.Execute(context.Background(), schema.ToolCall{
		ID: "call_2", Name: "broken", Arguments: json.RawMessage(`{}`),
	})

	if !result.IsError {
		t.Fatal("expected IsError=true when tool.Execute returns error")
	}
	if !strings.Contains(result.Output, "disk full") {
		t.Errorf("error output should contain error message, got: %s", result.Output)
	}
}
