package tools

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestReadFileTool_Name(t *testing.T) {
	tool := NewReadFileTool("/tmp")
	if tool.Name() != "read_file" {
		t.Errorf("expected 'read_file', got %q", tool.Name())
	}
}

func TestReadFileTool_Definition(t *testing.T) {
	tool := NewReadFileTool("/tmp")
	def := tool.Definition()
	if def.Name != "read_file" {
		t.Errorf("definition name mismatch: %q", def.Name)
	}
	if def.Description == "" {
		t.Error("definition should have a description")
	}
	if def.InputSchema == nil {
		t.Error("definition should have an input schema")
	}
}

func TestReadFileTool_Execute_Success(t *testing.T) {
	dir := t.TempDir()
	content := "hello, world"
	if err := os.WriteFile(dir+"/test.txt", []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	tool := NewReadFileTool(dir)
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"path":"test.txt"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != content {
		t.Errorf("expected %q, got %q", content, out)
	}
}

func TestReadFileTool_Execute_Truncation(t *testing.T) {
	dir := t.TempDir()
	large := strings.Repeat("x", maxReadLen+100)
	if err := os.WriteFile(dir+"/big.txt", []byte(large), 0644); err != nil {
		t.Fatal(err)
	}

	tool := NewReadFileTool(dir)
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"path":"big.txt"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "截断") {
		t.Errorf("output should mention truncation, got length=%d", len(out))
	}
	if len([]byte(out)) < maxReadLen {
		t.Errorf("truncated output should still contain %d bytes of content", maxReadLen)
	}
}

func TestReadFileTool_Execute_FileNotFound(t *testing.T) {
	dir := t.TempDir()
	tool := NewReadFileTool(dir)

	_, err := tool.Execute(context.Background(), json.RawMessage(`{"path":"nonexistent.txt"}`))
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
}

func TestReadFileTool_Execute_BadJSON(t *testing.T) {
	tool := NewReadFileTool("/tmp")
	_, err := tool.Execute(context.Background(), json.RawMessage(`not_json`))
	if err == nil {
		t.Fatal("expected error for malformed JSON args")
	}
}

func TestReadFileTool_Execute_PathTraversal(t *testing.T) {
	dir := t.TempDir()
	tool := NewReadFileTool(dir)

	_, err := tool.Execute(context.Background(), json.RawMessage(`{"path":"../../etc/passwd"}`))
	if err == nil {
		t.Fatal("expected sandbox error for path traversal")
	}
	if !strings.Contains(err.Error(), "超出工作区范围") {
		t.Errorf("expected sandbox error, got: %v", err)
	}
}
