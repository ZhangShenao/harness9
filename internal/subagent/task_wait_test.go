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

// TestTaskWaitToolTerminalStateRendering 验证终态渲染按 State 三分：
// Done→完成、Failed→失败、Cancelled→已取消。主动取消（主代理决策）与
// 出错（子代理失败）的后续处置不同（重跑 vs 读原因排查），LLM 必须能区分。
func TestTaskWaitToolTerminalStateRendering(t *testing.T) {
	tr := NewTaskTracker()
	idDone := tr.Start("explorer", "探索", "p1")
	tr.Finish(idDone, "正常结论", false)
	idFailed := tr.Start("builder", "构建", "p2")
	tr.Finish(idFailed, "构建出错", true)
	idCancelled := tr.Start("reviewer", "审查", "p3")
	tr.Cancel(idCancelled, "方向错了")

	tool := NewTaskWaitTool(tr, context.Background())
	out, err := tool.Execute(context.Background(), json.RawMessage(
		`{"task_ids":["`+idDone+`","`+idFailed+`","`+idCancelled+`"],"timeout_sec":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "完成 "+idDone) {
		t.Fatalf("Done 任务应渲染为\"完成\":\n%s", out)
	}
	if !strings.Contains(out, "失败 "+idFailed) {
		t.Fatalf("Failed 任务应渲染为\"失败\":\n%s", out)
	}
	if !strings.Contains(out, "已取消 "+idCancelled) {
		t.Fatalf("Cancelled 任务应渲染为\"已取消\"而非\"失败\":\n%s", out)
	}
}

// TestTaskWaitToolUnknownExplicitID 验证显式传入的 task_id 不存在时返回 Go error
// （与 task_status 一致），而非静默跳过——typo 的 id 否则会把等待变成对空集合的空转。
func TestTaskWaitToolUnknownExplicitID(t *testing.T) {
	tr := NewTaskTracker()
	id := tr.Start("explorer", "探索", "p")
	tool := NewTaskWaitTool(tr, context.Background())
	if _, err := tool.Execute(context.Background(),
		json.RawMessage(`{"task_ids":["task-ghost-1"],"timeout_sec":1}`)); err == nil {
		t.Fatal("显式传入不存在的 task_id 应返回 error")
	}
	// 混合列表中任一未知 id 同样报错（逐 id 校验）
	if _, err := tool.Execute(context.Background(),
		json.RawMessage(`{"task_ids":["`+id+`","task-ghost-2"],"timeout_sec":1}`)); err == nil {
		t.Fatal("混合列表含未知 id 应返回 error")
	}
}
