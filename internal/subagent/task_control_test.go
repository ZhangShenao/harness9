// task_control 工具单元测试。
package subagent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestTaskControlToolActions 验证四种 action 的成功文案与 steer 必填校验。
func TestTaskControlToolActions(t *testing.T) {
	tr := NewTaskTracker()
	ctl := NewTaskController(nil)
	id := tr.Start("explorer", "探索", "p")
	tr.Attach(id, ctl)
	tool := NewTaskControlTool(tr)

	cases := []struct {
		name, args, wantSub string
	}{
		{"pause", `{"task_id":"` + id + `","action":"pause"}`, "已暂停"},
		{"steer 缺 message", `{"task_id":"` + id + `","action":"steer"}`, "需要"},
		{"steer", `{"task_id":"` + id + `","action":"steer","message":"只看 engine 包"}`, "转向指令"},
		{"resume", `{"task_id":"` + id + `","action":"resume"}`, "已恢复"},
		{"cancel", `{"task_id":"` + id + `","action":"cancel","message":"方向错了"}`, "取消"},
	}
	for _, c := range cases {
		out, err := tool.Execute(context.Background(), json.RawMessage(c.args))
		if err != nil {
			t.Fatalf("%s: 意外 error: %v", c.name, err)
		}
		if !strings.Contains(out, c.wantSub) {
			t.Fatalf("%s: 输出 %q 不含 %q", c.name, out, c.wantSub)
		}
	}
	if ctl.State() != TaskCancelled {
		t.Fatal("cancel 后应为 Cancelled")
	}

	// 终态任务操作返回可读文本（非 Go error），LLM 可自行调整
	out, err := tool.Execute(context.Background(),
		json.RawMessage(`{"task_id":"`+id+`","action":"pause"}`))
	if err != nil {
		t.Fatalf("终态操作不应返回 Go error: %v", err)
	}
	if !strings.Contains(out, "已结束") {
		t.Fatalf("终态操作输出不符: %s", out)
	}
}

// TestTaskControlToolBadArgs 验证参数校验。
func TestTaskControlToolBadArgs(t *testing.T) {
	tool := NewTaskControlTool(NewTaskTracker())
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"action":"pause"}`)); err == nil {
		t.Fatal("缺 task_id 应报错")
	}
	if _, err := tool.Execute(context.Background(),
		json.RawMessage(`{"task_id":"task-x-1","action":"fly"}`)); err == nil {
		t.Fatal("未知 action 应报错")
	}
}
