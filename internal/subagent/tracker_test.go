package subagent

import (
	"sync"
	"testing"

	"github.com/harness9/internal/schema"
)

func TestTrackerStartListRunning(t *testing.T) {
	tr := NewTaskTracker()
	id := tr.Start("explorer", "梳理 internal/engine")
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
	id := tr.Start("explorer", "p")
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
	id := tr.Start("explorer", "p")
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
	id := tr.Start("x", "p")
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
	id := tr.Start("x", "p")
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
			id := tr.Start("a", "p")
			tr.AppendLog(id, schema.SubAgentUpdate{Kind: schema.SubAgentDelta, Text: "x"})
			tr.Finish(id, "done", false)
		}()
	}
	wg.Wait()
	if tr.DoneCount() != 20 {
		t.Fatalf("DoneCount=%d, want 20", tr.DoneCount())
	}
}
