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
	if !strings.Contains(out, id1) {
		t.Fatalf("单任务输出不符:\n%s", out)
	}

	// 回归（Important-1）：运行中任务被单查不得 MarkInjected——
	// Finish 后 DrainCompleted 仍须送达结果，自动注入通道不被切断。
	tr.Finish(id1, "梳理结论 XYZ", false)
	found := false
	for _, c := range tr.DrainCompleted() {
		if c.TaskID == id1 {
			found = true
		}
	}
	if !found {
		t.Fatal("运行中任务被单查后，Finish 的结果不应被切断自动注入")
	}
}

// TestTaskStatusToolSkipsAlreadyInjected 验证已注入感知（spec §7.8"恰好注入一次"）：
// 结果已被 TUI harvest（DrainCompleted）或 task_wait 消费后，task_status 的列表与
// 单查路径均不得重复附 FinalText——只显示状态行 + 省略提示，防止同一结果两次进入
// LLM 上下文。
func TestTaskStatusToolSkipsAlreadyInjected(t *testing.T) {
	tr := NewTaskTracker()
	idA := tr.Start("explorer", "收割", "p1")
	idB := tr.Start("builder", "未收割", "p2")
	tr.Finish(idA, "已被 harvest 的结果", false)
	tr.Finish(idB, "未被收割的结果", false)

	// 模拟 TUI harvest：只消费 A（显式 MarkInjected，等价 DrainCompleted 对 A 的效果）
	tr.MarkInjected(idA)

	tool := NewTaskStatusTool(tr)
	out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "已被 harvest 的结果") {
		t.Fatalf("列表路径不得重复附已注入结果:\n%s", out)
	}
	if !strings.Contains(out, "已注入上下文") || !strings.Contains(out, idA) {
		t.Fatalf("列表路径应输出状态行 + 省略提示:\n%s", out)
	}
	if !strings.Contains(out, "未被收割的结果") {
		t.Fatalf("未收割结果应照旧附文本:\n%s", out)
	}

	out, err = tool.Execute(context.Background(),
		json.RawMessage(`{"task_id":"`+idA+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "已被 harvest 的结果") {
		t.Fatalf("单查路径不得重复附已注入结果:\n%s", out)
	}
	if !strings.Contains(out, "已注入上下文") {
		t.Fatalf("单查路径应输出省略提示:\n%s", out)
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
