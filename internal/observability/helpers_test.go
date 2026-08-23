// helpers_test.go — Span 属性净化与序列化辅助函数的白盒单元测试。
// 与其他 *_test.go（外部包 observability_test）不同，本文件使用同包（package observability）
// 以访问未导出的 truncateAttr / serializeMessages / serializeOutput。
package observability

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/harness9/internal/schema"
)

// TestTruncateAttr 验证 OTEL Span 属性的净化与截断约束。
func TestTruncateAttr(t *testing.T) {
	t.Run("short_passthrough", func(t *testing.T) {
		got := truncateAttr("普通短文本")
		if got != "普通短文本" {
			t.Errorf("short input should pass through, got %q", got)
		}
	})

	t.Run("invalid_utf8_sanitized", func(t *testing.T) {
		// \xff 是非法 UTF-8 字节，必须被剔除以满足 OTLP protobuf 序列化要求
		got := truncateAttr("bad\xffbyte")
		if !utf8.ValidString(got) {
			t.Errorf("output must be valid UTF-8, got %q", got)
		}
		if got != "badbyte" {
			t.Errorf("invalid bytes should be dropped (not replaced), got %q", got)
		}
	})

	t.Run("oversize_truncated_at_rune_boundary", func(t *testing.T) {
		// 中文 3 字节/字：超出 maxSpanAttrLen 后必须在 rune 边界截断
		long := strings.Repeat("汉", maxSpanAttrLen) // 3x 超限
		got := truncateAttr(long)
		if !utf8.ValidString(got) {
			t.Errorf("truncated output must be valid UTF-8, got tail %q", got[len(got)-6:])
		}
		if !strings.HasSuffix(got, "…（已截断）") {
			t.Errorf("truncated output should carry suffix marker, got tail %q", got[len(got)-16:])
		}
	})

	t.Run("exact_boundary_not_truncated", func(t *testing.T) {
		exact := strings.Repeat("a", maxSpanAttrLen)
		got := truncateAttr(exact)
		if got != exact {
			t.Errorf("input exactly at limit should not be truncated, got len %d", len(got))
		}
	})
}

// TestSerializeMessages 验证 langfuse.input 属性的消息序列化视图。
func TestSerializeMessages(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		if got := serializeMessages(nil); got != "[]" {
			t.Errorf("nil messages should serialize to [], got %q", got)
		}
	})

	t.Run("fields_and_toolcalls", func(t *testing.T) {
		msgs := []schema.Message{
			{Role: schema.RoleSystem, Content: "sys"},
			{Role: schema.RoleAssistant, ToolCalls: []schema.ToolCall{
				{ID: "c1", Name: "bash", Arguments: json.RawMessage(`{"command":"ls"}`)},
			}},
			{Role: schema.RoleUser, Content: "obs", ToolCallID: "c1"},
		}
		got := serializeMessages(msgs)
		var views []map[string]any
		if err := json.Unmarshal([]byte(got), &views); err != nil {
			t.Fatalf("output must be valid JSON array: %v\n%s", err, got)
		}
		if len(views) != 3 {
			t.Fatalf("expected 3 views, got %d", len(views))
		}
		if views[0]["role"] != "system" || views[0]["content"] != "sys" {
			t.Errorf("view[0] mismatch: %+v", views[0])
		}
		if views[2]["tool_call_id"] != "c1" {
			t.Errorf("tool_call_id should be preserved: %+v", views[2])
		}
		calls, ok := views[1]["tool_calls"].([]any)
		if !ok || len(calls) != 1 {
			t.Fatalf("tool_calls view missing: %s", got)
		}
	})
}

// TestSerializeOutput 验证 langfuse.output 属性的响应序列化。
func TestSerializeOutput(t *testing.T) {
	t.Run("nil_message", func(t *testing.T) {
		if got := serializeOutput(nil); got != "" {
			t.Errorf("nil message should serialize to empty string, got %q", got)
		}
	})

	t.Run("text_content", func(t *testing.T) {
		got := serializeOutput(&schema.Message{Content: "final answer"})
		if got != "final answer" {
			t.Errorf("plain content mismatch: %q", got)
		}
	})

	t.Run("tool_calls_preferred_over_text", func(t *testing.T) {
		// 同时有文本与工具调用时，工具调用列表优先（对齐 Langfuse 的 observation 展示语义）
		msg := &schema.Message{
			Content:   "thinking aloud",
			ToolCalls: []schema.ToolCall{{ID: "c9", Name: "read_file", Arguments: json.RawMessage(`{"path":"x"}`)}},
		}
		got := serializeOutput(msg)
		if !strings.HasPrefix(got, "[{") {
			t.Errorf("tool calls should serialize as JSON array, got %q", got)
		}
		if !strings.Contains(got, `"read_file"`) {
			t.Errorf("tool name missing in serialized calls: %q", got)
		}
	})
}
