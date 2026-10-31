// Package subagent — TaskController：后台子代理任务的控制平面。
//
// TaskController 实现 engine.TaskGate，是主 agent 对运行中后台子代理施加控制
// （暂停/恢复/取消/转向）的唯一媒介。核心语义（spec §5.1/§5.2）：
//   - 暂停为 Turn 边界门控：子引擎每轮开始前调用 AwaitTurn，暂停期间阻塞、
//     不消耗 MaxTurns 配额；进行中的 LLM 调用与工具执行跑完当前轮后停住
//   - 转向消息进入信箱，引擎放行时取出并以 user 角色持久化到子代理历史；
//     任务在取走前结束则消息丢弃（best-effort，spec §10）
//   - 取消经 execCtx cancel 立即生效（与用户 Ctrl+C 同语义），并唤醒暂停中的门控
package subagent

import (
	"context"
	"fmt"
	"sync"

	"github.com/harness9/internal/engine"
	"github.com/harness9/internal/schema"
)

// TaskController 是单个后台子代理任务的控制器。所有方法并发安全；
// Pause/Resume/Cancel 对非目标状态幂等（无操作返回 nil）。
type TaskController struct {
	mu           sync.Mutex
	state        TaskState
	resumeCh     chan struct{}      // 非 nil 表示暂停中；Resume/Cancel 时 close 唤醒全部等待者
	steerBox     []string           // 待投递的转向消息（一次取走即消费）
	cancelReason string             // 取消原因（CancelReason 供 tracker 注入结果）
	cancelExec   context.CancelFunc // Runner 派生 execCtx 后经 bindExec 注入
	emit         func(schema.SubAgentUpdate)
}

// NewTaskController 创建 Running 状态的控制器。emit 可为 nil（无进度上报）。
func NewTaskController(emit func(schema.SubAgentUpdate)) *TaskController {
	return &TaskController{state: TaskRunning, emit: emit}
}

// 编译期断言：TaskController 实现 engine.TaskGate（Runner 经 engine.WithTaskGate
// 注入子引擎），防止接口漂移。
var _ engine.TaskGate = (*TaskController)(nil)

// bindExec 绑定子代理执行 ctx 的 cancel 函数（Runner.Run 派生 execCtx 后调用，
// 包内契约：仅 Runner 与本包测试使用）。
func (c *TaskController) bindExec(cancel context.CancelFunc) {
	c.mu.Lock()
	c.cancelExec = cancel
	alreadyCancelled := c.state == TaskCancelled
	c.mu.Unlock()
	// Cancel 早于 bindExec 到达（sandbox 创建等窗口）：补发取消，防止 lost-cancel
	if alreadyCancelled {
		cancel()
	}
}

// Pause 暂停任务（Turn 边界门控）。非 Running 状态（含终态）无操作。
func (c *TaskController) Pause() error {
	c.mu.Lock()
	if c.state != TaskRunning {
		c.mu.Unlock()
		return nil
	}
	c.state = TaskPaused
	c.resumeCh = make(chan struct{})
	emit := c.emit
	c.mu.Unlock()
	if emit != nil {
		emit(schema.SubAgentUpdate{Kind: schema.SubAgentPaused})
	}
	return nil
}

// Resume 恢复已暂停的任务。非 Paused 状态无操作。
func (c *TaskController) Resume() error {
	c.mu.Lock()
	if c.state != TaskPaused {
		c.mu.Unlock()
		return nil
	}
	c.state = TaskRunning
	if c.resumeCh != nil {
		close(c.resumeCh)
		c.resumeCh = nil
	}
	emit := c.emit
	c.mu.Unlock()
	if emit != nil {
		emit(schema.SubAgentUpdate{Kind: schema.SubAgentResumed})
	}
	return nil
}

// Cancel 请求取消：cancel 子代理 execCtx（立即生效于 in-flight 调用），同时
// 唤醒暂停中的门控等待。终态任务无操作。reason 记录取消原因供结果注入。
func (c *TaskController) Cancel(reason string) error {
	c.mu.Lock()
	if isTerminalTaskState(c.state) {
		c.mu.Unlock()
		return nil
	}
	c.state = TaskCancelled
	c.cancelReason = reason
	if c.resumeCh != nil {
		close(c.resumeCh)
		c.resumeCh = nil
	}
	cancel, emit := c.cancelExec, c.emit
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if emit != nil {
		emit(schema.SubAgentUpdate{Kind: schema.SubAgentCancelled, Text: reason})
	}
	return nil
}

// Steer 追加一条转向消息到信箱，子代理下一轮开始时取出。不改变运行状态：
// 暂停中的任务不因 Steer 自动恢复（恢复由主 agent 显式 resume，职责分离）。
// 终态任务返回错误。
func (c *TaskController) Steer(msg string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if isTerminalTaskState(c.state) {
		return fmt.Errorf("任务已结束（%s），无法转向", c.state)
	}
	c.steerBox = append(c.steerBox, msg)
	return nil
}

// State 返回当前状态。
func (c *TaskController) State() TaskState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

// CancelReason 返回取消原因（未取消时为空串）。
func (c *TaskController) CancelReason() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cancelReason
}

// Finish 标记终态（err==nil → Done，否则 Failed）。Runner 是第一调用方
// （RunStream 错误路径与正常完成两个标记点），TaskTool 为兜底调用方。
// 已处于终态（如 Cancelled）时无操作——终态不可迁移。
func (c *TaskController) Finish(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if isTerminalTaskState(c.state) {
		return
	}
	if err != nil {
		c.state = TaskFailed
	} else {
		c.state = TaskDone
	}
}

// AwaitTurn 实现 engine.TaskGate：暂停时阻塞直到 Resume/Cancel 或 ctx 结束，
// 放行时取出信箱中的全部转向消息（一次取走即消费）。
func (c *TaskController) AwaitTurn(ctx context.Context) ([]string, error) {
	c.mu.Lock()
	ch := c.resumeCh
	c.mu.Unlock()
	if ch != nil {
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	steer := c.steerBox
	c.steerBox = nil
	return steer, nil
}

// isTerminalTaskState 判断是否终态。
func isTerminalTaskState(s TaskState) bool {
	return s == TaskCancelled || s == TaskDone || s == TaskFailed
}
