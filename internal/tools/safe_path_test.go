package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestSafePath_BlocksTraversal 验证 safePath 拒绝路径遍历攻击。
func TestSafePath_BlocksTraversal(t *testing.T) {
	workDir := "/Users/zsa/Desktop/harness/harness9"
	cases := []string{
		"../../etc/passwd",
		"../escaped.txt",
		"src/../../escaped.txt",
	}
	for _, p := range cases {
		_, err := safePath(workDir, p)
		if err == nil {
			t.Errorf("safePath(%q, %q) should have returned error", workDir, p)
		}
	}
}

// TestSafePath_AllowsInsideWorkDir 验证 safePath 接受合法路径。
func TestSafePath_AllowsInsideWorkDir(t *testing.T) {
	workDir := "/project"
	cases := map[string]string{
		"src/main.go": "/project/src/main.go",
		"./README.md": "/project/README.md",
		"a/b/c.txt":   "/project/a/b/c.txt",
		"":            "/project",
	}
	for in, want := range cases {
		got, err := safePath(workDir, in)
		if err != nil {
			t.Errorf("safePath(%q, %q) unexpected error: %v", workDir, in, err)
			continue
		}
		if got != want {
			t.Errorf("safePath(%q, %q) = %q, want %q", workDir, in, got, want)
		}
	}
}

// TestWriteFileTool_RejectsPathTraversal 验证 WriteFileTool 拒绝逃逸工作区的写入请求。
func TestWriteFileTool_RejectsPathTraversal(t *testing.T) {
	tmp := t.TempDir()
	tool := NewWriteFileTool(tmp)

	args := json.RawMessage(`{"path":"../escaped.txt","content":"pwned"}`)
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected error for traversal path, got nil")
	}
	if !strings.Contains(err.Error(), "超出工作区范围") {
		t.Fatalf("expected sandbox error, got: %v", err)
	}
}
