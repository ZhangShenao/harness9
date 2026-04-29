// read_file 工具单元测试。
//
// 分两组：
//   - TestTruncateAtRuneBoundary —— 独立覆盖 UTF-8 回退逻辑，和 IO / 沙箱解耦
//   - TestReadFileTool_* —— 整体行为：offset / limit / EOF / 截断提示 /
//     沙箱边界 / 不存在的文件 / 二进制文件
//
// 基础行为（safePath、不存在文件、二进制）虽不是本轮新加，但当前 0 覆盖，
// 顺手补上，防止退化。
package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// ============================================================================
// 独立的 rune 边界回退测试
// ============================================================================

// TestTruncateAtRuneBoundary 独立验证 UTF-8 rune 边界回退。
// 用中文（每字符 3 字节）构造切在 rune 中间的情况。
func TestTruncateAtRuneBoundary(t *testing.T) {
	// "你好世界" 是 4 个中文字符 = 12 字节（每个 3 字节）
	s := []byte("你好世界")
	if len(s) != 12 {
		t.Fatalf("前置假设错：'你好世界' 预期 12 字节，实际 %d", len(s))
	}

	tests := []struct {
		name      string
		limit     int
		wantBytes int
		wantStr   string
	}{
		{name: "limit 很大不截断", limit: 100, wantBytes: 12, wantStr: "你好世界"},
		{name: "limit 恰等于长度", limit: 12, wantBytes: 12, wantStr: "你好世界"},
		{name: "limit 落在第 1 字符中间", limit: 2, wantBytes: 0, wantStr: ""},
		{name: "limit 落在第 1 字符末尾", limit: 3, wantBytes: 3, wantStr: "你"},
		{name: "limit 落在第 2 字符中间", limit: 4, wantBytes: 3, wantStr: "你"},
		{name: "limit 落在第 2 字符末尾", limit: 6, wantBytes: 6, wantStr: "你好"},
		{name: "limit 落在第 3 字符中间 (byte 7)", limit: 7, wantBytes: 6, wantStr: "你好"},
		{name: "limit 落在第 3 字符末尾 (byte 9)", limit: 9, wantBytes: 9, wantStr: "你好世"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncateAtRuneBoundary(s, tt.limit)
			if len(got) != tt.wantBytes {
				t.Fatalf("bytes: got %d, want %d", len(got), tt.wantBytes)
			}
			if string(got) != tt.wantStr {
				t.Fatalf("string: got %q, want %q", got, tt.wantStr)
			}
			if !utf8.Valid(got) {
				t.Fatalf("回退后应是合法 UTF-8，实际 invalid: %q", got)
			}
		})
	}

	// 纯 ASCII 情形：每字节都是 rune 起点，limit 应原样生效。
	ascii := []byte("abcdefghij")
	got := truncateAtRuneBoundary(ascii, 5)
	if string(got) != "abcde" {
		t.Fatalf("ASCII: got %q, want 'abcde'", got)
	}
}

// ============================================================================
// ReadFileTool 端到端行为
// ============================================================================

// readFileToolAt 构造一个 tool + tmpdir，返回 tool、tmpdir、清理函数。
func readFileToolAt(t *testing.T, opts ...ReadFileOption) (*ReadFileTool, string) {
	t.Helper()
	dir := t.TempDir()
	tool := NewReadFileTool(dir, opts...)
	return tool, dir
}

// invokeRead 封装工具调用：按 readFileArgs 构造 JSON 并执行，返回 (output, err)。
func invokeRead(t *testing.T, tool *ReadFileTool, args readFileArgs) (string, error) {
	t.Helper()
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return tool.Execute(context.Background(), b)
}

// TestReadFileTool_Basic 完整读取一个小文件。
func TestReadFileTool_Basic(t *testing.T) {
	tool, dir := readFileToolAt(t)
	path := filepath.Join(dir, "hello.txt")
	if err := os.WriteFile(path, []byte("hello harness9"), 0644); err != nil {
		t.Fatal(err)
	}
	out, err := invokeRead(t, tool, readFileArgs{Path: "hello.txt"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if out != "hello harness9" {
		t.Fatalf("got %q", out)
	}
}

// TestReadFileTool_Offset 从指定偏移读取，应带 header 显示 offset 和文件大小。
func TestReadFileTool_Offset(t *testing.T) {
	tool, dir := readFileToolAt(t)
	path := filepath.Join(dir, "data.txt")
	if err := os.WriteFile(path, []byte("0123456789ABCDEF"), 0644); err != nil {
		t.Fatal(err)
	}
	out, err := invokeRead(t, tool, readFileArgs{Path: "data.txt", Offset: 10})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "offset=10") {
		t.Fatalf("期望 header 含 offset=10, got %q", out)
	}
	if !strings.Contains(out, "ABCDEF") {
		t.Fatalf("期望内容含 ABCDEF, got %q", out)
	}
}

// TestReadFileTool_OffsetBeyondEOF offset 超过文件大小应返回明确提示而不是 error。
func TestReadFileTool_OffsetBeyondEOF(t *testing.T) {
	tool, dir := readFileToolAt(t)
	path := filepath.Join(dir, "small.txt")
	if err := os.WriteFile(path, []byte("abc"), 0644); err != nil {
		t.Fatal(err)
	}
	out, err := invokeRead(t, tool, readFileArgs{Path: "small.txt", Offset: 99})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !strings.Contains(out, "offset=99") || !strings.Contains(out, "无内容可读") {
		t.Fatalf("期望带 offset 提示和'无内容可读'，got %q", out)
	}
}

// TestReadFileTool_Truncated 超过 limit 时应截断并提示下一个 offset。
func TestReadFileTool_Truncated(t *testing.T) {
	tool, dir := readFileToolAt(t, WithMaxReadLen(10))
	path := filepath.Join(dir, "long.txt")
	if err := os.WriteFile(path, []byte("0123456789ABCDEFGHIJ"), 0644); err != nil {
		t.Fatal(err)
	}
	out, err := invokeRead(t, tool, readFileArgs{Path: "long.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "已截断") {
		t.Fatalf("期望含'已截断'提示, got %q", out)
	}
	// 下一个 offset 应该指向 10（前 10 字节纯 ASCII，rune 边界不回退）
	if !strings.Contains(out, "offset=10") {
		t.Fatalf("期望下一个 offset=10, got %q", out)
	}
}

// TestReadFileTool_TruncatedUTF8 验证 limit 恰好切在多字节字符中间时，
// 实际返回长度 < limit 且是合法 UTF-8，nextOffset 基于实际返回长度。
func TestReadFileTool_TruncatedUTF8(t *testing.T) {
	tool, dir := readFileToolAt(t, WithMaxReadLen(7))
	path := filepath.Join(dir, "zh.txt")
	// 每个中文字符 3 字节，内容 = "你好世界AB" = 3*4 + 2 = 14 字节
	content := []byte("你好世界AB")
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}
	out, err := invokeRead(t, tool, readFileArgs{Path: "zh.txt"})
	if err != nil {
		t.Fatal(err)
	}
	// limit=7 落在第 3 个中文字符中间 (byte 7 处)，回退后应返回前 2 个中文 = 6 字节。
	if !strings.Contains(out, "你好") {
		t.Fatalf("期望返回'你好', got %q", out)
	}
	if strings.Contains(out, "世") || strings.Contains(out, "界") {
		t.Fatalf("第 3/4 个中文不应出现，got %q", out)
	}
	if !strings.Contains(out, "offset=6") {
		t.Fatalf("期望下一次 offset=6 (UTF-8 回退后实际返回 6 字节), got %q", out)
	}
	if !utf8.ValidString(out) {
		t.Fatalf("输出应为合法 UTF-8, got %q", out)
	}
}

// TestReadFileTool_LimitParam 用户显式传入 limit，且 limit < maxReadLen 时生效。
func TestReadFileTool_LimitParam(t *testing.T) {
	tool, dir := readFileToolAt(t, WithMaxReadLen(1000))
	path := filepath.Join(dir, "x.txt")
	if err := os.WriteFile(path, []byte("0123456789"), 0644); err != nil {
		t.Fatal(err)
	}
	out, err := invokeRead(t, tool, readFileArgs{Path: "x.txt", Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "012") {
		t.Fatalf("期望读到 '012', got %q", out)
	}
	if !strings.Contains(out, "已截断") {
		t.Fatalf("期望截断提示, got %q", out)
	}
}

// TestReadFileTool_NegativeOffset 负偏移应返回 Go error（与 IsError Observation 区分）。
func TestReadFileTool_NegativeOffset(t *testing.T) {
	tool, dir := readFileToolAt(t)
	path := filepath.Join(dir, "x.txt")
	if err := os.WriteFile(path, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := invokeRead(t, tool, readFileArgs{Path: "x.txt", Offset: -1})
	if err == nil {
		t.Fatal("期望返回 error，实际 nil")
	}
}

// TestReadFileTool_SandboxEscape safePath 应挡住 ../../ 之类逃逸尝试。
// 本轮 PR 没改 safePath 但新增的 offset/limit 必须在沙箱之内才有意义 ——
// 顺手补这条回归。
func TestReadFileTool_SandboxEscape(t *testing.T) {
	tool, _ := readFileToolAt(t)
	_, err := invokeRead(t, tool, readFileArgs{Path: "../../etc/passwd"})
	if err == nil {
		t.Fatal("期望 safePath 拒绝路径逃逸，实际放行")
	}
}

// TestReadFileTool_FileNotFound 不存在的文件应返回 error（打开失败）。
func TestReadFileTool_FileNotFound(t *testing.T) {
	tool, _ := readFileToolAt(t)
	_, err := invokeRead(t, tool, readFileArgs{Path: "nope.txt"})
	if err == nil {
		t.Fatal("期望不存在的文件返回 error，实际 nil")
	}
}

// TestReadFileTool_BinaryFile 二进制文件在 limit 内应原样返回（字节透明）。
// 超出 limit 的二进制文件可能无法按 rune 回退 —— 本工具不保证二进制文件的
// rune 合法性，这条测试只验证小二进制文件不会被 UTF-8 逻辑破坏。
func TestReadFileTool_BinaryFile(t *testing.T) {
	tool, dir := readFileToolAt(t)
	path := filepath.Join(dir, "bin.dat")
	data := []byte{0x00, 0x01, 0xFF, 0xFE, 0x42, 0x43}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
	out, err := invokeRead(t, tool, readFileArgs{Path: "bin.dat"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(out) != len(data) {
		t.Fatalf("长度不符: got %d, want %d", len(out), len(data))
	}
}
