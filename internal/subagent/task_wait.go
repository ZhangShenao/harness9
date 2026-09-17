// Package subagent — TaskWaitTool：主代理等待后台子代理任务的 join 原语。
//
// 关键语义（spec §5.4）：
//   - 等待 ctx 从会话级 baseCtx 派生并自带 timeout（clamp [1,600]s），
//     刻意忽略父 Turn 的 60s 工具超时（手法同 Runner.Run 的 execCtx 派生）；
//     用户 Ctrl+C 经 baseCtx 传播结束等待
//   - 超时不是 error：返回未完成任务的当前状态快照，由 LLM 决定继续等或先做别的
//   - 已完成任务结果随返回 MarkInjected（结果恰好注入一次）
package subagent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/harness9/internal/schema"
)

// TaskWaitTool 实现 tools.BaseTool。
type TaskWaitTool struct {
	tracker *TaskTracker
	baseCtx context.Context
}

// NewTaskWaitTool 创建 task_wait 工具。baseCtx 为会话级 ctx（main.go 注入）。
func NewTaskWaitTool(tracker *TaskTracker, baseCtx context.Context) *TaskWaitTool {
	return &TaskWaitTool{tracker: tracker, baseCtx: baseCtx}
}

// Name 返回工具名 "task_wait"。
func (t *TaskWaitTool) Name() string { return "task_wait" }

// Definition 返回工具定义。
func (t *TaskWaitTool) Definition() schema.ToolDefinition {
	return schema.ToolDefinition{
		Name:        "task_wait",
		Description: "等待后台子代理任务完成（join）。默认等待全部运行中任务；timeout_sec 上限 600。超时不报错，返回仍在运行任务的状态快照——可继续等待或先做其他事。",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"task_ids": map[string]any{"type": "array", "items": map[string]any{"type": "string"},
					"description": "要等待的任务 id 列表；省略且 all=true 时等待全部运行中任务"},
				"all":         map[string]any{"type": "boolean", "description": "等待全部运行中任务（默认 true）"},
				"timeout_sec": map[string]any{"type": "integer", "description": "等待上限秒数（默认 60，上限 600）"},
			},
		},
	}
}

type taskWaitArgs struct {
	TaskIDs    []string `json:"task_ids"`
	All        bool     `json:"all"`
	TimeoutSec int      `json:"timeout_sec"`
}

// Execute 阻塞等待目标任务完成或超时。
func (t *TaskWaitTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a taskWaitArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("参数解析失败：%w", err)
	}
	timeout := a.TimeoutSec
	if timeout <= 0 {
		timeout = 60
	}
	if timeout > 600 {
		timeout = 600
	}

	// 目标集合在入口一次确定：显式 id 列表，或当前全部非终态任务。
	targets := a.TaskIDs
	if len(targets) == 0 {
		for _, s := range t.tracker.List() {
			if !isTerminalTaskState(s.State) {
				targets = append(targets, s.ID)
			}
		}
		if len(targets) == 0 {
			return "当前没有正在运行的后台子代理任务。", nil
		}
	} else {
		// 显式 id 逐个校验存在性（与 task_status 同语义）：未命中直接报错，
		// 而非静默跳过——typo 的 id 否则会把等待变成对空集合的空转。
		for _, id := range targets {
			if _, ok := t.tracker.Get(id); !ok {
				return "", fmt.Errorf("任务 %q 不存在", id)
			}
		}
	}

	waitCtx, cancel := context.WithTimeout(t.baseCtx, time.Duration(timeout)*time.Second)
	defer cancel()

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	timedOut := false
	for {
		if t.allDone(targets) {
			break
		}
		select {
		case <-waitCtx.Done():
			timedOut = true
		case <-ticker.C:
		}
		if timedOut {
			break
		}
	}
	return t.formatResult(targets, timedOut), nil
}

func (t *TaskWaitTool) allDone(ids []string) bool {
	for _, id := range ids {
		if d, ok := t.tracker.Get(id); ok && !isTerminalTaskState(d.State) {
			return false
		}
	}
	return true
}

// formatResult 汇总目标状态：完成的输出结果（截断 2KB）并 MarkInjected；
// 未完成的标注"仍在运行"。
func (t *TaskWaitTool) formatResult(ids []string, timedOut bool) string {
	var doneIDs []string
	var sb strings.Builder
	for _, id := range ids {
		d, ok := t.tracker.Get(id)
		if !ok {
			continue
		}
		if isTerminalTaskState(d.State) {
			doneIDs = append(doneIDs, id)
			// 终态按 State 三分渲染：Cancelled（主代理主动取消）必须与 Failed
			// （子代理出错）区分——两者的后续处置不同（重跑 vs 读原因排查）。
			var status string
			switch d.State {
			case TaskDone:
				status = "完成"
			case TaskFailed:
				status = "失败"
			default: // TaskCancelled
				status = "已取消"
			}
			fmt.Fprintf(&sb, "[%s %s %s]\n%s\n", d.AgentName, status, id, truncateRunes(d.FinalText, 2048))
		} else {
			fmt.Fprintf(&sb, "[%s 仍在运行 %s]（%s）\n", d.AgentName, id, d.State)
		}
	}
	t.tracker.MarkInjected(doneIDs...)
	if timedOut {
		sb.WriteString("等待超时，以上仍在运行的任务未完成。可再次 task_wait 继续等待，或先处理其他事项。")
	}
	return strings.TrimRight(sb.String(), "\n")
}
