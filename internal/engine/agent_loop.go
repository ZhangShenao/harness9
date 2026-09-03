// Package engine 实现了 harness9 的核心 agent loop — 标准 ReAct 循环编排层。
//
// 每个 Turn：LLM 调用（携带完整工具列表）→ 工具执行（如有）→ Observation 注入 → 下一 Turn。
// 通过 emitter 抽象支持阻塞（Run）和流式（RunStream）两种输出模式，共享同一主循环内核。
//
// 文件布局（按职责拆分，本文件只保留引擎结构与循环编排器）：
//
//	options.go     — With* 配置选项与运行期 Set* 方法
//	retry.go       — LLM 生成调用的双档重试策略（默认预算 / 网络预算）
//	loop_phases.go — runLoop 的阶段化实现（初始化 / Turn 前置 / 预处理 / 生成 / 注入 / 收尾）
//	history.go     — 会话历史加载、持久化与压缩适配
//	planmode.go    — Plan Mode 工具白名单、prompt 前缀与 nudge 工具集判定
//	tools_exec.go  — 同 Turn 多工具的并发调度
//	stream.go      — 流式入口 RunStream 与事件类型定义
//	compact.go     — TUI /compact 触发的手动强制压缩
//	observer.go    — EngineObserver 可观测接入点
//	permission.go  — PermissionMode 全局权限策略
package engine

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/harness9/internal/hooks"
	"github.com/harness9/internal/logfmt"
	"github.com/harness9/internal/memory"
	"github.com/harness9/internal/planning"
	"github.com/harness9/internal/provider"
	"github.com/harness9/internal/schema"
	"github.com/harness9/internal/tools"
)

// AgentEngine 是 harness9 agent loop 的核心编排器，将 LLM Provider（"大脑"）
// 与 Tool Registry（"双手"）组合在一起，执行多轮 ReAct 循环直到任务完成。
type AgentEngine struct {
	provider           provider.LLMProvider
	registry           tools.Registry
	workDir            string
	maxTurns           int
	toolTimeout        time.Duration
	maxConcurrentTools int
	contextWindow      int // 模型 context window（tokens），用于 TUI 展示，0 表示未知
	promptBuilder      PromptBuilder
	mu                 sync.RWMutex        // protects session and compactor
	session            memory.Session      // 可选，nil 表示无持久化
	compactor          memory.Compactor    // 可选，nil 表示不压缩
	planMode           planning.PlanMode   // 当前执行模式，影响工具过滤
	todoStore          *planning.TodoStore // 可选，nil 表示无 planning
	permissionMode     PermissionMode      // 全局权限策略，影响审批行为
	nudgeInterval      int                 // >0 时每隔该轮数注入一次记忆 nudge
	nudgeText          string              // nudge 提示文本
	stallWindow        int                 // >0 时连续该轮数无进展工具调用则注入一次停滞 nudge
	stallText          string              // 停滞 nudge 提示文本
	observer           EngineObserver      // 可选，nil 时自动退化为 noopObserver
	generateRetries    int                 // LLM 生成调用最大尝试次数（默认 3）
	generateRetryBase  time.Duration       // 重试退避基准（默认 1s）
	networkRetries     int                 // 网络传输层错误的独立最大尝试次数（默认 6）
	networkRetryBase   time.Duration       // 网络传输层错误的重试退避基准（默认 5s）
}

// NewAgentEngine 创建新的 AgentEngine。默认值：maxTurns=500, toolTimeout=60s,
// generateRetries=3（base 1s）—— 对瞬时 LLM/流式错误具备基础韧性。
func NewAgentEngine(p provider.LLMProvider, r tools.Registry, workDir string, opts ...Option) *AgentEngine {
	e := &AgentEngine{
		provider:          p,
		registry:          r,
		workDir:           workDir,
		maxTurns:          500,
		toolTimeout:       60 * time.Second,
		generateRetries:   3,
		generateRetryBase: 1 * time.Second,
		networkRetries:    6,
		networkRetryBase:  5 * time.Second,
	}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// emitter 封装 Run 与 RunStream 在"输出侧"的差异：
//   - generate:     如何执行一次 LLM 调用并处理输出（阻塞打印 stdout vs 流式发事件）
//   - toolStart:    工具开始时的副作用（仅日志 vs 日志 + EventToolStart）
//   - toolDone:     工具完成时的副作用（仅日志 vs 日志 + EventToolResult）
//   - tokenUpdate:  报告 token 用量（仅日志 vs 日志 + EventTokenUpdate）
//   - compaction:   上下文压缩时报告压缩详情（仅日志 vs 日志 + EventCompaction）
//
// toolStart / toolDone 在 per-tool goroutine 中并发调用，实现方需自行保证安全。
type emitter struct {
	// generate 执行一次 LLM 调用，返回响应 Message 和实际 token 用量（可能为 nil）。
	generate  func(ctx context.Context, turn int, history []schema.Message, tools []schema.ToolDefinition) (*schema.Message, *schema.Usage, error)
	toolStart func(turn int, tc schema.ToolCall)
	toolDone  func(turn int, tc schema.ToolCall, result schema.ToolResult, d time.Duration)
	// tokenUpdate 报告当前 context 的 token 用量。
	// 在 LLM 调用前以估算值调用；调用后若有实际用量则以实际值再次调用。
	// tokens = token 数；window = 模型 context window（0 表示未知）。
	tokenUpdate func(tokens, window int)
	// compaction 在上下文发生有效压缩时调用。
	compaction func(record memory.CompactionRecord)
	// approval 是人类审批回调，注入到工具执行 context 中。
	// RunStream 模式下通过 EventApprovalRequired 事件驱动 TUI 审批对话框；
	// Run（阻塞）模式下留 nil，HookActionAsk 视为 Allow（向后兼容）。
	approval hooks.ApprovalFunc
}

// Run 执行单个用户 prompt 的阻塞式主循环，文本输出到 stdout。
func (e *AgentEngine) Run(ctx context.Context, userPrompt string) error {
	em := emitter{
		generate: func(ctx context.Context, _ int, history []schema.Message, tools []schema.ToolDefinition) (*schema.Message, *schema.Usage, error) {
			msg, usage, err := e.provider.Generate(ctx, history, tools)
			if err != nil {
				return nil, nil, err
			}
			// 防御：契约违规的空响应（msg 为 nil）不在此处解引用——
			// 原样上抛给 generateWithRetry 统一处理（转为可重试错误）。
			if msg != nil && msg.Content != "" {
				fmt.Printf("[assistant] %s\n", msg.Content)
			}
			return msg, usage, nil
		},
		toolStart: func(turn int, tc schema.ToolCall) {
			log.Print(logfmt.FormatToolStart("engine", turn, tc))
		},
		toolDone: func(turn int, tc schema.ToolCall, result schema.ToolResult, d time.Duration) {
			log.Print(logfmt.FormatToolDone("engine", turn, tc, result, d))
		},
		tokenUpdate: func(tokens, window int) {
			log.Print(logfmt.FormatMsg("engine", fmt.Sprintf("context tokens: ~%s", memory.FormatTokenCount(tokens))))
		},
		compaction: func(record memory.CompactionRecord) {
			log.Print(logfmt.FormatMsg("engine", fmt.Sprintf(
				"context compacted [tier %d]: %s → %s tokens (%d → %d msgs)",
				record.Tier,
				memory.FormatTokenCount(record.TokensBefore),
				memory.FormatTokenCount(record.TokensAfter),
				record.MsgsBefore, record.MsgsAfter,
			)))
		},
	}
	return e.runLoop(ctx, userPrompt, "engine", em)
}

// runLoop 是 Run 与 RunStream 共享的主循环内核，按阶段编排一次完整交互：
//
//	beginInteraction → 初始化（observer / 快照 / Todo 恢复 / Plan 前缀 / 历史加载）
//	for {
//	    beginTurn         → Turn 计数 + 双重终止判定（MaxTurns / ctx 取消）
//	    prepareTurnInput  → 工具过滤 + 压缩检查 + token 估算 + nudge 注入
//	    generateTurn      → 带重试 LLM 调用 + 实际用量上报
//	    （自然终止判定）    → 模型无工具调用则收敛退出
//	    executeTools      → 并发工具调度（tools_exec.go）
//	    injectObservations → Observation 注入
//	}
//	saveHistory      → 仅自然终止路径持久化新增消息
//
// 错误语义：非自然终止（MaxTurns / ctx 取消 / 生成失败）时返回错误且不持久化
// 本轮新增历史（丢弃整条轨迹，与既有行为一致）；TodoStore 无论何种退出路径均持久化。
func (e *AgentEngine) runLoop(ctx context.Context, userPrompt string, logPrefix string, em emitter) error {
	log.Print(logfmt.FormatLoopStart(logPrefix, e.workDir, e.maxTurns, e.toolTimeout, e.maxConcurrentTools))

	lc := e.beginInteraction(ctx, userPrompt, logPrefix, em)

	// defer 注册顺序（LIFO 决定执行顺序）：
	//   OnInteractionEnd 先注册 → 交互结束时最后执行，确保 span 覆盖 todos 持久化；
	//   saveTodos 后注册 → 所有退出路径最先执行，保证规划进度不因异常退出丢失。
	defer func() { lc.obs.OnInteractionEnd(lc.obsCtx, lc.turns, lc.interactionErr) }()
	defer lc.saveTodos(ctx)

	for {
		turnCtx, err := lc.beginTurn(ctx)
		if err != nil {
			return err
		}

		turnStart := time.Now()
		input, _ := lc.prepareTurnInput()
		log.Print(logfmt.FormatTurnStart(logPrefix, lc.turns, len(input.history), len(input.toolDefs)))

		responseMsg, llmDuration, err := lc.generateTurn(turnCtx, input)
		if err != nil {
			return err
		}

		// 自然终止判定：模型不再发起工具调用，ReAct 循环收敛。
		if len(responseMsg.ToolCalls) == 0 {
			log.Print(logfmt.FormatTurnDone(logPrefix, lc.turns, llmDuration, time.Since(lc.overallStart)))
			lc.obs.OnTurnEnd(turnCtx, lc.turns, false)
			break
		}

		lc.trackStall(responseMsg.ToolCalls)

		toolStart := time.Now()
		results := e.executeTools(turnCtx, lc.turns, responseMsg.ToolCalls, logPrefix, em)
		toolDuration := time.Since(toolStart)

		lc.history = injectObservations(lc.history, responseMsg.ToolCalls, results)

		log.Print(logfmt.FormatObservation(logPrefix, lc.turns, len(lc.history), llmDuration, toolDuration, time.Since(turnStart)))
		lc.obs.OnTurnEnd(turnCtx, lc.turns, true)
	}

	lc.saveHistory(ctx)
	log.Print(logfmt.FormatLoopEnd(logPrefix, lc.turns, time.Since(lc.overallStart)))
	return nil
}
