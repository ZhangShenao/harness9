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

// TestPlanWriteTool_PartialUpdatePreservesCompleted 还原线上事故：LLM 增量更新计划时
// 只发送仍需执行的条目（如 [7,8]），旧实现全量替换会把已完成的 1-6 丢失，
// 权威状态缩水成 2 条（TUI 显示 1/2 而非 7/8）。
// 核心不变量：已完成条目是既成事实的历史记录，部分更新时必须自动保留，
// 且保持首次创建时的相对顺序，使 TUI 编号跨更新稳定。
func TestPlanWriteTool_PartialUpdatePreservesCompleted(t *testing.T) {
	store := planning.NewPlanStore()
	tool := tools.NewPlanWriteTool(store)

	// 按合法协议构造初始状态：先建 8 条 pending → 全部标 in_progress → 1-6 标 completed
	// （in_progress→completed 不受"单个允许"限制，防作弊校验仅拦截 pending/新建 → completed）
	init, _ := json.Marshal(map[string]interface{}{
		"steps": []map[string]string{
			{"id": "1", "content": "s1", "status": "pending"},
			{"id": "2", "content": "s2", "status": "pending"},
			{"id": "3", "content": "s3", "status": "pending"},
			{"id": "4", "content": "s4", "status": "pending"},
			{"id": "5", "content": "s5", "status": "pending"},
			{"id": "6", "content": "s6", "status": "pending"},
			{"id": "7", "content": "s7", "status": "pending"},
			{"id": "8", "content": "s8", "status": "pending"},
		},
	})
	if _, err := tool.Execute(context.Background(), init); err != nil {
		t.Fatalf("init failed: %v", err)
	}
	inProgress, _ := json.Marshal(map[string]interface{}{
		"steps": []map[string]string{
			{"id": "1", "content": "s1", "status": "in_progress"},
			{"id": "2", "content": "s2", "status": "in_progress"},
			{"id": "3", "content": "s3", "status": "in_progress"},
			{"id": "4", "content": "s4", "status": "in_progress"},
			{"id": "5", "content": "s5", "status": "in_progress"},
			{"id": "6", "content": "s6", "status": "in_progress"},
			{"id": "7", "content": "s7", "status": "in_progress"},
			{"id": "8", "content": "s8", "status": "in_progress"},
		},
	})
	if _, err := tool.Execute(context.Background(), inProgress); err != nil {
		t.Fatalf("mark in_progress failed: %v", err)
	}
	completed, _ := json.Marshal(map[string]interface{}{
		"steps": []map[string]string{
			{"id": "1", "content": "s1", "status": "completed"},
			{"id": "2", "content": "s2", "status": "completed"},
			{"id": "3", "content": "s3", "status": "completed"},
			{"id": "4", "content": "s4", "status": "completed"},
			{"id": "5", "content": "s5", "status": "completed"},
			{"id": "6", "content": "s6", "status": "completed"},
			{"id": "7", "content": "s7", "status": "in_progress"},
			{"id": "8", "content": "s8", "status": "in_progress"},
		},
	})
	if _, err := tool.Execute(context.Background(), completed); err != nil {
		t.Fatalf("mark completed failed: %v", err)
	}

	// 事故写入：只提交剩余条目（7 直接完成属单个允许范围，8 转入 in_progress）
	update, _ := json.Marshal(map[string]interface{}{
		"steps": []map[string]string{
			{"id": "7", "content": "s7", "status": "completed"},
			{"id": "8", "content": "s8", "status": "in_progress"},
		},
	})
	result, err := tool.Execute(context.Background(), update)
	if err != nil {
		t.Fatalf("partial update failed: %v", err)
	}

	stored := store.Read()
	if len(stored) != 8 {
		t.Fatalf("部分更新后应保留全部 8 条（含已完成历史），实际 %d 条: %+v", len(stored), stored)
	}
	// 顺序必须稳定：1-8 保持首次创建顺序，TUI 编号不跳变
	for i, want := range []string{"1", "2", "3", "4", "5", "6", "7", "8"} {
		if stored[i].ID != want {
			t.Errorf("stored[%d].ID = %q, want %q", i, stored[i].ID, want)
		}
	}
	// 状态合并正确：历史 1-6 保持 completed，7 更新为 completed，8 更新为 in_progress
	for _, item := range stored[:7] {
		if item.Status != planning.PlanCompleted {
			t.Errorf("item %s status = %q, want completed", item.ID, item.Status)
		}
	}
	if stored[7].Status != planning.PlanInProgress {
		t.Errorf("item 8 status = %q, want in_progress", stored[7].Status)
	}

	// 返回值与存储一致（也应是合并后的 8 条）
	var got []planning.PlanItem
	if err := json.Unmarshal([]byte(result), &got); err != nil {
		t.Fatalf("result not valid JSON: %v — got %q", err, result)
	}
	if len(got) != 8 {
		t.Errorf("返回值应包含合并后的 8 条，实际 %d 条", len(got))
	}
}

// TestPlanWriteTool_PartialUpdateAppendsNewItems 验证部分更新中新增的条目
// 追加在保留的已完成历史之后。
func TestPlanWriteTool_PartialUpdateAppendsNewItems(t *testing.T) {
	store := planning.NewPlanStore()
	tool := tools.NewPlanWriteTool(store)

	init, _ := json.Marshal(map[string]interface{}{
		"steps": []map[string]string{
			{"id": "1", "content": "done", "status": "completed"},
			{"id": "2", "content": "doing", "status": "in_progress"},
		},
	})
	if _, err := tool.Execute(context.Background(), init); err != nil {
		t.Fatalf("init failed: %v", err)
	}

	add, _ := json.Marshal(map[string]interface{}{
		"steps": []map[string]string{
			{"id": "3", "content": "new step", "status": "pending"},
		},
	})
	if _, err := tool.Execute(context.Background(), add); err != nil {
		t.Fatalf("append update failed: %v", err)
	}

	stored := store.Read()
	if len(stored) != 3 {
		t.Fatalf("want 3 items, got %d: %+v", len(stored), stored)
	}
	if stored[0].ID != "1" || stored[0].Status != planning.PlanCompleted {
		t.Errorf("已完成条目应被保留且在前: %+v", stored[0])
	}
	if stored[1].ID != "2" || stored[1].Status != planning.PlanInProgress {
		t.Errorf("活跃条目应更新为新版本: %+v", stored[1])
	}
	if stored[2].ID != "3" || stored[2].Content != "new step" {
		t.Errorf("新增条目应追加在末尾: %+v", stored[2])
	}
}

// TestPlanWriteTool_OmittedNonCompletedDropped 验证计划裁剪语义：
// 被省略的 pending 条目视为有意裁剪（尚未开始的工作允许删减），
// cancelled 条目同样不复活；只有 in_progress/completed 受保留保护。
func TestPlanWriteTool_OmittedNonCompletedDropped(t *testing.T) {
	store := planning.NewPlanStore()
	tool := tools.NewPlanWriteTool(store)

	init, _ := json.Marshal(map[string]interface{}{
		"steps": []map[string]string{
			{"id": "1", "content": "pending", "status": "pending"},
			{"id": "2", "content": "active", "status": "in_progress"},
			{"id": "3", "content": "done", "status": "completed"},
			{"id": "4", "content": "cancelled", "status": "cancelled"},
		},
	})
	if _, err := tool.Execute(context.Background(), init); err != nil {
		t.Fatalf("init failed: %v", err)
	}

	// 新增条目、省略其余：1（pending）与 4（cancelled）被裁剪，2/3 保留
	replace, _ := json.Marshal(map[string]interface{}{
		"steps": []map[string]string{
			{"id": "5", "content": "fresh", "status": "pending"},
		},
	})
	if _, err := tool.Execute(context.Background(), replace); err != nil {
		t.Fatalf("replace failed: %v", err)
	}

	stored := store.Read()
	if len(stored) != 3 {
		t.Fatalf("want 3 items (保留 2/3 + 新增 5), got %d: %+v", len(stored), stored)
	}
	if stored[0].ID != "2" || stored[0].Status != planning.PlanInProgress {
		t.Errorf("被省略的 in_progress 条目应保留: %+v", stored[0])
	}
	if stored[1].ID != "3" || stored[1].Status != planning.PlanCompleted {
		t.Errorf("被省略的 completed 条目应保留: %+v", stored[1])
	}
	if stored[2].ID != "5" {
		t.Errorf("新增条目应追加在末尾: %+v", stored[2])
	}
}

// TestPlanWriteTool_PlanWriterReceivesMergedList 验证 PlanWriter（落盘/检查点）
// 收到的是合并后的完整列表，而非 LLM 提交的子集——否则计划文件同样会丢失历史。
func TestPlanWriteTool_PlanWriterReceivesMergedList(t *testing.T) {
	store := planning.NewPlanStore()
	pw := &mockPlanWriter{}
	tool := tools.NewPlanWriteTool(store, tools.WithPlanWriter(pw))

	init, _ := json.Marshal(map[string]interface{}{
		"steps": []map[string]string{
			{"id": "1", "content": "done", "status": "completed"},
			{"id": "2", "content": "doing", "status": "in_progress"},
		},
	})
	if _, err := tool.Execute(context.Background(), init); err != nil {
		t.Fatalf("init failed: %v", err)
	}

	update, _ := json.Marshal(map[string]interface{}{
		"steps": []map[string]string{
			{"id": "2", "content": "doing", "status": "completed"},
		},
	})
	if _, err := tool.Execute(context.Background(), update); err != nil {
		t.Fatalf("update failed: %v", err)
	}

	if pw.calls != 2 {
		t.Fatalf("PlanWriter.Write should be called twice, called %d times", pw.calls)
	}
	if len(pw.last) != 2 {
		t.Errorf("PlanWriter 应收到合并后的 2 条，实际 %d 条: %+v", len(pw.last), pw.last)
	}
	if pw.last[0].ID != "1" || pw.last[0].Status != planning.PlanCompleted {
		t.Errorf("PlanWriter 收到的列表应保留已完成条目: %+v", pw.last[0])
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
