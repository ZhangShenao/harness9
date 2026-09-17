// Package subagent — TaskStatusTool：主代理查询后台子代理任务状态的工具。
//
// 观察（spec §5.4）：全部列表或单任务查询；已完成任务的结果随查询返回并
// MarkInjected（与 DrainCompleted/task_wait 共用标志，结果恰好注入一次）。
package subagent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/harness9/internal/schema"
)

// TaskStatusTool 实现 tools.BaseTool（结构类型隐式满足）。
type TaskStatusTool struct {
	tracker *TaskTracker
}

// NewTaskStatusTool 创建 task_status 工具。
func NewTaskStatusTool(tracker *TaskTracker) *TaskStatusTool {
	return &TaskStatusTool{tracker: tracker}
}

// Name 返回工具名 "task_status"。
func (t *TaskStatusTool) Name() string { return "task_status" }

// Definition 返回工具定义。
func (t *TaskStatusTool) Definition() schema.ToolDefinition {
	return schema.ToolDefinition{
		Name:        "task_status",
		Description: "查询后台子代理任务的状态：运行中/已暂停/已完成/已失败/已取消，含耗时与最近活动。已完成任务的结果会随查询直接返回（此后不再重复注入）。省略 task_id 返回全部任务。",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"task_id": map[string]any{"type": "string", "description": "要查询的任务 id；省略则返回全部"},
			},
		},
	}
}

type taskStatusArgs struct {
	TaskID string `json:"task_id"`
}

// Execute 查询并格式化任务状态。
func (t *TaskStatusTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a taskStatusArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("参数解析失败：%w", err)
	}
	if a.TaskID != "" {
		d, ok := t.tracker.Get(a.TaskID)
		if !ok {
			return "", fmt.Errorf("任务 %q 不存在", a.TaskID)
		}
		t.tracker.MarkInjected(a.TaskID)
		return formatTaskDetailStatus(d), nil
	}
	snaps := t.tracker.List()
	if len(snaps) == 0 {
		return "当前没有后台子代理任务。", nil
	}
	var doneIDs []string
	var sb strings.Builder
	for _, s := range snaps {
		sb.WriteString(formatTaskSnapshotStatus(s))
		sb.WriteString("\n")
		if isTerminalTaskState(s.State) {
			doneIDs = append(doneIDs, s.ID)
			if d, ok := t.tracker.Get(s.ID); ok {
				sb.WriteString(truncateRunes(d.FinalText, 2048))
				sb.WriteString("\n")
			}
		}
	}
	t.tracker.MarkInjected(doneIDs...)
	return strings.TrimRight(sb.String(), "\n"), nil
}

// formatTaskSnapshotStatus 生成单行状态摘要。
func formatTaskSnapshotStatus(s TaskSnapshot) string {
	desc := s.Description
	if desc == "" {
		desc = truncateRunes(s.Prompt, 24)
	}
	line := fmt.Sprintf("%s [%s] %s %q %s", s.ID, s.State, s.AgentName, desc, formatDuration(s.Elapsed))
	if s.LastActivity != "" {
		line += "；最近：" + s.LastActivity
	}
	return line
}

// formatTaskDetailStatus 生成单任务详情（状态行 + 结果文本）。
func formatTaskDetailStatus(d TaskDetail) string {
	line := formatTaskSnapshotStatus(TaskSnapshot{
		ID: d.ID, AgentName: d.AgentName, Description: d.Description,
		State: d.State, Elapsed: d.Elapsed, LastActivity: "详见任务面板",
	})
	if isTerminalTaskState(d.State) && d.FinalText != "" {
		return line + "\n" + truncateRunes(d.FinalText, 2048)
	}
	return line
}

// formatDuration 生成人类可读时长。
func formatDuration(d time.Duration) string {
	switch {
	case d >= time.Minute:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
}
