package logfmt

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/harness9/internal/schema"
)

// TestFormatJSON 验证 JSON 渲染的三类分支：空输入、短 payload 内联、长 payload pretty-print。
func TestFormatJSON(t *testing.T) {
	tests := []struct {
		name string
		raw  json.RawMessage
		want string
	}{
		{name: "empty", raw: nil, want: "{}"},
		{name: "short_inline", raw: json.RawMessage(`{"command":"ls"}`), want: `{"command":"ls"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FormatJSON(tt.raw)
			if got != tt.want {
				t.Errorf("FormatJSON(%s) = %q, want %q", string(tt.raw), got, tt.want)
			}
		})
	}
}

// TestFormatJSON_LongPayloadIsPretty 验证超过 argInlineThreshold 的 payload 被 pretty-print 并加缩进。
func TestFormatJSON_LongPayloadIsPretty(t *testing.T) {
	// 构造一个明显超过 80 字节阈值的 payload（压缩后约 130+ 字节）
	raw := json.RawMessage(`{"path":"src/very/long/file/path/segment/main.go","content":"package main\n\nfunc main() { println(42) }"}`)
	got := FormatJSON(raw)
	if !strings.Contains(got, "\n") {
		t.Fatalf("long payload should be multi-line, got: %q", got)
	}
	if !strings.HasPrefix(got, Indent) {
		t.Errorf("pretty output should start with Indent constant, got: %q", got[:min(len(got), 20)])
	}
}

// TestFormatJSON_NoHTMLEscape 验证 &、<、> 字符不被转义为 \uXXXX 形式。
// 这是日志可读性的关键 — 命令中常见的 && 必须原样显示，不能出现 & 这种 Unicode 转义。
func TestFormatJSON_NoHTMLEscape(t *testing.T) {
	raw := json.RawMessage(`{"cmd":"echo a && b < c > d"}`)
	got := FormatJSON(raw)

	// 不应出现 HTML 字符的 Unicode 转义形式（如 JSON 输出文本中的字面 6 字符 "\u0026"）
	for _, esc := range []string{"\\u0026", "\\u003c", "\\u003e"} {
		if strings.Contains(got, esc) {
			t.Errorf("FormatJSON should not emit %s, got: %q", esc, got)
		}
	}

	// 字面字符应保留
	if !strings.Contains(got, "&&") {
		t.Errorf("FormatJSON should preserve && literally, got: %q", got)
	}
	if !strings.Contains(got, "<") || !strings.Contains(got, ">") {
		t.Errorf("FormatJSON should preserve < and > literally, got: %q", got)
	}
}

// TestFormatOutput 验证基础输出渲染：单行、多行、空输入、超长截断。
func TestFormatOutput(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		body, total, truncated := FormatOutput("")
		if body != Indent+"│ <empty>" {
			t.Errorf("empty body should render as <empty> placeholder, got: %q", body)
		}
		if total != 0 || truncated {
			t.Errorf("empty: total=%d truncated=%v", total, truncated)
		}
	})

	t.Run("single_line", func(t *testing.T) {
		body, total, truncated := FormatOutput("hello")
		want := Indent + "│ hello"
		if body != want {
			t.Errorf("got %q, want %q", body, want)
		}
		if total != 5 || truncated {
			t.Errorf("single line: total=%d truncated=%v", total, truncated)
		}
	})

	t.Run("multi_line", func(t *testing.T) {
		body, _, _ := FormatOutput("a\nb\nc")
		lines := strings.Split(body, "\n")
		if len(lines) != 3 {
			t.Fatalf("expected 3 lines, got %d: %q", len(lines), body)
		}
		for _, ln := range lines {
			if !strings.HasPrefix(ln, Indent+"│ ") {
				t.Errorf("line should start with Indent + │, got: %q", ln)
			}
		}
	})

	t.Run("trailing_newline_trimmed", func(t *testing.T) {
		body, _, _ := FormatOutput("done\n\n\n")
		// 末尾换行应被 trim，避免出现孤立的 "│ " 空行
		if strings.HasSuffix(body, "│ ") {
			t.Errorf("trailing empty line should be trimmed, got: %q", body)
		}
	})

	t.Run("truncation", func(t *testing.T) {
		long := strings.Repeat("x", MaxOutputLen+100)
		body, total, truncated := FormatOutput(long)
		if total != MaxOutputLen+100 {
			t.Errorf("total should reflect pre-truncation size, got %d", total)
		}
		if !truncated {
			t.Error("oversized input should set truncated=true")
		}
		// body 截断后总字节数应不超过 MaxOutputLen + 前缀
		visible := strings.TrimPrefix(body, Indent+"│ ")
		if len(visible) > MaxOutputLen {
			t.Errorf("visible content should be ≤ MaxOutputLen, got %d", len(visible))
		}
	})

	t.Run("truncation_utf8_safe", func(t *testing.T) {
		// 中文每字符 3 字节：MaxOutputLen=512 不是 3 的倍数，
		// 字节截断必然把某个汉字切成两半产生无效 UTF-8。
		// 回归测试：截断结果必须仍是合法 UTF-8（修复前此用例失败）。
		long := strings.Repeat("中文输出测试", 200)
		body, _, truncated := FormatOutput(long)
		if !truncated {
			t.Fatal("oversized CJK input should set truncated=true")
		}
		visible := strings.TrimSuffix(strings.TrimPrefix(body, Indent+"│ "), "\n")
		if !utf8.ValidString(visible) {
			// 直接打印整个 visible（%q），避免 len(visible) < 8 时尾部切片导致测试自身 panic
			t.Errorf("truncated CJK output should remain valid UTF-8, got invalid bytes: %q", visible)
		}
	})
}

// TestFormatToolStart 验证工具启动条目的头部 + arguments 渲染。
func TestFormatToolStart(t *testing.T) {
	tc := schema.ToolCall{
		ID:        "call_abc",
		Name:      "bash",
		Arguments: json.RawMessage(`{"command":"ls"}`),
	}
	got := FormatToolStart("engine", 1, tc)
	if !strings.Contains(got, "[engine] Turn 1 │ 工具启动 │ tool=bash id=call_abc") {
		t.Errorf("header missing or malformed: %q", got)
	}
	if !strings.Contains(got, `arguments: {"command":"ls"}`) {
		t.Errorf("short arguments should be inlined: %q", got)
	}
}

// TestFormatToolDone 验证 status 与耗时格式化、成功/失败标签切换、截断后缀。
func TestFormatToolDone(t *testing.T) {
	tc := schema.ToolCall{ID: "call_1", Name: "bash"}

	t.Run("success", func(t *testing.T) {
		result := schema.ToolResult{Output: "ok"}
		got := FormatToolDone("engine", 1, tc, result, 50*time.Millisecond)
		if !strings.Contains(got, "工具完成") {
			t.Errorf("success should use 工具完成 label: %q", got)
		}
		if !strings.Contains(got, "status=ok") {
			t.Errorf("success status missing: %q", got)
		}
		if strings.Contains(got, "truncated to") {
			t.Errorf("short output should not show truncation suffix: %q", got)
		}
	})

	t.Run("failure", func(t *testing.T) {
		result := schema.ToolResult{Output: "permission denied", IsError: true}
		got := FormatToolDone("engine-stream", 2, tc, result, time.Second)
		if !strings.Contains(got, "工具失败") {
			t.Errorf("failure should use 工具失败 label: %q", got)
		}
		if !strings.Contains(got, "status=error") {
			t.Errorf("failure status missing: %q", got)
		}
		if !strings.Contains(got, "[engine-stream]") {
			t.Errorf("logPrefix should propagate: %q", got)
		}
	})

	t.Run("truncated", func(t *testing.T) {
		result := schema.ToolResult{Output: strings.Repeat("x", MaxOutputLen+50)}
		got := FormatToolDone("engine", 1, tc, result, time.Millisecond)
		if !strings.Contains(got, "truncated to") {
			t.Errorf("oversized output should show truncation suffix: %q", got)
		}
	})
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestFormatMsg 验证通用单行日志条目的前缀拼接格式。
func TestFormatMsg(t *testing.T) {
	got := FormatMsg("server", "RunStream 启动失败：timeout")
	want := "[server] RunStream 启动失败：timeout"
	if got != want {
		t.Errorf("FormatMsg = %q, want %q", got, want)
	}
}

// TestFormatLoopStart 验证启动日志包含所有关键参数字段。
func TestFormatLoopStart(t *testing.T) {
	got := FormatLoopStart("engine", "/work", 50, 60*time.Second, 4)
	for _, want := range []string{"[engine]", "workdir=/work", "maxTurns=50", "toolTimeout=1m0s", "maxConcurrent=4"} {
		if !strings.Contains(got, want) {
			t.Errorf("FormatLoopStart missing %q in: %q", want, got)
		}
	}
}

// TestFormatTurnStart 验证 Turn 头部日志包含 turn 编号、history 和 tools 字段。
func TestFormatTurnStart(t *testing.T) {
	got := FormatTurnStart("engine-stream", 3, 7, 4)
	for _, want := range []string{"[engine-stream]", "Turn 3", "history=7", "tools=4"} {
		if !strings.Contains(got, want) {
			t.Errorf("FormatTurnStart missing %q in: %q", want, got)
		}
	}
}

// TestFormatLoopEnd 验证结束日志包含 turns 和 total 字段。
func TestFormatLoopEnd(t *testing.T) {
	got := FormatLoopEnd("engine", 5, 3*time.Second)
	for _, want := range []string{"[engine]", "turns=5", "total=3s"} {
		if !strings.Contains(got, want) {
			t.Errorf("FormatLoopEnd missing %q in: %q", want, got)
		}
	}
}
