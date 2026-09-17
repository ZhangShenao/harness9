// glob 工具与 ** 模式匹配的表驱动测试。
package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeGlobTime 是测试用固定时间：保证 a.go 的 mtime 最新（降序排首）。
var fakeGlobTime = time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)

func TestMatchGlobPath(t *testing.T) {
	cases := []struct {
		rel, pattern string
		want         bool
	}{
		{"a.go", "*.go", true},
		{"dir/a.go", "*.go", false}, // 段内 * 不跨目录
		{"dir/a.go", "**/*.go", true},
		{"a/b/c/a.go", "**/*.go", true},
		{"a/b/c/a.go", "a/**/*.go", true},
		{"x/b/c/a.go", "a/**/*.go", false},
		{"a/b.txt", "**/*.go", false},
		{"ab/x.go", "a?/x.go", true},
		{"a/x.go", "a/*.go", true},
		{"a.go", "**/*.go", true}, // ** 匹配零层
	}
	for _, c := range cases {
		if got := MatchGlobPath(c.rel, c.pattern); got != c.want {
			t.Errorf("MatchGlobPath(%q, %q) = %v, want %v", c.rel, c.pattern, got, c.want)
		}
	}
}

func writeGlobFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := []string{
		"a.go", "b.txt",
		"internal/x.go", "internal/y.go",
		"internal/engine/z.go",
		".git/config",
	}
	for _, f := range files {
		p := filepath.Join(root, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// a.go 稍新，mtime 排序默认在前
	newTime := fakeGlobTime
	if err := os.Chtimes(filepath.Join(root, "a.go"), newTime, newTime); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestGlobToolExecute(t *testing.T) {
	root := writeGlobFixture(t)
	tool := NewGlobTool(root)

	out, err := tool.Execute(context.Background(),
		json.RawMessage(`{"pattern":"**/*.go"}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"a.go", "internal/x.go", "internal/engine/z.go"} {
		if !strings.Contains(out, want) {
			t.Fatalf("输出应含 %s:\n%s", want, out)
		}
	}
	if strings.Contains(out, ".git") {
		t.Fatal("不应匹配 .git 目录")
	}
	if !strings.Contains(out, "共") {
		t.Fatal("应输出总数统计")
	}

	// name 排序
	out, err = tool.Execute(context.Background(),
		json.RawMessage(`{"pattern":"internal/*.go","sort":"name"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "internal/x.go") || !strings.Contains(out, "internal/y.go") {
		t.Fatalf("输出不符:\n%s", out)
	}

	// 空结果
	out, err = tool.Execute(context.Background(), json.RawMessage(`{"pattern":"**/*.rs"}`))
	if err != nil || !strings.Contains(out, "没有匹配的文件") {
		t.Fatalf("空结果输出不符: %s err=%v", out, err)
	}
}

func TestGlobToolPathEscape(t *testing.T) {
	tool := NewGlobTool(t.TempDir())
	if _, err := tool.Execute(context.Background(),
		json.RawMessage(`{"pattern":"*.go","path":"../../etc"}`)); err == nil {
		t.Fatal("越界 path 应报错")
	}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{}`)); err == nil {
		t.Fatal("缺 pattern 应报错")
	}
}
