// Package subagent — Runner：构建并运行隔离子代理引擎。
// 本文件实现 Runner，负责为每次子代理调用构建独立的工具注册表和引擎实例，
// 消费子引擎事件流并将进度透传给父 TUI，同时桥接审批回调。
// 关键设计：子代理从会话级 baseCtx 派生 execCtx，绕过父工具的 60s 超时限制，
// 避免多轮子代理在单次工具调用超时前被强制终止。
package subagent

import (
	"context"
	"fmt"
	"time"

	"github.com/harness9/internal/engine"
	"github.com/harness9/internal/hooks"
	"github.com/harness9/internal/memory"
	"github.com/harness9/internal/permission"
	"github.com/harness9/internal/provider"
	"github.com/harness9/internal/sandbox"
	"github.com/harness9/internal/schema"
	"github.com/harness9/internal/skills"
	"github.com/harness9/internal/tools"
)

// Runner 构建并运行子代理引擎。从 main.go 注入一次，运行期只读。
type Runner struct {
	baseTools          []tools.BaseTool // 全部基础工具实例（可安全跨引擎共享）
	sharedHooks        []hooks.ToolHook // danger + offload（permission 单独派生）
	settingsPath       string           // .harness9/settings.json（权限继承源）
	skillsIndex        *skills.Index    // 预加载 skill 正文（可为 nil）
	workDir            string
	defaultMaxTurns    int
	toolTimeout        time.Duration
	maxConcurrentTools int
	providerFor        func(model string) (provider.LLMProvider, int, error)
	compactorFor       func(p provider.LLMProvider, ctxWin int) memory.Compactor
	baseCtx            context.Context  // 会话级 ctx，后台任务从此派生
	sandboxMgr         *sandbox.Manager // optional；nil = 子代理不使用 Sandbox
}

// SubAgentResult 是子代理一次执行的结果。
type SubAgentResult struct {
	AgentID   string
	FinalText string
}

// denyTaskHook 是纵深防御 hook：始终拒绝子代理调用 task 工具（防递归）。
type denyTaskHook struct{}

func (denyTaskHook) BeforeExecute(ctx context.Context, tc schema.ToolCall) (context.Context, hooks.HookDecision, error) {
	if tc.Name == "task" {
		return ctx, hooks.Deny("子代理不允许再派生子代理"), nil
	}
	return ctx, hooks.Allow(), nil
}

func (denyTaskHook) AfterExecute(_ context.Context, _ schema.ToolCall, r schema.ToolResult) schema.ToolResult {
	return r
}

// buildChildRegistry 构造子代理的隔离工具注册表：仅注册定义允许的基础工具
// （永不含 task），再包权限派生 hook + denyTaskHook + sharedHooks（danger/offload）。
// baseTools 参数允许调用方传入经 sandbox 包装后的工具集（Sandbox 模式），
// 或直接传入 r.baseTools（无 Sandbox 模式），实现解耦。
func (r *Runner) buildChildRegistry(def SubAgentDefinition, baseTools []tools.BaseTool) (tools.Registry, error) {
	allNames := make([]string, 0, len(baseTools))
	byName := make(map[string]tools.BaseTool, len(baseTools))
	for _, t := range baseTools {
		allNames = append(allNames, t.Name())
		byName[t.Name()] = t
	}
	resolved := def.ResolveTools(allNames)

	base := tools.NewRegistry()
	for _, name := range resolved {
		if t, ok := byName[name]; ok {
			if err := base.Register(t); err != nil {
				return nil, fmt.Errorf("注册子代理工具 %q 失败: %w", name, err)
			}
		}
	}

	hookChain := []hooks.ToolHook{permission.NewFileHook(r.settingsPath), denyTaskHook{}}
	hookChain = append(hookChain, r.sharedHooks...)
	return hooks.NewHookRegistry(base, hookChain...), nil
}

// Run 同步执行一个子代理：构建隔离子引擎，调用 RunStream，消费事件流，
// 转发进度、桥接审批、累积最终文本。background 控制审批策略与执行 context。
func (r *Runner) Run(ctx context.Context, def SubAgentDefinition, prompt string, background bool) (SubAgentResult, error) {
	// 为子代理创建独立 Sandbox（若 Manager 已配置）
	effectiveBaseTools := r.baseTools
	if r.sandboxMgr != nil {
		sandboxEnv, err := r.sandboxMgr.Create(ctx, r.workDir)
		if err != nil {
			return SubAgentResult{}, fmt.Errorf("sandbox: 为子代理创建环境失败: %w", err)
		}
		defer r.sandboxMgr.Destroy(r.baseCtx, sandboxEnv.ID())
		effectiveBaseTools = wrapToolsWithSandbox(r.baseTools, sandboxEnv, r.workDir)
	}

	childReg, err := r.buildChildRegistry(def, effectiveBaseTools)
	if err != nil {
		return SubAgentResult{}, fmt.Errorf("构建子代理工具注册表失败: %w", err)
	}

	p, ctxWin, err := r.providerFor(def.Model)
	if err != nil {
		return SubAgentResult{}, fmt.Errorf("解析子代理模型失败: %w", err)
	}

	var loader skillLoader
	if r.skillsIndex != nil {
		loader = r.skillsIndex.GetFullContent
	}
	spb := newPromptBuilder(def.SystemPrompt, r.workDir, def.Skills, loader).
		WithSandboxContext(r.sandboxMgr != nil)

	maxTurns := r.defaultMaxTurns
	if def.MaxTurns > 0 {
		maxTurns = def.MaxTurns
	}

	childID := fmt.Sprintf("subagent-%s", def.Name)
	childSession := memory.NewMemorySession(childID)

	opts := []engine.Option{
		engine.WithPromptBuilder(spb),
		engine.WithSession(childSession),
		engine.WithMaxTurns(maxTurns),
		engine.WithContextWindow(ctxWin),
		engine.WithToolTimeout(r.toolTimeout),
		engine.WithMaxConcurrentTools(r.maxConcurrentTools),
	}
	if comp := r.compactorFor(p, ctxWin); comp != nil {
		opts = append(opts, engine.WithCompactor(comp))
	}
	sub := engine.NewAgentEngine(p, childReg, r.workDir, opts...)

	// 执行 context 的关键设计：
	// 调用方 ctx 是父引擎的"工具执行 ctx"，带有 toolTimeout（默认 60s）——这是为单个普通工具
	// （bash/read_file）设计的时限，但子代理本身是一个多轮 agent，整轮运行可能远超 60s。
	// 若直接复用 ctx，子代理会在 60s 处被父工具超时杀死（表现为 LLM 调用 context deadline exceeded）。
	// 因此前台与后台都从会话级 baseCtx 派生 execCtx（无 60s 工具时限）：
	//   - 后台：完全脱离父 turn，仅受会话级取消约束。
	//   - 前台：额外监听调用方 ctx 的"真正取消"（用户 Ctrl+C，Cause != DeadlineExceeded）并传播，
	//           但忽略 60s 工具超时这一 Cause，使子代理得以跑完多轮。
	execCtx, cancel := context.WithCancel(r.baseCtx)
	defer cancel()
	if !background {
		go func() {
			select {
			case <-ctx.Done():
				if context.Cause(ctx) != context.DeadlineExceeded {
					cancel() // 真正的取消（如 Ctrl+C）→ 停止子代理；60s 工具超时则忽略
				}
			case <-execCtx.Done():
				// 子代理已结束，停止监听，避免 goroutine 泄漏。
			}
		}()
	}

	// 进度去向由调用方 ctx 决定：前台为父 TUI 的 RunStream sink；后台由 TaskTool 注入"写 TaskTracker"的 sink
	// （后台 sink 写内存缓冲、不碰父 channel，故无 send-on-closed-channel 风险）。
	progress := hooks.SubAgentProgressFromContext(ctx)
	emit := func(u schema.SubAgentUpdate) {
		u.AgentName = def.Name
		if progress != nil {
			progress(u)
		}
	}
	parentApproval := hooks.ApprovalFnFromContext(ctx)

	emit(schema.SubAgentUpdate{Kind: schema.SubAgentStart})

	stream, err := sub.RunStream(execCtx, prompt)
	if err != nil {
		return SubAgentResult{}, err
	}

	var currentTurnText string
	curTurn := -1
	for evt := range stream {
		switch evt.Type {
		case engine.EventActionDelta:
			if evt.Turn != curTurn {
				curTurn = evt.Turn
				currentTurnText = ""
			}
			s, _ := evt.Data.(string)
			currentTurnText += s
			emit(schema.SubAgentUpdate{Kind: schema.SubAgentDelta, Text: s})
		case engine.EventThinkingDelta:
			s, _ := evt.Data.(string)
			emit(schema.SubAgentUpdate{Kind: schema.SubAgentThinking, Text: s})
		case engine.EventToolStart:
			currentTurnText = "" // 工具前文本是中间产物，丢弃
			if tc, ok := evt.Data.(schema.ToolCall); ok {
				// Text 携带工具调用参数（紧凑 JSON），供 TUI 展示 `工具名(参数)`。
				emit(schema.SubAgentUpdate{Kind: schema.SubAgentToolStart, ToolName: tc.Name, Text: string(tc.Arguments)})
			}
		case engine.EventToolResult:
			if d, ok := evt.Data.(engine.ToolResultData); ok {
				emit(schema.SubAgentUpdate{Kind: schema.SubAgentToolResult, IsError: d.Result.IsError})
			}
		case engine.EventApprovalRequired:
			req, ok := evt.Data.(engine.ApprovalRequest)
			if !ok {
				continue
			}
			if background || parentApproval == nil {
				req.ResponseCh <- hooks.ApprovalResponse{Approved: false,
					Feedback: "子代理无可用审批通道，已自动拒绝"}
			} else {
				req.ResponseCh <- parentApproval(execCtx, req.ToolCall, req.Reason, req.RiskLevel)
			}
		case engine.EventError:
			msg, _ := evt.Data.(string)
			emit(schema.SubAgentUpdate{Kind: schema.SubAgentError, Text: msg})
			return SubAgentResult{}, fmt.Errorf("子代理执行失败: %s", msg)
		case engine.EventTerminated:
			// 受控熔断（max_turns/预算耗尽/死循环拦截）对子代理而言同样意味着
			// 任务未完成：作为错误上抛给主代理，附带原因码供其决策。
			data, _ := evt.Data.(engine.TerminationData)
			emit(schema.SubAgentUpdate{Kind: schema.SubAgentError, Text: data.Message})
			return SubAgentResult{}, fmt.Errorf("子代理受控终止 [%s]: %s", data.Reason, data.Message)
		case engine.EventDone:
			// 循环将随 channel 关闭自然结束。
		}
	}

	emit(schema.SubAgentUpdate{Kind: schema.SubAgentDone, Text: currentTurnText})
	return SubAgentResult{AgentID: childID, FinalText: currentTurnText}, nil
}

// RunnerConfig 是 NewRunner 的配置。
type RunnerConfig struct {
	BaseTools          []tools.BaseTool
	SharedHooks        []hooks.ToolHook
	SettingsPath       string
	SkillsIndex        *skills.Index
	WorkDir            string
	DefaultMaxTurns    int
	ToolTimeout        time.Duration
	MaxConcurrentTools int
	ProviderFor        func(model string) (provider.LLMProvider, int, error)
	CompactorFor       func(p provider.LLMProvider, ctxWin int) memory.Compactor
	BaseCtx            context.Context
	SandboxMgr         *sandbox.Manager // optional；nil = 子代理不使用 Sandbox
}

// NewRunner 从配置构造 Runner。
func NewRunner(cfg RunnerConfig) *Runner {
	return &Runner{
		baseTools:          cfg.BaseTools,
		sharedHooks:        cfg.SharedHooks,
		settingsPath:       cfg.SettingsPath,
		skillsIndex:        cfg.SkillsIndex,
		workDir:            cfg.WorkDir,
		defaultMaxTurns:    cfg.DefaultMaxTurns,
		toolTimeout:        cfg.ToolTimeout,
		maxConcurrentTools: cfg.MaxConcurrentTools,
		providerFor:        cfg.ProviderFor,
		compactorFor:       cfg.CompactorFor,
		baseCtx:            cfg.BaseCtx,
		sandboxMgr:         cfg.SandboxMgr,
	}
}

// wrapToolsWithSandbox 为标准工具注入 sandbox.Environment，非标准工具原样返回。
// 工具名字符串（"bash"/"read_file" 等）与各 Tool.Name() 方法返回值耦合；
// 若工具名变更，此处需同步维护。
func wrapToolsWithSandbox(ts []tools.BaseTool, env sandbox.Environment, workDir string) []tools.BaseTool {
	result := make([]tools.BaseTool, len(ts))
	for i, t := range ts {
		switch t.Name() {
		case "bash":
			result[i] = tools.NewBashTool(workDir, tools.WithEnvironment(env))
		case "read_file":
			result[i] = tools.NewReadFileTool(workDir, tools.ReadFileWithEnvironment(env))
		case "write_file":
			result[i] = tools.NewWriteFileTool(workDir, tools.WriteFileWithEnvironment(env))
		case "edit_file":
			result[i] = tools.NewEditFileTool(workDir, tools.EditFileWithEnvironment(env))
		default:
			result[i] = t
		}
	}
	return result
}
