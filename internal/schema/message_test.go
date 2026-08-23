// Package schema 核心类型的序列化契约测试。
//
// AGENTS.md 6.5 约定：schema.Message 的 JSON tag 使用 snake_case + omitempty 组合，
// 且该序列化格式会被 SQLiteSession 持久化（tool_calls 列）——一旦 tag 变更，
// 历史会话中的存量数据将无法反序列化。本测试把契约固化，防止无意中破坏向后兼容。
package schema

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestMessageJSONContract 验证 Message 序列化输出的字段名与 omitempty 行为。
func TestMessageJSONContract(t *testing.T) {
	t.Run("full_message_field_names", func(t *testing.T) {
		m := Message{
			Role:       RoleAssistant,
			Content:    "hello",
			ToolCalls:  []ToolCall{{ID: "call_1", Name: "bash", Arguments: json.RawMessage(`{"command":"ls"}`)}},
			ToolCallID: "call_1",
			IsError:    true,
		}
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		got := string(b)
		// snake_case 契约：字段名不得出现驼峰形式
		for _, camel := range []string{"toolCalls", "toolCallId", "toolCallID", "isError"} {
			if strings.Contains(got, camel) {
				t.Errorf("serialized field names must be snake_case, found %q in: %s", camel, got)
			}
		}
		for _, want := range []string{`"role"`, `"content"`, `"tool_calls"`, `"tool_call_id"`, `"is_error"`} {
			if !strings.Contains(got, want) {
				t.Errorf("missing expected field %s in: %s", want, got)
			}
		}
	})

	t.Run("omitempty_drops_empty_fields", func(t *testing.T) {
		m := Message{Role: RoleUser, Content: "hi"}
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		got := string(b)
		for _, absent := range []string{"tool_calls", "tool_call_id", "is_error"} {
			if strings.Contains(got, absent) {
				t.Errorf("empty field %s should be omitted by omitempty, got: %s", absent, got)
			}
		}
	})

	t.Run("roundtrip_preserves_tool_calls", func(t *testing.T) {
		// 验证 SQLiteSession 持久化路径依赖的 marshal→unmarshal 往返无损。
		m := Message{
			Role:      RoleAssistant,
			ToolCalls: []ToolCall{{ID: "c1", Name: "read_file", Arguments: json.RawMessage(`{"path":"a.go"}`)}},
		}
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var back Message
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if back.Role != RoleAssistant || len(back.ToolCalls) != 1 {
			t.Fatalf("roundtrip lost data: %+v", back)
		}
		if back.ToolCalls[0].ID != "c1" || back.ToolCalls[0].Name != "read_file" {
			t.Errorf("tool call roundtrip mismatch: %+v", back.ToolCalls[0])
		}
		if string(back.ToolCalls[0].Arguments) != `{"path":"a.go"}` {
			t.Errorf("arguments roundtrip mismatch: %s", back.ToolCalls[0].Arguments)
		}
	})
}

// TestToolCallArgumentsRaw 契约：Arguments 必须保持 json.RawMessage 延迟反序列化语义，
// 空参数不应在序列化时被丢弃（id/name/arguments 为必填 tag，无 omitempty）。
func TestToolCallArgumentsRaw(t *testing.T) {
	tc := ToolCall{ID: "c2", Name: "todo_write"}
	b, err := json.Marshal(tc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(b)
	if !strings.Contains(got, `"arguments"`) {
		t.Errorf("arguments field must always serialize (no omitempty), got: %s", got)
	}

	var back ToolCall
	if err := json.Unmarshal([]byte(`{"id":"c3","name":"bash","arguments":{"command":"go version"}}`), &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(back.Arguments) != `{"command":"go version"}` {
		t.Errorf("arguments must stay raw (unparsed), got: %s", back.Arguments)
	}
}

// TestStreamChunkTypeValues 验证流式 chunk 类型枚举值与 Provider 层产出的字符串一致。
// 这些字符串作为事件路由的 key，变更会导致引擎事件分发静默失败。
func TestStreamChunkTypeValues(t *testing.T) {
	cases := map[StreamChunkType]string{
		StreamChunkTextDelta:     "text_delta",
		StreamChunkThinkingDelta: "thinking_delta",
		StreamChunkDone:          "done",
		StreamChunkError:         "error",
	}
	for typ, want := range cases {
		if string(typ) != want {
			t.Errorf("StreamChunkType %q = %q, want %q", typ, typ, want)
		}
	}
}

// TestSubAgentUpdateKindValues 验证子代理进度更新枚举值与 TUI 侧 switch 分支一致。
func TestSubAgentUpdateKindValues(t *testing.T) {
	cases := map[SubAgentUpdateKind]string{
		SubAgentStart:      "start",
		SubAgentDelta:      "delta",
		SubAgentThinking:   "thinking",
		SubAgentToolStart:  "tool_start",
		SubAgentToolResult: "tool_result",
		SubAgentDone:       "done",
		SubAgentError:      "error",
	}
	for kind, want := range cases {
		if string(kind) != want {
			t.Errorf("SubAgentUpdateKind %q = %q, want %q", kind, kind, want)
		}
	}
}
