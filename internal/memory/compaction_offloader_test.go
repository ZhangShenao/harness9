package memory_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/harness9/internal/memory"
	"github.com/harness9/internal/schema"
)

func TestCompactionOffloader_WritesFileAndReturnsPlaceholder(t *testing.T) {
	dir := t.TempDir()
	o := memory.NewCompactionOffloader(dir, "sess1")

	large := strings.Repeat("line\n", 1000)
	msg := schema.Message{Role: schema.RoleUser, Content: large, ToolCallID: "tc_001"}

	entry, placeholder, err := o.OffloadToolResult(msg)
	if err != nil {
		t.Fatalf("offload failed: %v", err)
	}
	if entry.ToolCallID != "tc_001" {
		t.Errorf("ToolCallID: want tc_001, got %s", entry.ToolCallID)
	}
	if entry.Bytes != len(large) {
		t.Errorf("Bytes: want %d, got %d", len(large), entry.Bytes)
	}

	filePath := filepath.Join(dir, ".harness9", "tool_results", "sess1", "tc_001.txt")
	data, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("file should exist: %v", err)
	}
	if string(data) != large {
		t.Error("file content should match original")
	}
	if !strings.Contains(placeholder, "tc_001.txt") {
		t.Errorf("placeholder should contain path, got: %q", placeholder)
	}
}

func TestCompactionOffloader_SkipsSmallContent(t *testing.T) {
	dir := t.TempDir()
	o := memory.NewCompactionOffloader(dir, "sess1")
	msg := schema.Message{Role: schema.RoleUser, Content: "small", ToolCallID: "tc_002"}
	_, _, err := o.OffloadToolResult(msg)
	if err == nil {
		t.Error("small content should return error (skip)")
	}
}

func TestCompactionOffloader_SkipsExistingPlaceholder(t *testing.T) {
	dir := t.TempDir()
	o := memory.NewCompactionOffloader(dir, "sess1")
	msg := schema.Message{
		Role:       schema.RoleUser,
		Content:    "[输出已保存至 .harness9/tool_results/sess1/old.txt]\n预览...",
		ToolCallID: "tc_003",
	}
	_, _, err := o.OffloadToolResult(msg)
	if err == nil {
		t.Error("existing OffloadHook placeholder should return error (skip)")
	}
}

func TestCompactionOffloader_CachePreventsRewrite(t *testing.T) {
	dir := t.TempDir()
	o := memory.NewCompactionOffloader(dir, "sess1")
	large := strings.Repeat("x", 5000)
	msg := schema.Message{Role: schema.RoleUser, Content: large, ToolCallID: "tc_004"}

	_, _, err := o.OffloadToolResult(msg)
	if err != nil {
		t.Fatalf("first offload failed: %v", err)
	}

	filePath := filepath.Join(dir, ".harness9", "tool_results", "sess1", "tc_004.txt")
	os.WriteFile(filePath, []byte("MODIFIED"), 0600)

	_, _, err = o.OffloadToolResult(msg)
	if err != nil {
		t.Fatalf("second offload failed: %v", err)
	}

	data, _ := os.ReadFile(filePath)
	if string(data) != "MODIFIED" {
		t.Error("cache should prevent rewrite")
	}
}

func TestCompactionOffloader_NoToolCallID(t *testing.T) {
	dir := t.TempDir()
	o := memory.NewCompactionOffloader(dir, "sess1")
	msg := schema.Message{Role: schema.RoleUser, Content: strings.Repeat("x", 5000)}
	_, _, err := o.OffloadToolResult(msg)
	if err == nil {
		t.Error("empty ToolCallID should return error")
	}
}
