// harness.go — 评估运行框架核心，提供 RunCase（单 Case）和 Suite.Run（批量）两个入口。
//
// 设计约束：
//   - eval 场景要求完全确定性，不绑定 Session 或 Compactor，避免压缩策略引入非确定性
//   - 工作目录使用 Case.WorkDir；若为空则自动创建临时目录并在 Case 结束后清理
//   - recordingHook 位于 HookRegistry 链的最前端，确保即使工具未注册也能记录调用名称
package evals

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/harness9/internal/engine"
	"github.com/harness9/internal/hooks"
	"github.com/harness9/internal/schema"
	"github.com/harness9/internal/tools"
)

// Suite 是多个 Case 的集合，提供批量运行能力。
type Suite struct {
	// Cases 是待运行的评估用例列表。按顺序执行，互相隔离（每个 Case 有独立 workDir）。
	Cases []*Case
}

// Run 按序运行所有 Case，返回结果列表（长度与 Cases 相同，顺序对应）。
// 每个 Case 使用独立的工作目录，失败不影响后续 Case 的运行。
func (s *Suite) Run(ctx context.Context) []Result {
	results := make([]Result, len(s.Cases))
	for i, c := range s.Cases {
		results[i] = RunCase(ctx, c)
	}
	return results
}

// RunCase 运行单个评估 Case，返回 Result。
//
// 内部流程：
//  1. 确定工作目录（Case.WorkDir 或自动创建临时目录）
//  2. 构造最小化 AgentEngine：ScriptedProvider + 四个基础工具 + recordingHook
//  3. 调用 eng.Run 执行 agent loop
//  4. 运行所有 Assertion，分类为硬失败（Failures）或软警告（Warnings）
//
// 不绑定 Session 或 Compactor，确保 eval 在任何环境下行为完全一致（hermetic）。
func RunCase(ctx context.Context, c *Case) Result {
	start := time.Now()

	workDir := c.WorkDir
	if workDir == "" {
		var err error
		workDir, err = os.MkdirTemp("", "harness9-eval-*")
		if err != nil {
			return Result{
				Case:     c,
				Passed:   false,
				RunError: fmt.Errorf("创建临时目录失败：%w", err),
			}
		}
		// 自动清理：eval 结束后删除临时工作目录，防止高频运行时磁盘泄漏。
		defer os.RemoveAll(workDir)
	}

	maxTurns := c.MaxTurns
	if maxTurns <= 0 {
		maxTurns = 50
	}

	// 记录被执行工具的名称
	var toolNames []string
	recorder := &recordingHook{names: &toolNames}

	// 注册基础工具（eval 场景固定注册这四个工具）。
	// Registry.Register 对同名工具返回 ErrAlreadyRegistered；此处四个工具名各不相同，
	// 不会触发该错误——若触发则说明框架内部逻辑有误，此时通过 RunError 明确上浮。
	registry := tools.NewRegistry()
	for _, t := range []tools.BaseTool{
		tools.NewReadFileTool(workDir),
		tools.NewWriteFileTool(workDir),
		tools.NewBashTool(workDir),
		tools.NewEditFileTool(workDir),
	} {
		if err := registry.Register(t); err != nil {
			return Result{
				Case:     c,
				Passed:   false,
				RunError: fmt.Errorf("注册工具 %s 失败: %w", t.Name(), err),
			}
		}
	}
	hookReg := hooks.NewHookRegistry(registry, recorder)

	// 基础选项 + Case 附加选项（如 WithStallNudge）。附加选项置于其后，可覆盖默认。
	engOpts := append([]engine.Option{engine.WithMaxTurns(maxTurns)}, c.EngineOptions...)
	eng := engine.NewAgentEngine(c.Provider, hookReg, workDir, engOpts...)

	runErr := eng.Run(ctx, c.Prompt)
	finalOutput := extractFinalOutput(c.Provider)

	result := Result{
		Case:              c,
		TurnCount:         c.Provider.TurnIndex(),
		ToolCallsExecuted: toolNames,
		FinalOutput:       finalOutput,
		RunError:          runErr,
		Duration:          time.Since(start),
	}

	// 运行所有断言
	passed := true
	for _, a := range c.Assertions {
		if f := a.Check(&result); f != nil {
			if f.IsSoft {
				result.Warnings = append(result.Warnings, f)
			} else {
				result.Failures = append(result.Failures, f)
				passed = false
			}
		}
	}
	result.Passed = passed
	return result
}

// recordingHook 实现 hooks.ToolHook，在 BeforeExecute 阶段记录所有被调用工具的名称。
// 放在 HookRegistry 链的最前端，确保即使工具未注册也能捕获 LLM 的调用意图，
// 用于 ToolCalledAssertion / ToolNotCalledAssertion 的验证。
type recordingHook struct {
	// names 是外部传入的切片指针，所有 BeforeExecute 调用均追加到此切片。
	// 使用指针而非直接持有切片，确保 goroutine 中的 append 反映到外部变量。
	names *[]string
	// mu 保护 names 切片：引擎在同一 Turn 内并发执行多个工具调用时，
	// 各工具 goroutine 会并发进入 BeforeExecute，若无锁保护则发生切片并发写竞争
	// （go test -race 可复现）。顺序无严格要求，此处仅保证写入原子性。
	mu sync.Mutex
}

// BeforeExecute 记录工具调用名称，始终返回 HookActionAllow（不拦截任何工具）。
func (h *recordingHook) BeforeExecute(ctx context.Context, tc schema.ToolCall) (context.Context, hooks.HookDecision, error) {
	h.mu.Lock()
	*h.names = append(*h.names, tc.Name)
	h.mu.Unlock()
	return ctx, hooks.Allow(), nil
}

// AfterExecute 透传工具执行结果，不做任何修改。
func (h *recordingHook) AfterExecute(_ context.Context, _ schema.ToolCall, result schema.ToolResult) schema.ToolResult {
	return result
}

// extractFinalOutput 从 ScriptedProvider 的脚本序列中提取最终文本输出。
// 从末尾向前遍历，返回第一个满足"无工具调用且有文本"的 Turn 的 Text。
// 若脚本序列全为工具调用（无纯文本 Turn），返回通用终止文案"任务完成。"。
//
// 这反映了 ReAct 循环的自然语义：最终的文本回复是 Agent 在停止调用工具后的总结陈述。
func extractFinalOutput(p *ScriptedProvider) string {
	for i := len(p.Turns) - 1; i >= 0; i-- {
		if len(p.Turns[i].ToolCalls) == 0 && p.Turns[i].Text != "" {
			return p.Turns[i].Text
		}
	}
	return "任务完成。"
}
