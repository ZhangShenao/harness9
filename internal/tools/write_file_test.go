package tools

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestWriteFileTool_Name(t *testing.T) {
	tool := NewWriteFileTool("/tmp")
	if tool.Name() != "write_file" {
		t.Errorf("expected 'write_file', got %q", tool.Name())
	}
}

func TestWriteFileTool_Definition(t *testing.T) {
	tool := NewWriteFileTool("/tmp")
	def := tool.Definition()
	if def.Name != "write_file" {
		t.Errorf("definition name mismatch: %q", def.Name)
	}
	if def.Description == "" {
		t.Error("definition should have a description")
	}
	if def.InputSchema == nil {
		t.Error("definition should have an input schema")
	}
}

func TestWriteFileTool_Execute_Success(t *testing.T) {
	dir := t.TempDir()
	tool := NewWriteFileTool(dir)

	_, err := tool.Execute(context.Background(), json.RawMessage(`{"path":"out.txt","content":"hello"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, err := os.ReadFile(dir + "/out.txt")
	if err != nil {
		t.Fatalf("file should exist after write: %v", err)
	}
	if string(data) != "hello" {
		t.Errorf("file content mismatch: %q", data)
	}
}

func TestWriteFileTool_Execute_ReturnsFilePathAndByteCount(t *testing.T) {
	dir := t.TempDir()
	tool := NewWriteFileTool(dir)

	out, err := tool.Execute(context.Background(), json.RawMessage(`{"path":"result.txt","content":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "result.txt") {
		t.Errorf("success message should mention file path, got: %q", out)
	}
	if !strings.Contains(out, "5") { // len("hello") == 5
		t.Errorf("success message should contain byte count 5, got: %q", out)
	}
}

func TestWriteFileTool_Execute_AutoMkdir(t *testing.T) {
	dir := t.TempDir()
	tool := NewWriteFileTool(dir)

	_, err := tool.Execute(context.Background(), json.RawMessage(`{"path":"deep/nested/dir/file.txt","content":"nested"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, err := os.ReadFile(dir + "/deep/nested/dir/file.txt")
	if err != nil {
		t.Fatalf("nested file should exist: %v", err)
	}
	if string(data) != "nested" {
		t.Errorf("nested file content mismatch: %q", data)
	}
}

func TestWriteFileTool_Execute_Overwrite(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/file.txt", []byte("original"), 0644); err != nil {
		t.Fatal(err)
	}

	tool := NewWriteFileTool(dir)
	_, err := tool.Execute(context.Background(), json.RawMessage(`{"path":"file.txt","content":"overwritten"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, err := os.ReadFile(dir + "/file.txt")
	if err != nil {
		t.Fatalf("failed to read file after overwrite: %v", err)
	}
	if string(data) != "overwritten" {
		t.Errorf("expected overwritten content, got %q", data)
	}
}

func TestWriteFileTool_Execute_BadJSON(t *testing.T) {
	tool := NewWriteFileTool("/tmp")
	_, err := tool.Execute(context.Background(), json.RawMessage(`not_json`))
	if err == nil {
		t.Fatal("expected error for malformed JSON args")
	}
}
