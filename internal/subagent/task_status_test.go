// task_status 工具单元测试。
package subagent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestTaskStatusToolListAndSingle 验证全部列表与单任务查询、
// 已完成结果随查询返回并标记注入（防重复注入）。
func TestTaskStatusToolListAndSingle(t *testing.T) {
	tr := NewTaskTracker()
	id1 := tr.Start("explorer", "梳理调用", "p1")
	id2 := tr.Start("researcher", "调研", "p2")
	tr.Finish(id2, "调研结论 ABC", false)

	tool := NewTaskStatusTool(tr)
	out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, id1) || !strings.Contains(out, "运行中") ||
		!strings.Contains(out, id2) || !strings.Contains(out, "调研结论 ABC") {
		t.Fatalf("列表输出不符:\n%s", out)
	}
	// id2 结果已随查询注入 → Drain 不再返回
	for _, c := range tr.DrainCompleted() {
		if c.TaskID == id2 {
			t.Fatal("查询过的结果不应再被 Drain 注入")
		}
	}

	out, err = tool.Execute(context.Background(),
		json.RawMessage(`{"task_id":"`+id1+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "调用工具") && !strings.Contains(out, id1) {
		t.Fatalf("单任务输出不符:\n%s", out)
	}
}

// TestTaskStatusToolEmpty 验证无任务时的友好输出。
func TestTaskStatusToolEmpty(t *testing.T) {
	tool := NewTaskStatusTool(NewTaskTracker())
	out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil || !strings.Contains(out, "没有后台子代理任务") {
		t.Fatalf("输出不符: %s, err=%v", out, err)
	}
}

// TestTaskStatusToolUnknownID 验证未知 task_id 返回错误。
func TestTaskStatusToolUnknownID(t *testing.T) {
	tool := NewTaskStatusTool(NewTaskTracker())
	if _, err := tool.Execute(context.Background(),
		json.RawMessage(`{"task_id":"task-x-1"}`)); err == nil {
		t.Fatal("未知 task_id 应报错")
	}
}
