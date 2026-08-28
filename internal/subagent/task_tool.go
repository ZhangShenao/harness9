// Package subagent — TaskTool：主代理委派子任务的 task 工具。
// 本文件实现 TaskTool（工具名 "task"），允许主代理通过 LLM ToolCall 将边界清晰的子任务
// 委派给已注册的子代理执行。支持前台阻塞（background=false）和后台异步（background=true）两种模式：
//   - 前台：阻塞当前 Turn，消费子代理事件流，最终回传完整结论文本
//   - 后台：立即返回 task id，子代理在后台 goroutine 中运行，结果经 TaskTracker 注入后续 Turn
//
// 安全保障：denyTaskHook 在子代理工具注册表层面强制禁止递归委派（task 工具永不出现在子代理工具集中）。
package subagent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/harness9/internal/hooks"
	"github.com/harness9/internal/schema"
)

// TaskTool 实现 tools.BaseTool（结构类型隐式满足，无需 import tools）。
type TaskTool struct {
	reg     *Registry
	runner  *Runner
	tracker *TaskTracker
}

// NewTaskTool 创建 task 工具。
func NewTaskTool(reg *Registry, runner *Runner, tracker *TaskTracker) *TaskTool {
	return &TaskTool{reg: reg, runner: runner, tracker: tracker}
}

// Name 返回工具标识符 "task"。
func (t *TaskTool) Name() string { return "task" }

// Definition 动态生成工具定义：subagent_type 枚举所有已注册子代理，description 拼接各用途。
func (t *TaskTool) Definition() schema.ToolDefinition {
	defs := t.reg.List()
	names := make([]string, 0, len(defs))
	var sb strings.Builder
	sb.WriteString("把一个边界清晰的任务委派给专门的子代理执行。子代理拥有独立上下文与受限工具集。\n可用子代理：\n")
	for _, d := range defs {
		names = append(names, d.Name)
		fmt.Fprintf(&sb, "- %s: %s\n", d.Name, d.Description)
	}
	return schema.ToolDefinition{
		Name:        "task",
		Description: sb.String(),
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"subagent_type": map[string]any{
					"type": "string", "enum": names,
					"description": "要调用的子代理类型名称",
				},
				"description": map[string]any{
					"type": "string", "description": "任务的简短标题（3-5 词，用于 UI 展示）",
				},
				"prompt": map[string]any{
					"type":        "string",
					"description": "传给子代理的完整任务描述。子代理看不到主对话历史，所有必要信息（文件路径、背景、要求）都要写在这里。",
				},
				"background": map[string]any{
					"type":        "boolean",
					"description": "是否后台异步运行。true 时立即返回，结果稍后注入；false（默认）阻塞直到完成。",
				},
			},
			"required": []string{"subagent_type", "prompt"},
		},
	}
}

type taskArgs struct {
	SubAgentType string `json:"subagent_type"`
	Description  string `json:"description"`
	Prompt       string `json:"prompt"`
	Background   bool   `json:"background"`
}

// Execute 解析参数、查找子代理定义并执行（前台阻塞 / 后台异步）。
func (t *TaskTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a taskArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("参数解析失败：%w", err)
	}
	if a.SubAgentType == "" {
		return "", fmt.Errorf("subagent_type 不能为空")
	}
	if a.Prompt == "" {
		return "", fmt.Errorf("prompt 不能为空")
	}
	def, ok := t.reg.Get(a.SubAgentType)
	if !ok {
		return "", fmt.Errorf("未知子代理类型 %q，可用: %s", a.SubAgentType, t.agentNames())
	}

	if a.Background {
		taskID := t.tracker.Start(def.Name, a.Prompt)
		go func() {
			defer func() {
				if rec := recover(); rec != nil {
					t.tracker.Finish(taskID, fmt.Sprintf("子代理后台执行 panic: %v", rec), true)
				}
			}()
			// 关键安全点 1：从 context.Background() 构造 bgCtx，而非复用 t.runner.baseCtx。
			// 背景：后台子代理的生命周期可能超过整个会话（用户退出 TUI 后仍在运行）。
			// 若用 baseCtx，用户 Ctrl+C 会取消 baseCtx 进而强制中止后台子代理；
			// context.Background() 使后台任务完全独立，持久到自然结束。
			//
			// 关键安全点 2：只注入"写 tracker"的 sink，绝不复用父 ctx 的 progress sink
			// （其 channel 会在父 turn 结束后关闭，写入即 panic / goroutine 阻塞）。
			sink := func(u schema.SubAgentUpdate) { t.tracker.AppendLog(taskID, u) }
			bgCtx := hooks.WithSubAgentProgress(context.Background(), sink)
			res, err := t.runner.Run(bgCtx, def, a.Prompt, true)
			if err != nil {
				t.tracker.Finish(taskID, err.Error(), true)
			} else {
				t.tracker.Finish(taskID, res.FinalText, false)
			}
		}()
		return fmt.Sprintf(`<task id=%q state="running"/>`, taskID), nil
	}

	res, err := t.runner.Run(ctx, def, a.Prompt, false)
	if err != nil {
		return fmt.Sprintf(`<task state="error">%s</task>`, err.Error()), nil
	}
	return fmt.Sprintf(`<task state="completed"><task_result>%s</task_result></task>`, res.FinalText), nil
}

func (t *TaskTool) agentNames() string {
	defs := t.reg.List()
	names := make([]string, len(defs))
	for i, d := range defs {
		names[i] = d.Name
	}
	return strings.Join(names, ", ")
}
