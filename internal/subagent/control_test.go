// TaskController 单元测试：状态机迁移、幂等性、门控阻塞语义与并发安全。
package subagent

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/harness9/internal/schema"
)

// TestTaskControllerPauseResumeGate 验证暂停后 AwaitTurn 阻塞、恢复后放行。
func TestTaskControllerPauseResumeGate(t *testing.T) {
	c := NewTaskController(nil)
	if c.State() != TaskRunning {
		t.Fatal("初始态应为 Running")
	}
	if err := c.Pause(); err != nil {
		t.Fatal(err)
	}
	if c.State() != TaskPaused {
		t.Fatal("Pause 后应为 Paused")
	}

	done := make(chan error, 1)
	go func() {
		_, err := c.AwaitTurn(context.Background())
		done <- err
	}()
	select {
	case <-done:
		t.Fatal("暂停中 AwaitTurn 不应返回")
	case <-time.After(50 * time.Millisecond):
	}
	if err := c.Resume(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if c.State() != TaskRunning {
		t.Fatal("Resume 后应为 Running")
	}
}

// TestTaskControllerSteerMailbox 验证转向消息一次取走即消费；终态后 Steer 报错。
func TestTaskControllerSteerMailbox(t *testing.T) {
	c := NewTaskController(nil)
	if err := c.Steer("第一条"); err != nil {
		t.Fatal(err)
	}
	if err := c.Steer("第二条"); err != nil {
		t.Fatal(err)
	}
	got, err := c.AwaitTurn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "第一条" || got[1] != "第二条" {
		t.Fatalf("信箱内容不符: %v", got)
	}
	again, _ := c.AwaitTurn(context.Background())
	if len(again) != 0 {
		t.Fatalf("信箱应已清空: %v", again)
	}
}

// TestTaskControllerCancelWakesGateAndCtx 验证取消：唤醒暂停中的门控等待、
// 调用绑定的 execCtx cancel、状态进入 Cancelled 且记录原因；终态操作幂等。
func TestTaskControllerCancelWakesGateAndCtx(t *testing.T) {
	c := NewTaskController(nil)
	cancelled := false
	c.bindExec(func() { cancelled = true })

	if err := c.Pause(); err != nil {
		t.Fatal(err)
	}
	gateDone := make(chan error, 1)
	go func() {
		_, err := c.AwaitTurn(context.Background()) // 取消应经 resumeCh 关闭唤醒而非 ctx
		gateDone <- err
	}()
	time.Sleep(20 * time.Millisecond)

	if err := c.Cancel("方向错误"); err != nil {
		t.Fatal(err)
	}
	if err := <-gateDone; err != nil {
		t.Fatalf("取消后门控应放行（无错误）: %v", err)
	}
	if !cancelled {
		t.Fatal("Cancel 应调用绑定的 execCtx cancel")
	}
	if c.State() != TaskCancelled || c.CancelReason() != "方向错误" {
		t.Fatal("状态/原因不符")
	}
	// 幂等与终态保护
	if err := c.Cancel("again"); err != nil {
		t.Fatal(err)
	}
	if err := c.Pause(); err != nil || c.State() != TaskCancelled {
		t.Fatal("终态任务 Pause 应无操作")
	}
	if err := c.Steer("late"); err == nil {
		t.Fatal("终态任务 Steer 应报错")
	}
	c.Finish(nil) // 不应把 Cancelled 覆盖为 Done
	if c.State() != TaskCancelled {
		t.Fatal("终态不可迁移")
	}
}

// TestTaskControllerEmitAndFinish 验证状态迁移 emit 进度、Finish 区分 Done/Failed。
func TestTaskControllerEmitAndFinish(t *testing.T) {
	var mu sync.Mutex
	var kinds []schema.SubAgentUpdateKind
	emit := func(u schema.SubAgentUpdate) {
		mu.Lock()
		kinds = append(kinds, u.Kind)
		mu.Unlock()
	}
	c := NewTaskController(emit)
	_ = c.Pause()
	_ = c.Resume()
	_ = c.Cancel("bye")
	mu.Lock()
	defer mu.Unlock()
	want := []schema.SubAgentUpdateKind{schema.SubAgentPaused, schema.SubAgentResumed, schema.SubAgentCancelled}
	if len(kinds) != len(want) {
		t.Fatalf("emit 序列不符: %v", kinds)
	}
	for i, k := range want {
		if kinds[i] != k {
			t.Fatalf("emit[%d] = %s, want %s", i, kinds[i], k)
		}
	}

	c2 := NewTaskController(nil)
	c2.Finish(nil)
	if c2.State() != TaskDone {
		t.Fatal("Finish(nil) 应为 Done")
	}
}
