// grep 工具表驱动测试：正则、大小写、glob 过滤、二进制跳过、截断与超限标记。
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

func writeGrepFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"a.go":        "package main\n\nfunc Hello() {\n\treturn\n}\n",
		"b.txt":       "hello world\nHELLO AGAIN\n",
		"sub/c.go":    "func Hello() {}\n// hello comment\n",
		"sub/bin.dat": "text\x00binary\x00data hello\n",
	}
	for f, content := range files {
		p := filepath.Join(root, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestGrepToolBasic(t *testing.T) {
	root := writeGrepFixture(t)
	tool := NewGrepTool(root)

	out, err := tool.Execute(context.Background(), json.RawMessage(`{"pattern":"Hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "a.go:3:") || !strings.Contains(out, "sub/c.go:1:") {
		t.Fatalf("输出应含两处 Hello（a.go:3 与 sub/c.go:1）:\n%s", out)
	}
	if strings.Contains(out, "bin.dat") {
		t.Fatal("二进制文件应被跳过")
	}
	if !strings.Contains(out, "个文件") {
		t.Fatal("应有命中统计")
	}
}

func TestGrepToolIgnoreCaseAndGlobFilter(t *testing.T) {
	root := writeGrepFixture(t)
	tool := NewGrepTool(root)

	out, err := tool.Execute(context.Background(),
		json.RawMessage(`{"pattern":"hello","ignore_case":true,"glob":"*.txt"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "b.txt:1:") || !strings.Contains(out, "b.txt:2:") {
		t.Fatalf("ignore_case + glob 过滤输出不符:\n%s", out)
	}
	if strings.Contains(out, "a.go") {
		t.Fatal("glob 过滤后不应含 a.go")
	}
}

func TestGrepToolTruncationAndLimits(t *testing.T) {
	root := writeGrepFixture(t)
	tool := NewGrepTool(root)

	out, err := tool.Execute(context.Background(),
		json.RawMessage(`{"pattern":"hello","ignore_case":true,"max_results":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "已截断") {
		t.Fatalf("超限应标注截断:\n%s", out)
	}

	// 无效正则 → 工具级错误
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"pattern":"["}`)); err == nil {
		t.Fatal("无效正则应报错")
	}
	// 越界 path
	if _, err := tool.Execute(context.Background(),
		json.RawMessage(`{"pattern":"x","path":"../../etc"}`)); err == nil {
		t.Fatal("越界 path 应报错")
	}
}

// Task 14 移交修复：needle 前置到行首（前 7 runes），保证命中行输出真实触发
// 500-rune 截断——原 fixture 把 needle 放在 rune 1001+，截断后命中内容整体
// 被裁掉，断言恒不触发（空转）。
func TestGrepToolLineTruncateUTF8(t *testing.T) {
	root := t.TempDir()
	long := "needle " + strings.Repeat("界", 1000) // needle 在前 7 runes
	if err := os.WriteFile(filepath.Join(root, "long.txt"), []byte(long), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := NewGrepTool(root)
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"pattern":"needle"}`))
	if err != nil {
		t.Fatal(err)
	}
	line := strings.Split(out, "\n")[0]
	if !strings.HasPrefix(line, "long.txt:1: needle ") {
		t.Fatalf("首行应为命中行 long.txt:1: needle …，实际: %q", truncateForLog(line))
	}
	content := strings.TrimPrefix(line, "long.txt:1: ")
	r := []rune(content)
	if len(r) != 501 || r[500] != '…' {
		t.Fatalf("应截断为 500 runes + …，实际 %d runes", len(r))
	}
	if !utf8.ValidString(line) {
		t.Fatal("截断后应保持 UTF-8 有效")
	}
}

// truncateForLog 输出断言消息用的短预览（避免整行打爆测试输出）。
func truncateForLog(s string) string {
	r := []rune(s)
	if len(r) <= 40 {
		return s
	}
	return string(r[:40]) + "…"
}

// I1：root 不存在时 WalkDir fail-open 会吞掉根级错误、误报"没有命中"，
// 应在遍历前显式校验并返回 Go error。
func TestGrepToolRootNotExist(t *testing.T) {
	tool := NewGrepTool(t.TempDir())
	if _, err := tool.Execute(context.Background(),
		json.RawMessage(`{"pattern":"x","path":"no/such/dir"}`)); err == nil {
		t.Fatal("root 不存在应报错")
	}
}
