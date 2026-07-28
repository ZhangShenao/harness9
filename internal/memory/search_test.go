package memory_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/harness9/internal/memory"
	"github.com/harness9/internal/schema"
)

// newTestManager 创建临时目录中的 Manager，测试结束自动关闭。
func newTestManager(t *testing.T) *memory.Manager {
	t.Helper()
	dir := t.TempDir()
	mgr, err := memory.NewManager(filepath.Join(dir, "search_test.db"))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(func() { mgr.Close() })
	return mgr
}

// addMsg 是向指定 session 写入消息的辅助函数。
func addMsg(t *testing.T, sess memory.Session, role schema.Role, content string) {
	t.Helper()
	if err := sess.AddMessages(context.Background(), []schema.Message{
		{Role: role, Content: content},
	}); err != nil {
		t.Fatalf("AddMessages: %v", err)
	}
}

// TestSearchMessages_Found 写入消息后能按关键词检索到。
func TestSearchMessages_Found(t *testing.T) {
	ctx := context.Background()
	mgr := newTestManager(t)

	sess, err := mgr.NewSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	addMsg(t, sess, schema.RoleUser, "the quick brown fox jumps over the lazy dog")

	results, err := mgr.SearchMessages(ctx, "fox", 0)
	if err != nil {
		t.Fatalf("SearchMessages error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	if results[0].SessionID != sess.SessionID() {
		t.Errorf("SessionID: want %q, got %q", sess.SessionID(), results[0].SessionID)
	}
	if results[0].Role != string(schema.RoleUser) {
		t.Errorf("Role: want %q, got %q", schema.RoleUser, results[0].Role)
	}
	if results[0].Content != "the quick brown fox jumps over the lazy dog" {
		t.Errorf("Content mismatch: %q", results[0].Content)
	}
}

// TestSearchMessages_NotFound 检索不存在的关键词应返回空切片，不报错。
func TestSearchMessages_NotFound(t *testing.T) {
	ctx := context.Background()
	mgr := newTestManager(t)

	sess, err := mgr.NewSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	addMsg(t, sess, schema.RoleAssistant, "hello world")

	results, err := mgr.SearchMessages(ctx, "zzznomatch999", 0)
	if err != nil {
		t.Fatalf("SearchMessages error: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("want 0 results, got %d", len(results))
	}
}

// TestSearchMessages_EmptyQuery query 为空字符串时返回空切片且不报错。
func TestSearchMessages_EmptyQuery(t *testing.T) {
	ctx := context.Background()
	mgr := newTestManager(t)

	sess, err := mgr.NewSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	addMsg(t, sess, schema.RoleUser, "something here")

	results, err := mgr.SearchMessages(ctx, "", 0)
	if err != nil {
		t.Fatalf("want nil error for empty query, got: %v", err)
	}
	if results == nil {
		t.Fatal("want non-nil empty slice, got nil")
	}
	if len(results) != 0 {
		t.Fatalf("want 0 results for empty query, got %d", len(results))
	}
}

// TestSearchMessages_LimitEnforced limit 参数生效：只返回不超过 limit 条结果。
func TestSearchMessages_LimitEnforced(t *testing.T) {
	ctx := context.Background()
	mgr := newTestManager(t)

	sess, err := mgr.NewSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// 写入 5 条都包含 "golang" 的消息
	for i := 0; i < 5; i++ {
		addMsg(t, sess, schema.RoleUser, "golang is great for building systems")
	}

	results, err := mgr.SearchMessages(ctx, "golang", 3)
	if err != nil {
		t.Fatalf("SearchMessages error: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("want 3 results with limit=3, got %d", len(results))
	}
}

// TestSearchMessages_DefaultLimit limit<=0 时默认最多 20 条。
func TestSearchMessages_DefaultLimit(t *testing.T) {
	ctx := context.Background()
	mgr := newTestManager(t)

	sess, err := mgr.NewSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// 写入 25 条消息
	for i := 0; i < 25; i++ {
		addMsg(t, sess, schema.RoleUser, "defaultlimit keyword repeated message")
	}

	// limit=0 应触发默认上限 20
	results, err := mgr.SearchMessages(ctx, "defaultlimit", 0)
	if err != nil {
		t.Fatalf("SearchMessages error: %v", err)
	}
	if len(results) > 20 {
		t.Fatalf("want at most 20 results with limit=0, got %d", len(results))
	}
	if len(results) == 0 {
		t.Fatal("want some results, got 0")
	}
}

// TestSearchMessages_MultiSession 多个 session 的消息互不串台：
// 检索只应返回包含关键词的 session 的消息。
func TestSearchMessages_MultiSession(t *testing.T) {
	ctx := context.Background()
	mgr := newTestManager(t)

	sessA, err := mgr.NewSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sessB, err := mgr.NewSession(ctx)
	if err != nil {
		t.Fatal(err)
	}

	addMsg(t, sessA, schema.RoleUser, "unique_keyword_alpha belongs to session A")
	addMsg(t, sessB, schema.RoleUser, "unique_keyword_beta belongs to session B")

	// 检索 alpha：只应命中 sessA
	resA, err := mgr.SearchMessages(ctx, "unique_keyword_alpha", 0)
	if err != nil {
		t.Fatalf("SearchMessages(alpha) error: %v", err)
	}
	if len(resA) != 1 {
		t.Fatalf("want 1 result for alpha, got %d", len(resA))
	}
	if resA[0].SessionID != sessA.SessionID() {
		t.Errorf("alpha result sessionID: want %q, got %q", sessA.SessionID(), resA[0].SessionID)
	}

	// 检索 beta：只应命中 sessB
	resB, err := mgr.SearchMessages(ctx, "unique_keyword_beta", 0)
	if err != nil {
		t.Fatalf("SearchMessages(beta) error: %v", err)
	}
	if len(resB) != 1 {
		t.Fatalf("want 1 result for beta, got %d", len(resB))
	}
	if resB[0].SessionID != sessB.SessionID() {
		t.Errorf("beta result sessionID: want %q, got %q", sessB.SessionID(), resB[0].SessionID)
	}
}

// TestSearchMessages_MultipleRoles 不同 role 的消息都能检索到，role 字段正确保留。
func TestSearchMessages_MultipleRoles(t *testing.T) {
	ctx := context.Background()
	mgr := newTestManager(t)

	sess, err := mgr.NewSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	addMsg(t, sess, schema.RoleUser, "rolecheck from user message")
	addMsg(t, sess, schema.RoleAssistant, "rolecheck from assistant message")

	results, err := mgr.SearchMessages(ctx, "rolecheck", 0)
	if err != nil {
		t.Fatalf("SearchMessages error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("want 2 results, got %d", len(results))
	}

	roles := map[string]bool{}
	for _, r := range results {
		roles[r.Role] = true
	}
	if !roles[string(schema.RoleUser)] {
		t.Error("want user role in results")
	}
	if !roles[string(schema.RoleAssistant)] {
		t.Error("want assistant role in results")
	}
}
