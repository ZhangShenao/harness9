package tools_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/harness9/internal/planning"
	"github.com/harness9/internal/tools"
)

func TestPlanWriteTool_Name(t *testing.T) {
	store := planning.NewPlanStore()
	tool := tools.NewPlanWriteTool(store)
	if tool.Name() != "plan_write" {
		t.Errorf("Name() = %q, want plan_write", tool.Name())
	}
}

func TestPlanWriteTool_Write(t *testing.T) {
	store := planning.NewPlanStore()
	tool := tools.NewPlanWriteTool(store)

	args, _ := json.Marshal(map[string]interface{}{
		"steps": []map[string]string{
			{"id": "1", "content": "step one", "status": "pending"},
			{"id": "2", "content": "step two", "status": "in_progress"},
		},
	})

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}

	// Result should be JSON of the current list
	var got []planning.PlanItem
	if err := json.Unmarshal([]byte(result), &got); err != nil {
		t.Fatalf("result not valid JSON: %v — got %q", err, result)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 items, got %d", len(got))
	}
	if got[0].ID != "1" || got[1].ID != "2" {
		t.Errorf("unexpected items: %+v", got)
	}

	// Store should be updated
	stored := store.Read()
	if len(stored) != 2 {
		t.Fatalf("store has %d items, want 2", len(stored))
	}
}

func TestPlanWriteTool_Read_WhenNoSteps(t *testing.T) {
	store := planning.NewPlanStore()
	tool := tools.NewPlanWriteTool(store)

	// Omit todos field → read current (empty) list
	args, _ := json.Marshal(map[string]interface{}{})
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	// Should return "[]" for empty list
	var got []planning.PlanItem
	if err := json.Unmarshal([]byte(result), &got); err != nil {
		t.Fatalf("result not valid JSON: %v — got %q", err, result)
	}
	if len(got) != 0 {
		t.Errorf("want empty list, got %+v", got)
	}
}

func TestPlanWriteTool_Write_Replaces(t *testing.T) {
	store := planning.NewPlanStore()
	tool := tools.NewPlanWriteTool(store)

	first, _ := json.Marshal(map[string]interface{}{
		"steps": []map[string]string{
			{"id": "1", "content": "old", "status": "pending"},
		},
	})
	tool.Execute(context.Background(), first) //nolint:errcheck

	second, _ := json.Marshal(map[string]interface{}{
		"steps": []map[string]string{
			{"id": "2", "content": "new", "status": "in_progress"},
		},
	})
	tool.Execute(context.Background(), second) //nolint:errcheck

	stored := store.Read()
	if len(stored) != 1 || stored[0].ID != "2" {
		t.Errorf("second Write should replace first: %+v", stored)
	}
}

func TestPlanWriteTool_InvalidJSON(t *testing.T) {
	store := planning.NewPlanStore()
	tool := tools.NewPlanWriteTool(store)

	_, err := tool.Execute(context.Background(), []byte(`not json`))
	if err == nil {
		t.Error("expected error for invalid JSON, got nil")
	}
}

// TestPlanWriteTool_BulkPendingToCompleted 验证批量 pending→completed（2 个以上）被拒绝。
// 单个计划条目直接 pending→completed 允许（LLM 实际完成工作但未经 in_progress 步骤），
// 但同时完成 2+ 个未开始的计划条目视为作弊行为。
func TestPlanWriteTool_BulkPendingToCompleted(t *testing.T) {
	store := planning.NewPlanStore()
	tool := tools.NewPlanWriteTool(store)

	// 初始化：两个 pending 计划条目
	init, _ := json.Marshal(map[string]interface{}{
		"steps": []map[string]string{
			{"id": "1", "content": "task one", "status": "pending"},
			{"id": "2", "content": "task two", "status": "pending"},
		},
	})
	if _, err := tool.Execute(context.Background(), init); err != nil {
		t.Fatalf("init failed: %v", err)
	}

	// 尝试在一次调用中将两个 pending 计划条目全部标记为 completed（批量作弊）
	cheat, _ := json.Marshal(map[string]interface{}{
		"steps": []map[string]string{
			{"id": "1", "content": "task one", "status": "completed"},
			{"id": "2", "content": "task two", "status": "completed"},
		},
	})
	_, err := tool.Execute(context.Background(), cheat)
	if err == nil {
		t.Error("expected error when bulk-completing 2 pending items, got nil")
	}

	// store 应保持未变
	stored := store.Read()
	for _, item := range stored {
		if item.Status == planning.PlanCompleted {
			t.Errorf("store should not have completed items after rejected write, got %+v", stored)
		}
	}
}

// TestPlanWriteTool_SinglePendingToCompleted 验证单个 pending→completed 允许通过。
// LLM 完成工作后可以直接标记为 completed，不强制要求经过 in_progress。
func TestPlanWriteTool_SinglePendingToCompleted(t *testing.T) {
	store := planning.NewPlanStore()
	tool := tools.NewPlanWriteTool(store)

	// 初始化：一个 pending 计划条目
	init, _ := json.Marshal(map[string]interface{}{
		"steps": []map[string]string{
			{"id": "1", "content": "task one", "status": "pending"},
		},
	})
	if _, err := tool.Execute(context.Background(), init); err != nil {
		t.Fatalf("init failed: %v", err)
	}

	// 单个 pending → completed 应该允许（LLM 完成了实际工作）
	complete, _ := json.Marshal(map[string]interface{}{
		"steps": []map[string]string{
			{"id": "1", "content": "task one", "status": "completed"},
		},
	})
	if _, err := tool.Execute(context.Background(), complete); err != nil {
		t.Errorf("single pending→completed should be allowed, got error: %v", err)
	}
}

// TestPlanWriteTool_InProgressToCompleted 验证 in_progress→completed 允许通过。
func TestPlanWriteTool_InProgressToCompleted(t *testing.T) {
	store := planning.NewPlanStore()
	tool := tools.NewPlanWriteTool(store)

	// 初始化：item1 in_progress
	init, _ := json.Marshal(map[string]interface{}{
		"steps": []map[string]string{
			{"id": "1", "content": "task one", "status": "in_progress"},
		},
	})
	if _, err := tool.Execute(context.Background(), init); err != nil {
		t.Fatalf("init failed: %v", err)
	}

	// in_progress → completed 合法
	complete, _ := json.Marshal(map[string]interface{}{
		"steps": []map[string]string{
			{"id": "1", "content": "task one", "status": "completed"},
		},
	})
	if _, err := tool.Execute(context.Background(), complete); err != nil {
		t.Errorf("in_progress→completed should be allowed, got error: %v", err)
	}
}

// TestPlanWriteTool_CancelledToCompleted 验证 cancelled→completed 始终被拒绝。
// cancelled 计划条目必须先恢复为 pending/in_progress 才能完成，不适用"单个允许"宽松规则。
func TestPlanWriteTool_CancelledToCompleted(t *testing.T) {
	store := planning.NewPlanStore()
	tool := tools.NewPlanWriteTool(store)

	// 初始化：一个 cancelled 计划条目
	init, _ := json.Marshal(map[string]interface{}{
		"steps": []map[string]string{
			{"id": "1", "content": "task one", "status": "cancelled"},
		},
	})
	if _, err := tool.Execute(context.Background(), init); err != nil {
		t.Fatalf("init failed: %v", err)
	}

	// cancelled → completed 即使只有 1 个也应被拒绝
	args, _ := json.Marshal(map[string]interface{}{
		"steps": []map[string]string{
			{"id": "1", "content": "task one", "status": "completed"},
		},
	})
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Error("expected error when cancelled→completed, got nil")
	}
}

// TestPlanWriteTool_SingleDirectPlusInProgress 验证"1 个直接完成 + 1 个经 in_progress 完成"的
// 混合调用允许通过（directCompletions == 1，未超过阈值）。
func TestPlanWriteTool_SingleDirectPlusInProgress(t *testing.T) {
	store := planning.NewPlanStore()
	tool := tools.NewPlanWriteTool(store)

	// 初始化：item1 pending，item2 in_progress
	init, _ := json.Marshal(map[string]interface{}{
		"steps": []map[string]string{
			{"id": "1", "content": "task one", "status": "pending"},
			{"id": "2", "content": "task two", "status": "in_progress"},
		},
	})
	if _, err := tool.Execute(context.Background(), init); err != nil {
		t.Fatalf("init failed: %v", err)
	}

	// item1: pending→completed（1 个直接完成），item2: in_progress→completed（合法）
	// directCompletions == 1 → 应允许通过
	args, _ := json.Marshal(map[string]interface{}{
		"steps": []map[string]string{
			{"id": "1", "content": "task one", "status": "completed"},
			{"id": "2", "content": "task two", "status": "completed"},
		},
	})
	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Errorf("1 direct + 1 in_progress completion should be allowed, got error: %v", err)
	}
}

// TestPlanWriteTool_BulkNewItemCompleted 验证批量新建 completed 条目（2 个以上）被拒绝。
// 单个新建直接 completed 允许（LLM 可能完成了工作再创建记录），
// 同时新建 2+ 个 completed 条目视为作弊。
func TestPlanWriteTool_BulkNewItemCompleted(t *testing.T) {
	store := planning.NewPlanStore()
	tool := tools.NewPlanWriteTool(store)

	// 同时创建 2 个已完成的全新条目 → 应被拒绝
	args, _ := json.Marshal(map[string]interface{}{
		"steps": []map[string]string{
			{"id": "1", "content": "brand new one", "status": "completed"},
			{"id": "2", "content": "brand new two", "status": "completed"},
		},
	})
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Error("expected error when creating 2 new items as completed, got nil")
	}
}

// mockPlanWriter 记录 Write 调用次数和最后收到的 todos。
type mockPlanWriter struct {
	calls int
	last  []planning.PlanItem
	err   error
}

func (m *mockPlanWriter) Write(todos []planning.PlanItem) error {
	m.calls++
	m.last = todos
	return m.err
}

func TestPlanWriteTool_PlanWriterCalledOnWrite(t *testing.T) {
	store := planning.NewPlanStore()
	pw := &mockPlanWriter{}
	tool := tools.NewPlanWriteTool(store, tools.WithPlanWriter(pw))

	args, _ := json.Marshal(map[string]interface{}{
		"steps": []map[string]string{
			{"id": "1", "content": "task", "status": "pending"},
		},
	})
	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatal(err)
	}

	if pw.calls != 1 {
		t.Errorf("PlanWriter.Write should be called once, called %d times", pw.calls)
	}
	if len(pw.last) != 1 || pw.last[0].Content != "task" {
		t.Errorf("PlanWriter received wrong todos: %v", pw.last)
	}
}

func TestPlanWriteTool_PlanWriterNotCalledOnRead(t *testing.T) {
	store := planning.NewPlanStore()
	pw := &mockPlanWriter{}
	tool := tools.NewPlanWriteTool(store, tools.WithPlanWriter(pw))

	// Read operation (no todos field) should NOT call PlanWriter
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if pw.calls != 0 {
		t.Errorf("PlanWriter.Write must not be called on read operation")
	}
}

func TestPlanWriteTool_PlanWriterNil_NoChange(t *testing.T) {
	store := planning.NewPlanStore()
	// No WithPlanWriter option — should behave identically to original
	tool := tools.NewPlanWriteTool(store)
	args, _ := json.Marshal(map[string]interface{}{
		"steps": []map[string]string{{"id": "1", "content": "x", "status": "pending"}},
	})
	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatalf("tool without PlanWriter should still work: %v", err)
	}
}

func TestPlanWriteTool_PlanWriterError_DoesNotAffectResult(t *testing.T) {
	store := planning.NewPlanStore()
	pw := &mockPlanWriter{err: errors.New("disk full")}
	tool := tools.NewPlanWriteTool(store, tools.WithPlanWriter(pw))

	args, _ := json.Marshal(map[string]interface{}{
		"steps": []map[string]string{{"id": "1", "content": "x", "status": "pending"}},
	})
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("PlanWriter error should not propagate: %v", err)
	}
	if result == "" {
		t.Error("result should not be empty even when PlanWriter fails")
	}
}
