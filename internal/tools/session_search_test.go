// internal/tools/session_search_test.go
package tools

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/harness9/internal/memory"
	"github.com/harness9/internal/schema"
)

// newTestSessionSearchManager 创建临时目录中的 Manager，测试结束自动关闭。
func newTestSessionSearchManager(t *testing.T) *memory.Manager {
	t.Helper()
	dir := t.TempDir()
	mgr, err := memory.NewManager(filepath.Join(dir, "session_search_test.db"))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(func() { mgr.Close() })
	return mgr
}

// addSessionMsg 向指定 session 写入一条消息的辅助函数。
func addSessionMsg(t *testing.T, sess memory.Session, role schema.Role, content string) {
	t.Helper()
	if err := sess.AddMessages(context.Background(), []schema.Message{
		{Role: role, Content: content},
	}); err != nil {
		t.Fatalf("AddMessages: %v", err)
	}
}

// TestSessionSearchTool_Name 检查工具名称。
func TestSessionSearchTool_Name(t *testing.T) {
	mgr := newTestSessionSearchManager(t)
	tool := NewSessionSearchTool(mgr)
	if got := tool.Name(); got != "session_search" {
		t.Errorf("Name() = %q, want %q", got, "session_search")
	}
}

// TestSessionSearchTool_Definition 检查工具定义基本字段。
func TestSessionSearchTool_Definition(t *testing.T) {
	mgr := newTestSessionSearchManager(t)
	tool := NewSessionSearchTool(mgr)
	def := tool.Definition()
	if def.Name != "session_search" {
		t.Errorf("Definition().Name = %q, want %q", def.Name, "session_search")
	}
	if def.Description == "" {
		t.Error("Definition().Description 不应为空")
	}
	if def.InputSchema == nil {
		t.Error("Definition().InputSchema 不应为 nil")
	}
}

// TestSessionSearchTool_Hit 写入消息后能按关键词检索到。
func TestSessionSearchTool_Hit(t *testing.T) {
	ctx := context.Background()
	mgr := newTestSessionSearchManager(t)

	sess, err := mgr.NewSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	addSessionMsg(t, sess, schema.RoleUser, "golang concurrency with goroutines and channels")
	addSessionMsg(t, sess, schema.RoleAssistant, "goroutines are lightweight threads managed by the Go runtime")

	tool := NewSessionSearchTool(mgr)
	out, err := tool.Execute(ctx, json.RawMessage(`{"query":"goroutines"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out == "[]" {
		t.Fatal("应检索到消息，但返回了空数组")
	}
	if !strings.Contains(out, "goroutines") {
		t.Errorf("结果应包含 goroutines，got: %s", out)
	}
	// 验证返回的是合法 JSON 数组
	var results []map[string]interface{}
	if err := json.Unmarshal([]byte(out), &results); err != nil {
		t.Fatalf("结果应为合法 JSON 数组: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("results 数组不应为空")
	}
	// 验证字段结构
	for _, r := range results {
		if _, ok := r["SessionID"]; !ok {
			t.Error("结果条目应含 SessionID 字段")
		}
		if _, ok := r["Role"]; !ok {
			t.Error("结果条目应含 Role 字段")
		}
		if _, ok := r["Content"]; !ok {
			t.Error("结果条目应含 Content 字段")
		}
	}
}

// TestSessionSearchTool_NoHit 检索不存在的关键词应返回 "[]"，不报错。
func TestSessionSearchTool_NoHit(t *testing.T) {
	ctx := context.Background()
	mgr := newTestSessionSearchManager(t)

	sess, err := mgr.NewSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	addSessionMsg(t, sess, schema.RoleUser, "hello world")

	tool := NewSessionSearchTool(mgr)
	out, err := tool.Execute(ctx, json.RawMessage(`{"query":"zzznomatch999"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out != "[]" {
		t.Errorf("无命中应返回 \"[]\"，got %q", out)
	}
}

// TestSessionSearchTool_LimitEnforced limit 参数生效：只返回不超过 limit 条结果。
func TestSessionSearchTool_LimitEnforced(t *testing.T) {
	ctx := context.Background()
	mgr := newTestSessionSearchManager(t)

	sess, err := mgr.NewSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// 写入 6 条都包含 "searchkeyword" 的消息
	for i := 0; i < 6; i++ {
		addSessionMsg(t, sess, schema.RoleUser, "searchkeyword appears in every message")
	}

	tool := NewSessionSearchTool(mgr)
	// limit=3，只应返回 3 条
	out, err := tool.Execute(ctx, json.RawMessage(`{"query":"searchkeyword","limit":3}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var results []map[string]interface{}
	if err := json.Unmarshal([]byte(out), &results); err != nil {
		t.Fatalf("结果应为合法 JSON 数组: %v", err)
	}
	if len(results) != 3 {
		t.Errorf("limit=3 时应返回 3 条，got %d", len(results))
	}
}

// TestSessionSearchTool_DefaultLimit 不传 limit（或 limit=0）时默认使用 5。
func TestSessionSearchTool_DefaultLimit(t *testing.T) {
	ctx := context.Background()
	mgr := newTestSessionSearchManager(t)

	sess, err := mgr.NewSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// 写入 8 条都包含 "defaultkw" 的消息
	for i := 0; i < 8; i++ {
		addSessionMsg(t, sess, schema.RoleUser, "defaultkw repeated message content")
	}

	tool := NewSessionSearchTool(mgr)
	// 不传 limit，默认 5
	out, err := tool.Execute(ctx, json.RawMessage(`{"query":"defaultkw"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var results []map[string]interface{}
	if err := json.Unmarshal([]byte(out), &results); err != nil {
		t.Fatalf("结果应为合法 JSON 数组: %v", err)
	}
	if len(results) > 5 {
		t.Errorf("默认 limit=5，应最多返回 5 条，got %d", len(results))
	}
	if len(results) == 0 {
		t.Fatal("写入了 8 条消息，应检索到结果")
	}
}

// TestSessionSearchTool_EmptyQuery query 为空时应返回参数错误，不发起检索。
func TestSessionSearchTool_EmptyQuery(t *testing.T) {
	ctx := context.Background()
	mgr := newTestSessionSearchManager(t)

	sess, err := mgr.NewSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	addSessionMsg(t, sess, schema.RoleUser, "some content here")

	tool := NewSessionSearchTool(mgr)
	_, err = tool.Execute(ctx, json.RawMessage(`{"query":""}`))
	if err == nil {
		t.Fatal("query 为空时应返回错误，got nil")
	}
	if !strings.Contains(err.Error(), "query") {
		t.Errorf("错误信息应提及 query，got: %v", err)
	}
}
