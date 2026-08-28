// Package subagent — TaskTracker：后台子代理任务的线程安全单一事实源。
// 本文件实现 TaskTracker，管理后台（background=true）子代理任务的完整生命周期。
// 写入路径（Start/AppendLog/Finish）来自后台 goroutine；
// 读取路径（List/Get/DrainCompleted/RunningCount）来自 TUI goroutine。
// 所有操作均通过 sync.Mutex 保护，不使用 channel，避免 send-on-closed-channel 风险。
package subagent

import (
	"fmt"
	"sync"

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
	// TaskDone 正常完成。
	TaskDone
	// TaskFailed 出错结束。
	TaskFailed
)

// String 返回状态可读名（用于 TUI 展示）。
func (s TaskState) String() string {
	switch s {
	case TaskRunning:
		return "运行中"
	case TaskDone:
		return "完成"
	case TaskFailed:
		return "失败"
	default:
		return "未知"
	}
}

// TaskSnapshot 是面板列表用的只读快照。
type TaskSnapshot struct {
	ID        string
	AgentName string
	Prompt    string
	State     TaskState
	LogLines  int
}

// TaskDetail 是详情视图用的只读快照（含全过程日志拷贝）。
type TaskDetail struct {
	ID        string
	AgentName string
	Prompt    string
	FinalText string
	State     TaskState
	Log       []schema.SubAgentUpdate
}

// bgTask 是单个后台任务的内部记录。
type bgTask struct {
	id        string
	agentName string
	prompt    string
	state     TaskState
	log       []schema.SubAgentUpdate
	finalText string
	isError   bool
	injected  bool // 是否已被 DrainCompleted 取走（防止重复注入 LLM）
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

// Start 注册一个 Running 任务，返回唯一 id。
func (t *TaskTracker) Start(agentName, prompt string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.seq++
	id := fmt.Sprintf("task-%s-%d", agentName, t.seq)
	t.tasks = append(t.tasks, &bgTask{id: id, agentName: agentName, prompt: prompt, state: TaskRunning})
	return id
}

// AppendLog 向指定任务追加一条进度日志。任务不存在时静默忽略。
func (t *TaskTracker) AppendLog(id string, u schema.SubAgentUpdate) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if task := t.find(id); task != nil {
		task.log = append(task.log, u)
	}
}

// Finish 标记任务完成（isErr 决定 Done/Failed），记录最终文本，并触发完成通知。
func (t *TaskTracker) Finish(id, finalText string, isErr bool) {
	t.mu.Lock()
	if task := t.find(id); task != nil {
		task.finalText = finalText
		task.isError = isErr
		if isErr {
			task.state = TaskFailed
		} else {
			task.state = TaskDone
		}
	}
	notify := t.notify
	t.mu.Unlock()
	if notify != nil {
		notify() // 锁外调用，避免回调重入死锁
	}
}

// SetNotify 设置完成通知回调（TUI 注入），Finish 时触发。
func (t *TaskTracker) SetNotify(fn func()) {
	t.mu.Lock()
	t.notify = fn
	t.mu.Unlock()
}

// DrainCompleted 返回已完成但尚未注入 LLM 的任务结果，并标记为已注入（幂等，不影响 List）。
func (t *TaskTracker) DrainCompleted() []CompletedTask {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []CompletedTask
	for _, task := range t.tasks {
		if task.state != TaskRunning && !task.injected {
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

// List 返回所有任务的快照（创建顺序）。
func (t *TaskTracker) List() []TaskSnapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]TaskSnapshot, len(t.tasks))
	for i, task := range t.tasks {
		out[i] = TaskSnapshot{
			ID: task.id, AgentName: task.agentName, Prompt: task.prompt,
			State: task.state, LogLines: len(task.log),
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
	return TaskDetail{
		ID: task.id, AgentName: task.agentName, Prompt: task.prompt,
		FinalText: task.finalText, State: task.state, Log: logCopy,
	}, true
}

// RunningCount 返回运行中任务数。
func (t *TaskTracker) RunningCount() int { return t.countState(TaskRunning) }

// DoneCount 返回已结束（完成 + 失败）任务数。
func (t *TaskTracker) DoneCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, task := range t.tasks {
		if task.state != TaskRunning {
			n++
		}
	}
	return n
}

func (t *TaskTracker) countState(s TaskState) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, task := range t.tasks {
		if task.state == s {
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
