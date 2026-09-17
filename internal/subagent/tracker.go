// Package subagent — TaskTracker：后台子代理任务的线程安全单一事实源。
// 本文件实现 TaskTracker，管理后台（background=true）子代理任务的完整生命周期。
// 写入路径（Start/AppendLog/Finish/Cancel/MarkInjected）来自后台 goroutine 与工具路径；
// 读取路径（List/Get/DrainCompleted/RunningCount）来自 TUI goroutine；
// 控制路径（Control）由主 agent 的 task_control 工具与 TUI 面板调用。
// 所有操作均通过 sync.Mutex 保护，不使用 channel，避免 send-on-closed-channel 风险。
package subagent

import (
	"fmt"
	"sync"
	"time"

	"github.com/harness9/internal/schema"
)

// CompletedTask 是一个已完成后台子代理任务的结果（供 DrainCompleted 注入 LLM）。
type CompletedTask struct {
	TaskID    string
	AgentName string
	FinalText string
	IsError   bool
}

// TaskState 后台子代理任务状态。
type TaskState int

const (
	// TaskRunning 运行中。
	TaskRunning TaskState = iota
	// TaskPaused 已暂停（Turn 边界门控）。
	TaskPaused
	// TaskDone 正常完成。
	TaskDone
	// TaskFailed 出错结束。
	TaskFailed
	// TaskCancelled 被主代理取消。
	TaskCancelled
)

// String 返回状态可读名（用于 TUI 展示）。
func (s TaskState) String() string {
	switch s {
	case TaskRunning:
		return "运行中"
	case TaskPaused:
		return "已暂停"
	case TaskDone:
		return "完成"
	case TaskFailed:
		return "失败"
	case TaskCancelled:
		return "已取消"
	default:
		return "未知"
	}
}

// TaskSnapshot 是面板列表用的只读快照。
type TaskSnapshot struct {
	ID           string
	AgentName    string
	Description  string
	Prompt       string
	State        TaskState
	LogLines     int
	Elapsed      time.Duration
	LastActivity string
}

// TaskDetail 是详情视图用的只读快照（含全过程日志拷贝）。
type TaskDetail struct {
	ID          string
	AgentName   string
	Description string
	Prompt      string
	FinalText   string
	State       TaskState
	Elapsed     time.Duration
	Log         []schema.SubAgentUpdate
}

// bgTask 是单个后台任务的内部记录。
type bgTask struct {
	id           string
	agentName    string
	description  string // 任务简短标题（task 工具的 description 参数）
	prompt       string
	state        TaskState
	log          []schema.SubAgentUpdate
	finalText    string
	isError      bool
	injected     bool
	controller   *TaskController // 后台任务的控制平面（Start 后由 TaskTool Attach）
	startedAt    time.Time
	finishedAt   time.Time
	lastActivity string // 最近一条进度摘要（≤80 runes，供 status 面板）
}

// TaskTracker 是后台子代理任务的线程安全单一事实源：
//   - 后台 goroutine：Start → AppendLog* → Finish
//   - TUI：List/Get（面板）、DrainCompleted（注入 LLM）、SetNotify（完成提示）、RunningCount/DoneCount（状态栏）
//
// 全过程日志写入内存缓冲（加锁），不经任何 channel，故不存在 send-on-closed-channel 风险。
type TaskTracker struct {
	mu     sync.Mutex
	tasks  []*bgTask // 按创建顺序
	seq    int
	notify func()
}

// NewTaskTracker 创建空 tracker。
func NewTaskTracker() *TaskTracker {
	return &TaskTracker{}
}

// Start 注册一个 Running 任务（agentName 子代理类型、description 简短标题、
// prompt 完整任务描述），返回唯一 id。
func (t *TaskTracker) Start(agentName, description, prompt string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.seq++
	id := fmt.Sprintf("task-%s-%d", agentName, t.seq)
	t.tasks = append(t.tasks, &bgTask{
		id: id, agentName: agentName, description: description, prompt: prompt,
		state: TaskRunning, startedAt: time.Now(),
	})
	return id
}

// AppendLog 向指定任务追加一条进度日志。任务不存在时静默忽略。
func (t *TaskTracker) AppendLog(id string, u schema.SubAgentUpdate) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if task := t.find(id); task != nil {
		task.log = append(task.log, u)
		task.lastActivity = summarizeUpdate(u)
	}
}

// Finish 标记任务完成（isErr 决定 Done/Failed），记录最终文本，并触发完成通知。
// 已终态（Done/Failed/Cancelled）的任务不可覆写——防 Cancel→panic-recover-Finish
// 竞态窗口把 Cancelled 终态改写。
func (t *TaskTracker) Finish(id, finalText string, isErr bool) {
	t.mu.Lock()
	if task := t.find(id); task != nil {
		if isTerminalTaskState(task.state) {
			// 已终态不可覆写（如 Cancel 后迟到的 Finish）
		} else {
			task.finalText = finalText
			task.isError = isErr
			if isErr {
				task.state = TaskFailed
			} else {
				task.state = TaskDone
			}
			task.finishedAt = time.Now()
		}
	}
	notify := t.notify
	t.mu.Unlock()
	if notify != nil {
		notify() // 锁外调用，避免回调重入死锁
	}
}

// Attach 把控制器挂接到任务（TaskTool 在后台 goroutine 启动前调用）。
func (t *TaskTracker) Attach(id string, c *TaskController) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if task := t.find(id); task != nil {
		task.controller = c
	}
}

// Control 是主 agent（task_control 工具）与 TUI 面板操作后台任务的唯一入口：
// 校验任务存在与终态后路由到 controller。action ∈ pause/resume/cancel/steer。
// pause/resume 成功后同步把非终态落账到 bgTask.state（syncNonTerminalState），
// 使 List/Get 快照立即反映暂停/恢复——面板状态色与 task_status 的 [已暂停]
// 输出依赖快照状态；cancel/终态仍由 TaskTool goroutine 异步落账（含收尾字段）。
// 终态判定双源：bgTask 已落账状态优先，controller 实时终态兜底——ctl.Cancel/Finish
// 先行、tracker 随后异步落账（TaskTool goroutine）的窗口期内，控制请求不得被
// 误路由到 ctl 的幂等 no-op 而向 LLM 报成功。锁序恒为 tracker.mu → ctl.mu
// （controller 不回调 tracker），无死锁风险。
func (t *TaskTracker) Control(id, action, message string) error {
	t.mu.Lock()
	task := t.find(id)
	if task == nil {
		t.mu.Unlock()
		return fmt.Errorf("任务 %q 不存在", id)
	}
	state := task.state
	if !isTerminalTaskState(state) && task.controller != nil {
		if cs := task.controller.State(); isTerminalTaskState(cs) {
			state = cs
		}
	}
	if isTerminalTaskState(state) {
		err := fmt.Errorf("任务 %s 已结束（%s），无法执行 %s", id, state, action)
		t.mu.Unlock()
		return err
	}
	ctl := task.controller
	t.mu.Unlock()
	if ctl == nil {
		return fmt.Errorf("任务 %q 未挂接控制器", id)
	}
	switch action {
	case "pause":
		if err := ctl.Pause(); err != nil {
			return err
		}
		t.syncNonTerminalState(id, ctl)
		return nil
	case "resume":
		if err := ctl.Resume(); err != nil {
			return err
		}
		t.syncNonTerminalState(id, ctl)
		return nil
	case "cancel":
		return ctl.Cancel(message)
	case "steer":
		if message == "" {
			return fmt.Errorf("steer 需要非空 message 参数")
		}
		return ctl.Steer(message)
	default:
		return fmt.Errorf("未知 action %q（可用: pause/resume/cancel/steer）", action)
	}
}

// syncNonTerminalState 把 controller 的非终态（Running/Paused）同步落账到
// bgTask.state。仅接受非终态 ctl 状态——Cancel 竞态窗口内（ctl.Pause 成功后、
// 本函数读取前 ctl 被并发 Cancel/Finish）ctl 已终态则跳过，留给 TaskTool
// goroutine 的 tracker.Cancel/Finish 异步落账，避免提前终态且丢失 finalText/
// finishedAt 收尾字段。锁序：ctl.State()（ctl.mu 短暂持有后释放）→ tracker.mu，
// 两次独立获取不构成嵌套（controller 不回调 tracker），无死锁风险。
func (t *TaskTracker) syncNonTerminalState(id string, ctl *TaskController) {
	if ns := ctl.State(); ns == TaskRunning || ns == TaskPaused {
		t.mu.Lock()
		if task := t.find(id); task != nil && !isTerminalTaskState(task.state) {
			task.state = ns
		}
		t.mu.Unlock()
	}
}

// Cancel 标记任务为 TaskCancelled（区别于 Finish 的 Failed），记录原因并通知。
// 由 TaskTool 后台 goroutine 在检测到 controller 取消后调用。
func (t *TaskTracker) Cancel(id, reason string) {
	t.mu.Lock()
	if task := t.find(id); task != nil && !isTerminalTaskState(task.state) {
		task.state = TaskCancelled
		task.finalText = reason
		task.isError = true
		task.finishedAt = time.Now()
	}
	notify := t.notify
	t.mu.Unlock()
	if notify != nil {
		notify()
	}
}

// SetNotify 设置完成通知回调（TUI 注入），Finish 时触发。
func (t *TaskTracker) SetNotify(fn func()) {
	t.mu.Lock()
	t.notify = fn
	t.mu.Unlock()
}

// DrainCompleted 返回已完成但尚未注入 LLM 的任务结果，并标记为已注入（幂等，不影响 List）。
// 完成条件为终态（Done/Failed/Cancelled）——Paused 任务不 Drain（仍属运行语义）。
func (t *TaskTracker) DrainCompleted() []CompletedTask {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []CompletedTask
	for _, task := range t.tasks {
		if isTerminalTaskState(task.state) && !task.injected {
			task.injected = true
			out = append(out, CompletedTask{
				TaskID:    task.id,
				AgentName: task.agentName,
				FinalText: task.finalText,
				IsError:   task.isError,
			})
		}
	}
	return out
}

// MarkInjected 标记结果已消费（task_status/task_wait 路径），与 DrainCompleted
// 共用 injected 标志，保证结果恰好注入一次。
func (t *TaskTracker) MarkInjected(ids ...string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, id := range ids {
		if task := t.find(id); task != nil {
			task.injected = true
		}
	}
}

// List 返回所有任务的快照（创建顺序）。
func (t *TaskTracker) List() []TaskSnapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]TaskSnapshot, len(t.tasks))
	for i, task := range t.tasks {
		elapsed := time.Since(task.startedAt)
		if !task.finishedAt.IsZero() {
			elapsed = task.finishedAt.Sub(task.startedAt)
		}
		out[i] = TaskSnapshot{
			ID: task.id, AgentName: task.agentName, Description: task.description,
			Prompt: task.prompt, State: task.state, LogLines: len(task.log),
			Elapsed: elapsed, LastActivity: task.lastActivity,
		}
	}
	return out
}

// Get 返回单个任务详情（Log 深拷贝，避免与后台写入竞态）。
func (t *TaskTracker) Get(id string) (TaskDetail, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	task := t.find(id)
	if task == nil {
		return TaskDetail{}, false
	}
	logCopy := make([]schema.SubAgentUpdate, len(task.log))
	copy(logCopy, task.log)
	elapsed := time.Since(task.startedAt)
	if !task.finishedAt.IsZero() {
		elapsed = task.finishedAt.Sub(task.startedAt)
	}
	return TaskDetail{
		ID: task.id, AgentName: task.agentName, Description: task.description,
		Prompt: task.prompt, FinalText: task.finalText, State: task.state,
		Elapsed: elapsed, Log: logCopy,
	}, true
}

// RunningCount 返回活跃（非终态）任务数：运行中 + 已暂停。
// 暂停仍属运行语义（TestTrackerPausedNotDrained 锁定"暂停任务仍计入运行计数"），
// 且按非终态计数避免"唯一任务暂停 → N=0 → 状态栏任务段整段隐藏"的展示缺口。
func (t *TaskTracker) RunningCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, task := range t.tasks {
		if !isTerminalTaskState(task.state) {
			n++
		}
	}
	return n
}

// DoneCount 返回已结束（完成 + 失败 + 取消）任务数。
// 终态判定——Paused 任务不计入（仍属运行语义，供状态栏区分展示）。
func (t *TaskTracker) DoneCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, task := range t.tasks {
		if isTerminalTaskState(task.state) {
			n++
		}
	}
	return n
}

// find 按 id 查找（调用方须持锁）。
func (t *TaskTracker) find(id string) *bgTask {
	for _, task := range t.tasks {
		if task.id == id {
			return task
		}
	}
	return nil
}

// summarizeUpdate 生成进度摘要（≤80 runes）。
func summarizeUpdate(u schema.SubAgentUpdate) string {
	switch u.Kind {
	case schema.SubAgentToolStart:
		return "调用工具 " + u.ToolName
	case schema.SubAgentToolResult:
		if u.IsError {
			return "工具执行出错"
		}
		return "工具完成 " + u.ToolName
	case schema.SubAgentDelta, schema.SubAgentThinking:
		return truncateRunes(u.Text, 60)
	case schema.SubAgentError:
		return "出错: " + truncateRunes(u.Text, 60)
	default:
		return string(u.Kind)
	}
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
