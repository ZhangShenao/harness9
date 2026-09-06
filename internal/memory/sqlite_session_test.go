package memory_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/harness9/internal/memory"
	"github.com/harness9/internal/planning"
	"github.com/harness9/internal/schema"
)

func newTestSession(t *testing.T) memory.Session {
	t.Helper()
	ctx := context.Background()
	mgr, err := memory.NewManager(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { mgr.Close() })
	sess, err := mgr.NewSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return sess
}

func TestSQLiteSession_AddAndGetAll(t *testing.T) {
	ctx := context.Background()
	sess := newTestSession(t)

	err := sess.AddMessages(ctx, []schema.Message{
		{Role: schema.RoleUser, Content: "hello"},
		{Role: schema.RoleAssistant, Content: "hi"},
	})
	if err != nil {
		t.Fatal(err)
	}

	msgs, err := sess.GetMessages(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("want 2, got %d", len(msgs))
	}
	if msgs[0].Content != "hello" || msgs[0].Role != schema.RoleUser {
		t.Errorf("unexpected first msg: %+v", msgs[0])
	}
	if msgs[1].Content != "hi" || msgs[1].Role != schema.RoleAssistant {
		t.Errorf("unexpected second msg: %+v", msgs[1])
	}
}

func TestSQLiteSession_GetWithLimit(t *testing.T) {
	ctx := context.Background()
	sess := newTestSession(t)

	_ = sess.AddMessages(ctx, []schema.Message{
		{Role: schema.RoleUser, Content: "a"},
		{Role: schema.RoleUser, Content: "b"},
		{Role: schema.RoleUser, Content: "c"},
	})

	got, err := sess.GetMessages(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2, got %d", len(got))
	}
	if got[0].Content != "b" || got[1].Content != "c" {
		t.Errorf("want b,c; got %q,%q", got[0].Content, got[1].Content)
	}
}

func TestSQLiteSession_ToolCalls(t *testing.T) {
	ctx := context.Background()
	sess := newTestSession(t)

	assistantMsg := schema.Message{
		Role: schema.RoleAssistant,
		ToolCalls: []schema.ToolCall{
			{ID: "tc1", Name: "bash", Arguments: []byte(`{"command":"ls"}`)},
		},
	}
	obs := schema.Message{
		Role:       schema.RoleUser,
		Content:    "file1\nfile2",
		ToolCallID: "tc1",
	}

	if err := sess.AddMessages(ctx, []schema.Message{assistantMsg, obs}); err != nil {
		t.Fatal(err)
	}

	msgs, err := sess.GetMessages(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("want 2, got %d", len(msgs))
	}
	if len(msgs[0].ToolCalls) != 1 || msgs[0].ToolCalls[0].ID != "tc1" {
		t.Errorf("tool call not preserved: %+v", msgs[0])
	}
	if msgs[1].ToolCallID != "tc1" {
		t.Errorf("tool_call_id not preserved: %+v", msgs[1])
	}
}

func TestSQLiteSession_PopMessage(t *testing.T) {
	ctx := context.Background()
	sess := newTestSession(t)

	_ = sess.AddMessages(ctx, []schema.Message{
		{Role: schema.RoleUser, Content: "first"},
		{Role: schema.RoleAssistant, Content: "second"},
	})

	popped, err := sess.PopMessage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if popped == nil || popped.Content != "second" {
		t.Errorf("want second, got %v", popped)
	}

	all, _ := sess.GetMessages(ctx, 0)
	if len(all) != 1 {
		t.Fatalf("want 1 after pop, got %d", len(all))
	}
}

func TestSQLiteSession_Clear(t *testing.T) {
	ctx := context.Background()
	sess := newTestSession(t)
	_ = sess.AddMessages(ctx, []schema.Message{{Role: schema.RoleUser, Content: "x"}})
	if err := sess.Clear(ctx); err != nil {
		t.Fatal(err)
	}
	all, _ := sess.GetMessages(ctx, 0)
	if len(all) != 0 {
		t.Errorf("want 0 after clear, got %d", len(all))
	}
}

func TestSQLiteSession_Persistence(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "persist.db")

	mgr1, _ := memory.NewManager(dbPath)
	sess1, _ := mgr1.NewSession(ctx)
	id := sess1.SessionID()
	_ = sess1.AddMessages(ctx, []schema.Message{{Role: schema.RoleUser, Content: "persisted"}})
	mgr1.Close()

	mgr2, err := memory.NewManager(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr2.Close()
	sess2, err := mgr2.OpenSession(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := sess2.GetMessages(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Content != "persisted" {
		t.Errorf("persistence failed: %+v", msgs)
	}
}

func TestSQLiteSession_SaveAndGetPlan(t *testing.T) {
	ctx := context.Background()
	sess := newTestSession(t)

	items := []planning.PlanItem{
		{ID: "1", Content: "step one", Status: planning.PlanPending},
		{ID: "2", Content: "step two", Status: planning.PlanInProgress},
		{ID: "3", Content: "step three", Status: planning.PlanCompleted},
	}

	if err := sess.SavePlan(ctx, items); err != nil {
		t.Fatalf("SavePlan error: %v", err)
	}

	got, err := sess.GetPlan(ctx)
	if err != nil {
		t.Fatalf("GetPlan error: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 plans, got %d", len(got))
	}
	if got[0].ID != "1" || got[0].Content != "step one" || got[0].Status != planning.PlanPending {
		t.Errorf("unexpected item 0: %+v", got[0])
	}
	if got[1].ID != "2" || got[1].Content != "step two" || got[1].Status != planning.PlanInProgress {
		t.Errorf("unexpected item 1: %+v", got[1])
	}
	if got[2].ID != "3" || got[2].Content != "step three" || got[2].Status != planning.PlanCompleted {
		t.Errorf("unexpected item 2: %+v", got[2])
	}
}

func TestSQLiteSession_SavePlan_WriteReplace(t *testing.T) {
	ctx := context.Background()
	sess := newTestSession(t)

	first := []planning.PlanItem{
		{ID: "old", Content: "old task", Status: planning.PlanPending},
	}
	if err := sess.SavePlan(ctx, first); err != nil {
		t.Fatalf("first SavePlan: %v", err)
	}

	second := []planning.PlanItem{
		{ID: "new", Content: "new task", Status: planning.PlanInProgress},
	}
	if err := sess.SavePlan(ctx, second); err != nil {
		t.Fatalf("second SavePlan: %v", err)
	}

	got, err := sess.GetPlan(ctx)
	if err != nil {
		t.Fatalf("GetPlan: %v", err)
	}
	if len(got) != 1 || got[0].ID != "new" {
		t.Errorf("write-replace failed, got: %+v", got)
	}
}

func TestSQLiteSession_SavePlan_Empty(t *testing.T) {
	ctx := context.Background()
	sess := newTestSession(t)

	// Save some plans first
	if err := sess.SavePlan(ctx, []planning.PlanItem{
		{ID: "1", Content: "task", Status: planning.PlanPending},
	}); err != nil {
		t.Fatalf("SavePlan: %v", err)
	}

	// Save empty list → should clear
	if err := sess.SavePlan(ctx, nil); err != nil {
		t.Fatalf("SavePlan empty: %v", err)
	}

	got, err := sess.GetPlan(ctx)
	if err != nil {
		t.Fatalf("GetPlan: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty list after saving nil, got: %+v", got)
	}
}

func TestSQLiteSession_GetPlan_CrossSessionIsolation(t *testing.T) {
	ctx := context.Background()
	mgr, err := memory.NewManager(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { mgr.Close() })

	sess1, err := mgr.NewSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sess2, err := mgr.NewSession(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if err := sess1.SavePlan(ctx, []planning.PlanItem{
		{ID: "a", Content: "sess1 task", Status: planning.PlanPending},
	}); err != nil {
		t.Fatalf("sess1 SavePlan: %v", err)
	}

	got, err := sess2.GetPlan(ctx)
	if err != nil {
		t.Fatalf("sess2 GetPlan: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("sess2 should see no plans, got: %+v", got)
	}
}
