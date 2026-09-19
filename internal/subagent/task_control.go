// Package subagent — TaskControlTool：主代理控制后台子代理任务的工具。
//
// 唯一入口路由到 tracker.Control（spec §5.4）。状态机错误（对终态任务操作等）
// 以正常文本结果返回而非 Go error——LLM 能读到原因并自行调整（spec §7.4）。
package subagent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/harness9/internal/schema"
)

// TaskControlTool 实现 tools.BaseTool。
type TaskControlTool struct {
	tracker *TaskTracker
}

// NewTaskControlTool 创建 task_control 工具。
func NewTaskControlTool(tracker *TaskTracker) *TaskControlTool {
	return &TaskControlTool{tracker: tracker}
}

// Name 返回工具名 "task_control"。
func (t *TaskControlTool) Name() string { return "task_control" }

// Definition 返回工具定义。
func (t *TaskControlTool) Definition() schema.ToolDefinition {
	return schema.ToolDefinition{
		Name:        "task_control",
		Description: "控制后台子代理任务：pause 暂停（当前轮完成后停住）、resume 恢复、cancel 取消（message 为原因，立即中断）、steer 注入转向指令（message 必填，子代理下一轮生效，适合中途调整方向而非推倒重来）。",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"task_id": map[string]any{"type": "string", "description": "目标任务 id"},
				"action":  map[string]any{"type": "string", "enum": []string{"pause", "resume", "cancel", "steer"}, "description": "控制动作"},
				"message": map[string]any{"type": "string", "description": "steer 的转向指令内容 / cancel 的原因"},
			},
			"required": []string{"task_id", "action"},
		},
	}
}

type taskControlArgs struct {
	TaskID  string `json:"task_id"`
	Action  string `json:"action"`
	Message string `json:"message"`
}

// Execute 校验参数并路由到 tracker.Control。
func (t *TaskControlTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a taskControlArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("参数解析失败：%w", err)
	}
	if a.TaskID == "" {
		return "", fmt.Errorf("task_id 不能为空")
	}
	switch a.Action {
	case "pause", "resume", "cancel", "steer":
	default:
		return "", fmt.Errorf("未知 action %q（可用: pause/resume/cancel/steer）", a.Action)
	}

	if err := t.tracker.Control(a.TaskID, a.Action, a.Message); err != nil {
		// 状态机错误（终态操作、steer 缺 message 等）以文本返回，LLM 可读原因。
		return fmt.Sprintf("操作失败：%s", err), nil
	}
	switch a.Action {
	case "pause":
		return fmt.Sprintf("%s 已暂停（当前轮完成后停住，resume 恢复）", a.TaskID), nil
	case "resume":
		return fmt.Sprintf("%s 已恢复运行", a.TaskID), nil
	case "cancel":
		return fmt.Sprintf("%s 已请求取消（进行中的调用将被中断）", a.TaskID), nil
	default: // steer
		return fmt.Sprintf("%s 已注入转向指令，子代理下一轮开始时生效", a.TaskID), nil
	}
}
