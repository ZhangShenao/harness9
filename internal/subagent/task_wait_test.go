// task_wait 工具单元测试：join 语义、超时返回快照而非 error。
package subagent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestTaskWaitToolCompletes 验证等待已完成任务立即返回结果并 MarkInjected。
func TestTaskWaitToolCompletes(t *testing.T) {
	tr := NewTaskTracker()
	id := tr.Start("explorer", "探索", "p")
	tr.Finish(id, "最终结论 XYZ", false)

	tool := NewTaskWaitTool(tr, context.Background())
	out, err := tool.Execute(context.Background(),
		json.RawMessage(`{"task_ids":["`+id+`"],"timeout_sec":2}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "最终结论 XYZ") {
		t.Fatalf("输出应含结果:\n%s", out)
	}
	if got := tr.DrainCompleted(); len(got) != 0 {
		t.Fatal("等待消费过的结果不应再被 Drain")
	}
}

// TestTaskWaitToolTimeoutReturnsSnapshot 验证超时返回"仍在运行"快照而非 error。
func TestTaskWaitToolTimeoutReturnsSnapshot(t *testing.T) {
	tr := NewTaskTracker()
	id := tr.Start("explorer", "探索", "p")

	tool := NewTaskWaitTool(tr, context.Background())
	out, err := tool.Execute(context.Background(),
		json.RawMessage(`{"task_ids":["`+id+`"],"timeout_sec":1}`))
	if err != nil {
		t.Fatalf("超时不应返回 error: %v", err)
	}
	if !strings.Contains(out, "仍在运行") || !strings.Contains(out, "等待超时") {
		t.Fatalf("超时输出应含状态快照:\n%s", out)
	}
}

// TestTaskWaitToolNoRunning 验证无目标任务时立即返回提示。
func TestTaskWaitToolNoRunning(t *testing.T) {
	tool := NewTaskWaitTool(NewTaskTracker(), context.Background())
	out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil || !strings.Contains(out, "没有") {
		t.Fatalf("输出不符: %s err=%v", out, err)
	}
}

// TestTaskWaitToolFinishWhileWaiting 验证等待中途任务完成即返回。
func TestTaskWaitToolFinishWhileWaiting(t *testing.T) {
	tr := NewTaskTracker()
	id := tr.Start("explorer", "探索", "p")
	go func() {
		time.Sleep(300 * time.Millisecond)
		tr.Finish(id, "中途完成的结果", false)
	}()
	tool := NewTaskWaitTool(tr, context.Background())
	out, err := tool.Execute(context.Background(),
		json.RawMessage(`{"task_ids":["`+id+`"],"timeout_sec":5}`))
	if err != nil || !strings.Contains(out, "中途完成的结果") {
		t.Fatalf("中途完成应被等到: %s err=%v", out, err)
	}
}
