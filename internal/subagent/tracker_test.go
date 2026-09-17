package subagent

import (
	"sync"
	"testing"

	"github.com/harness9/internal/schema"
)

func TestTrackerStartListRunning(t *testing.T) {
	tr := NewTaskTracker()
	id := tr.Start("explorer", "", "梳理 internal/engine")
	if id == "" {
		t.Fatal("Start 应返回非空 id")
	}
	if tr.RunningCount() != 1 || tr.DoneCount() != 0 {
		t.Fatalf("RunningCount=%d DoneCount=%d, want 1/0", tr.RunningCount(), tr.DoneCount())
	}
	list := tr.List()
	if len(list) != 1 || list[0].AgentName != "explorer" || list[0].State != TaskRunning {
		t.Fatalf("List=%+v", list)
	}
	if list[0].Prompt != "梳理 internal/engine" {
		t.Fatalf("Prompt=%q", list[0].Prompt)
	}
}

func TestTrackerAppendLogAndGet(t *testing.T) {
	tr := NewTaskTracker()
	id := tr.Start("explorer", "", "p")
	tr.AppendLog(id, schema.SubAgentUpdate{Kind: schema.SubAgentToolStart, ToolName: "bash", Text: `{"command":"ls"}`})
	tr.AppendLog(id, schema.SubAgentUpdate{Kind: schema.SubAgentDelta, Text: "hello"})
	d, ok := tr.Get(id)
	if !ok {
		t.Fatal("Get 应命中")
	}
	if len(d.Log) != 2 || d.Log[0].ToolName != "bash" || d.Log[1].Text != "hello" {
		t.Fatalf("Log=%+v", d.Log)
	}
	d.Log[0].ToolName = "mutated"
	d2, _ := tr.Get(id)
	if d2.Log[0].ToolName != "bash" {
		t.Fatal("Get 应返回 Log 的深拷贝")
	}
}

func TestTrackerFinishAndDrainCompleted(t *testing.T) {
	tr := NewTaskTracker()
	id := tr.Start("explorer", "", "p")
	tr.Finish(id, "最终结果", false)
	if tr.RunningCount() != 0 || tr.DoneCount() != 1 {
		t.Fatalf("Running=%d Done=%d", tr.RunningCount(), tr.DoneCount())
	}
	d, _ := tr.Get(id)
	if d.State != TaskDone || d.FinalText != "最终结果" {
		t.Fatalf("detail=%+v", d)
	}
	done := tr.DrainCompleted()
	if len(done) != 1 || done[0].FinalText != "最终结果" || done[0].AgentName != "explorer" {
		t.Fatalf("DrainCompleted=%+v", done)
	}
	if len(tr.DrainCompleted()) != 0 {
		t.Fatal("已注入的结果不应被重复 Drain")
	}
	if len(tr.List()) != 1 {
		t.Fatal("Drain 不应从 List 移除任务")
	}
}

func TestTrackerFinishError(t *testing.T) {
	tr := NewTaskTracker()
	id := tr.Start("x", "", "p")
	tr.Finish(id, "boom", true)
	d, _ := tr.Get(id)
	if d.State != TaskFailed {
		t.Fatalf("State=%v, want TaskFailed", d.State)
	}
	done := tr.DrainCompleted()
	if len(done) != 1 || !done[0].IsError {
		t.Fatalf("DrainCompleted=%+v", done)
	}
}

func TestTrackerSetNotify(t *testing.T) {
	tr := NewTaskTracker()
	var n int
	tr.SetNotify(func() { n++ })
	id := tr.Start("x", "", "p")
	tr.Finish(id, "r", false)
	if n != 1 {
		t.Fatalf("notify 应在 Finish 时触发 1 次，得 %d", n)
	}
}

func TestTrackerConcurrent(t *testing.T) {
	tr := NewTaskTracker()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := tr.Start("a", "", "p")
			tr.AppendLog(id, schema.SubAgentUpdate{Kind: schema.SubAgentDelta, Text: "x"})
			tr.Finish(id, "done", false)
		}()
	}
	wg.Wait()
	if tr.DoneCount() != 20 {
		t.Fatalf("DoneCount=%d, want 20", tr.DoneCount())
	}
}

// TestTrackerControlRouting 验证 Control 路由到 controller：pause/resume 生效、
// 终态任务操作被拒绝、steer 缺 message 报错。
func TestTrackerControlRouting(t *testing.T) {
	tr := NewTaskTracker()
	ctl := NewTaskController(nil)
	id := tr.Start("explorer", "探索任务", "找到所有调用点")
	tr.Attach(id, ctl)

	if err := tr.Control(id, "pause", ""); err != nil {
		t.Fatal(err)
	}
	if ctl.State() != TaskPaused {
		t.Fatal("Control(pause) 未生效")
	}
	if err := tr.Control(id, "steer", ""); err == nil {
		t.Fatal("steer 缺 message 应报错")
	}
	if err := tr.Control(id, "steer", "新方向"); err != nil {
		t.Fatal(err)
	}
	if err := tr.Control(id, "resume", ""); err != nil {
		t.Fatal(err)
	}
	if err := tr.Control(id, "bogus", ""); err == nil {
		t.Fatal("未知 action 应报错")
	}
	if err := tr.Control("task-x-none", "pause", ""); err == nil {
		t.Fatal("不存在的任务应报错")
	}

	tr.Finish(id, "done", false)
	if err := tr.Control(id, "pause", ""); err == nil {
		t.Fatal("终态任务应拒绝控制")
	}
}

// TestTrackerCancelDistinctFromFinish 验证 Cancel 标记 TaskCancelled 并触发
// DrainCompleted（isError=true），与 Finish 的 Failed 区分；且 Cancel 后 Finish
// 不得覆写终态（防 Cancel→panic-recover-Finish 竞态把 Cancelled 改写为 Done/Failed）。
func TestTrackerCancelDistinctFromFinish(t *testing.T) {
	tr := NewTaskTracker()
	id := tr.Start("explorer", "探索", "prompt")
	tr.Cancel(id, "方向错误")
	got := tr.DrainCompleted()
	if len(got) != 1 || got[0].IsError != true || got[0].FinalText != "方向错误" {
		t.Fatalf("Cancel 后 Drain 不符: %+v", got)
	}
	if d, ok := tr.Get(id); !ok || d.State != TaskCancelled {
		t.Fatal("状态应为 TaskCancelled")
	}
	// 已终态的任务再 Finish 不覆写（终态守卫）
	tr.Finish(id, "迟到的完成", false)
	d, _ := tr.Get(id)
	if d.State != TaskCancelled || d.FinalText != "方向错误" {
		t.Fatalf("Cancel 后 Finish 不应覆写终态: State=%v FinalText=%q", d.State, d.FinalText)
	}
}

// TestTrackerMarkInjectedDedup 验证 task_status/task_wait 消费路径的防重注入。
func TestTrackerMarkInjectedDedup(t *testing.T) {
	tr := NewTaskTracker()
	id := tr.Start("explorer", "探索", "prompt")
	tr.Finish(id, "结果文本", false)
	tr.MarkInjected(id)
	if got := tr.DrainCompleted(); len(got) != 0 {
		t.Fatalf("MarkInjected 后不应再 Drain 出结果: %+v", got)
	}
}

// TestTrackerPausedNotDrained 验证暂停中的任务不被 Drain（仍 Running 语义）。
func TestTrackerPausedNotDrained(t *testing.T) {
	tr := NewTaskTracker()
	id := tr.Start("explorer", "探索", "prompt")
	ctl := NewTaskController(nil)
	tr.Attach(id, ctl)
	_ = tr.Control(id, "pause", "")
	if got := tr.DrainCompleted(); len(got) != 0 {
		t.Fatalf("暂停中不应 Drain: %+v", got)
	}
	if tr.RunningCount() != 1 {
		t.Fatal("暂停任务仍计入运行计数（供状态栏展示）")
	}
}

// TestTrackerSnapshotFields 验证快照携带 Description/Elapsed/LastActivity。
func TestTrackerSnapshotFields(t *testing.T) {
	tr := NewTaskTracker()
	id := tr.Start("explorer", "梳理调用关系", "很长的 prompt")
	tr.AppendLog(id, schema.SubAgentUpdate{Kind: schema.SubAgentToolStart, ToolName: "grep"})
	snap := tr.List()
	if len(snap) != 1 {
		t.Fatal("应有 1 个任务")
	}
	if snap[0].Description != "梳理调用关系" || snap[0].LastActivity != "调用工具 grep" {
		t.Fatalf("快照字段不符: %+v", snap[0])
	}
	if snap[0].Elapsed <= 0 {
		t.Fatal("运行中任务 Elapsed 应大于 0")
	}
}
