// Package engine 实现了 harness9 的核心 agent loop — 标准 ReAct 循环编排层。
//
// 每个 Turn：LLM 调用（携带完整工具列表）→ 工具执行（如有）→ Observation 注入 → 下一 Turn。
// 通过 emitter 抽象支持阻塞（Run）和流式（RunStream）两种输出模式，共享同一主循环内核。
package engine

import (
	"context"
	"fmt"
	"log"
	"strings"
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

// Option 是 AgentEngine 的函数选项。
type Option func(*AgentEngine)

// WithMaxTurns 设置单次 Run 允许的最大 Turn 数，0 或负数表示不限制。
func WithMaxTurns(n int) Option {
	return func(e *AgentEngine) { e.maxTurns = n }
}

// WithToolTimeout 设置单个工具执行的超时时间，0 表示使用 context 原始截止时间。
func WithToolTimeout(d time.Duration) Option {
	return func(e *AgentEngine) { e.toolTimeout = d }
}

// WithMaxConcurrentTools 设置同一 Turn 内最大并发工具数，0 或负数表示不限制。
func WithMaxConcurrentTools(n int) Option {
	return func(e *AgentEngine) { e.maxConcurrentTools = n }
}

// WithContextWindow 设置模型的最大 context window（tokens），用于 TUI token 使用率展示。
// 通常通过 provider.GetModelLimits(modelName).ContextTokens 获取。
func WithContextWindow(tokens int) Option {
	return func(e *AgentEngine) { e.contextWindow = tokens }
}

// WithGenerateRetry 配置 LLM 生成调用的应用层重试：最多尝试 attempts 次，
// 首次重试退避 baseDelay，其后指数增长（上限 maxGenerateRetryDelay）。
//
// 这是对 SDK 内置重试的补充：SDK 的重试只覆盖"首字节到达前"的连接错误，
// 而流式响应中途断连（mid-stream EOF / 代理超时 / 5xx after headers）会以
// StreamChunkError 形式逃逸到引擎层，旧实现直接终止整个 agent loop（丢掉整条轨迹）。
// 此重试在保持 context 取消语义的前提下，把"一次瞬时抖动杀实例"变为可恢复事件。
// attempts<=1 时关闭重试（仅尝试一次）。
func WithGenerateRetry(attempts int, baseDelay time.Duration) Option {
	return func(e *AgentEngine) {
		e.generateRetries = attempts
		e.generateRetryBase = baseDelay
	}
}

// WithNetworkRetry 配置网络传输层错误（TLS/DNS/连接建立，见 isTransientNetworkError）
// 的独立重试预算：最多尝试 attempts 次，首次重试退避 baseDelay，其后指数增长
// （上限 maxNetworkRetryDelay）。这类错误比业务层错误（4xx/5xx）间歇性更强，
// 默认的 WithGenerateRetry 预算（默认 3 次、总计约 3s）对它们太窄——Terminal-Bench
// pilot 里 3 个任务在 Turn 1 就命中同一条 x509 证书错误，退避耗尽后直接放弃整个 turn
// （见 docs/技术调研/terminal-bench-轨迹分析-v1.md §2 R2）。attempts<=1 时该扩展形同
// 关闭——网络错误仅尝试 1 次，不再享有比默认预算更宽松的窗口（不会退化为借用
// WithGenerateRetry 的预算，两者预算互相独立）。
func WithNetworkRetry(attempts int, baseDelay time.Duration) Option {
	return func(e *AgentEngine) {
		e.networkRetries = attempts
		e.networkRetryBase = baseDelay
	}
}

// ---- 以下为护栏体系新 Options。----

// WithRunTimeout 设置整次 Run 的墙钟超时上限，仅在 Turn 边界裁决。0 表示不限。
func WithRunTimeout(d time.Duration) Option {
	return func(e *AgentEngine) { e.runTimeout = d }
}

// WithTokenBudget 设置单次 Run 的累计 input token 预算上限。
// 按 API 实际返回的 usage.InputTokens 累计，缺失时以上下文估算兜底。0 表示不限。
func WithTokenBudget(n int) Option {
	return func(e *AgentEngine) { e.tokenBudget = n }
}

// WithRepetitionReminder 启用重复调用死循环检测：最近 window 个 turn 内同一
// 签名（工具名+canonical 参数哈希）出现 threshold 次时注入定向提醒；
// 提醒无效则升级为硬终止（ReasonRepetitionLoop）。任一参数 <=0 时关闭。
func WithRepetitionReminder(window, threshold int) Option {
	return func(e *AgentEngine) { e.repWindow, e.repThreshold = window, threshold }
}

// WithStallReminder 配置停滞提醒：连续 window 个 turn 未调用进展工具
// （edit_file/write_file）时向当轮历史副本注入一次 text，然后重置计数。
// 仅注入副本，绝不持久化。window<=0 时关闭。
func WithStallReminder(window int, text string) Option {
	return func(e *AgentEngine) { e.stallReminderWindow, e.stallReminderText = window, text }
}

// WithMemoryReminder 配置记忆提醒：每隔 interval 个 turn 注入一次 text。
// 仅注入副本，绝不持久化。interval<=0 时关闭。
func WithMemoryReminder(interval int, text string) Option {
	return func(e *AgentEngine) { e.memoryReminderInterval, e.memoryReminderText = interval, text }
}

// PromptBuilder 构造 Agent 的 system prompt。
// 接口定义在 engine 包（使用者侧），由 internal/context 包实现。
// 引擎通过此接口与 Context Engineering 模块解耦。
type PromptBuilder interface {
	Build() string
}

// WithEngineObserver 注册引擎生命周期观察者，供可观测层（OpenTelemetry 等）无侵入接入。
func WithEngineObserver(o EngineObserver) Option {
	return func(e *AgentEngine) { e.observer = o }
}

// WithPromptBuilder 设置自定义 PromptBuilder。未设置时使用内置默认文案。
func WithPromptBuilder(pb PromptBuilder) Option {
	return func(e *AgentEngine) { e.promptBuilder = pb }
}

// WithSession 绑定 Session，使 runLoop 在启动时加载历史、结束时保存新消息。
func WithSession(s memory.Session) Option {
	return func(e *AgentEngine) { e.session = s }
}

// WithCompactor 绑定上下文压缩策略，在每次 LLM 调用前裁剪历史消息。
func WithCompactor(c memory.Compactor) Option {
	return func(e *AgentEngine) { e.compactor = c }
}

// WithPlanMode 设置 Agent 的初始执行模式。
// runLoop 在启动时会快照此值，循环内不会受后续 SetPlanMode 调用影响。
func WithPlanMode(mode planning.PlanMode) Option {
	return func(e *AgentEngine) { e.planMode = mode }
}

// WithTodoStore 绑定 TodoStore，使引擎在 runLoop 生命周期中自动执行以下操作：
//   - 启动时：从 Session 恢复 TodoStore 状态（跨会话续接未完成任务）
//   - 结束时：通过 defer 将 TodoStore 保存到 Session（所有路径均执行）
func WithTodoStore(s *planning.TodoStore) Option {
	return func(e *AgentEngine) { e.todoStore = s }
}

// SetSession 替换当前绑定的 Session，供 TUI /new、/resume 命令切换会话时调用。
// 线程安全：可从任意 goroutine 调用（如 TUI goroutine），内部以写锁保护。
// 注意：修改对当前正在运行的 runLoop 无影响（runLoop 在入口快照 session 值）。
func (e *AgentEngine) SetSession(s memory.Session) {
	e.mu.Lock()
	e.session = s
	e.mu.Unlock()
}

// SetPlanMode 线程安全地更新当前执行模式。TUI Shift+Tab 键调用此方法。
// 注意：修改对当前正在运行的 runLoop 无影响（runLoop 在入口快照 planMode 值），
// 仅在下一次 Run/RunStream 调用时生效。
func (e *AgentEngine) SetPlanMode(mode planning.PlanMode) {
	e.mu.Lock()
	e.planMode = mode
	e.mu.Unlock()
}

// AgentEngine 是 harness9 agent loop 的核心编排器，将 LLM Provider（"大脑"）
// 与 Tool Registry（"双手"）组合在一起，执行多轮 ReAct 循环直到任务完成。
type AgentEngine struct {
	provider               provider.LLMProvider
	registry               tools.Registry
	workDir                string
	maxTurns               int
	toolTimeout            time.Duration
	maxConcurrentTools     int
	contextWindow          int // 模型 context window（tokens），用于 TUI 展示，0 表示未知
	promptBuilder          PromptBuilder
	mu                     sync.RWMutex        // protects session and compactor
	session                memory.Session      // 可选，nil 表示无持久化
	compactor              memory.Compactor    // 可选，nil 表示不压缩
	planMode               planning.PlanMode   // 当前执行模式，影响工具过滤
	todoStore              *planning.TodoStore // 可选，nil 表示无 planning
	permissionMode         PermissionMode      // 全局权限策略，影响审批行为
	memoryReminderInterval int                 // >0 时每隔该轮数注入一次记忆提醒
	memoryReminderText     string              // 记忆提醒文本
	stallReminderWindow    int                 // >0 时连续该轮数无进展工具调用则注入一次停滞提醒
	stallReminderText      string              // 停滞提醒文本
	observer               EngineObserver      // 可选，nil 时自动退化为 noopObserver
	generateRetries        int                 // LLM 生成调用最大尝试次数（默认 3）
	generateRetryBase      time.Duration       // 重试退避基准（默认 1s）
	networkRetries         int                 // 网络传输层错误的独立最大尝试次数（默认 6）
	networkRetryBase       time.Duration       // 网络传输层错误的重试退避基准（默认 5s）

	runTimeout   time.Duration // >0 启用墙钟熔断
	tokenBudget  int           // >0 启用累计 token 预算熔断
	repWindow    int           // >0 启用重复签名检测窗口
	repThreshold int           // 同签名触发提醒阈值
}

// maxGenerateRetryDelay 是生成重试指数退避的上限，避免退避时间吞掉整体预算。
const maxGenerateRetryDelay = 30 * time.Second

// maxNetworkRetryDelay 是网络传输层错误重试指数退避的上限，比 maxGenerateRetryDelay
// 更宽松——这类故障（TLS/DNS/连接建立）间歇性更强，值得多等一会儿而不是放弃整个 turn。
const maxNetworkRetryDelay = 60 * time.Second

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

// generateWithRetry 在 em.generate 之上叠加有界指数退避重试。
//
// 重试策略：
//   - 成功立即返回；
//   - context 已取消/超时（ctx.Err()!=nil）不重试，原样返回（会话级终止信号）；
//   - 其余错误视为可能瞬时，退避后重试，直到耗尽 attempts；
//   - 退避期间感知 ctx 取消，避免无谓等待。
func (e *AgentEngine) generateWithRetry(ctx context.Context, em emitter, turn int, history []schema.Message, toolDefs []schema.ToolDefinition) (*schema.Message, *schema.Usage, error) {
	attempts := e.generateRetries
	if attempts < 1 {
		attempts = 1
	}
	base := e.generateRetryBase
	if base <= 0 {
		base = 1 * time.Second
	}
	networkAttempts := e.networkRetries
	if networkAttempts < 1 {
		networkAttempts = 1
	}
	networkBase := e.networkRetryBase
	if networkBase <= 0 {
		networkBase = 5 * time.Second
	}

	var lastErr error
	for attempt := 1; ; attempt++ {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		msg, usage, err := em.generate(ctx, turn, history, toolDefs)
		if err == nil {
			return msg, usage, nil
		}
		lastErr = err
		// context 取消/超时不重试。
		if ctx.Err() != nil {
			return nil, nil, err
		}

		// 网络传输层错误（TLS/DNS/连接建立）间歇性更强，用独立、更宽松的预算，
		// 不与其他错误类别共享 attempts/base——分类基于本次失败的错误本身，
		// 不影响非网络错误仍然只用默认预算。
		maxAttempts, delayBase, delayCap := attempts, base, maxGenerateRetryDelay
		if isTransientNetworkError(err) {
			maxAttempts, delayBase, delayCap = networkAttempts, networkBase, maxNetworkRetryDelay
		}
		if attempt >= maxAttempts {
			break
		}

		delay := delayBase << (attempt - 1)
		if delay > delayCap {
			delay = delayCap
		}
		log.Print(logfmt.FormatMsg("engine", fmt.Sprintf(
			"LLM 生成失败 (turn %d, 尝试 %d/%d)，%s 后重试: %v", turn, attempt, maxAttempts, delay, err)))
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(delay):
		}
	}
	return nil, nil, lastErr
}

// isTransientNetworkError 判断 LLM 生成失败的根因是否为建立到 API 端点连接阶段的
// 瞬时网络故障（TLS/证书校验、DNS 解析、连接建立），而非业务层错误（如 4xx/5xx）。
// 这类故障间歇性更强，容错窗口需要比默认重试策略更宽——Terminal-Bench pilot 里
// 3 个任务在同一条 x509 证书错误上耗尽默认预算后直接放弃整个 turn，详见
// docs/技术调研/terminal-bench-轨迹分析-v1.md §2 R2。
//
// 采用字符串匹配而非 errors.As 类型断言：openai-go/anthropic-sdk-go 对底层
// net/http 错误的包装不保证保留可断言的具体类型，但错误消息里的关键字符串
// （x509:/tls:/no such host 等）是稳定、可观测的。
func isTransientNetworkError(err error) bool {
	msg := err.Error()
	for _, marker := range []string{
		"x509:", "tls:", "no such host", "connection refused",
		"connection reset", "i/o timeout", "dial tcp",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
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
	// terminated 在受控熔断发生时调用（每种模式决定如何呈现给消费者）。
	terminated func(data TerminationData)
	// stateChanged 在状态机流转时调用。
	stateChanged func(data StateChangeData)
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
			if msg.Content != "" {
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
		terminated: func(data TerminationData) {
			log.Print(logfmt.FormatMsg("engine", fmt.Sprintf("受控终止 [%s]: %s", data.Reason, data.Message)))
		},
		stateChanged: func(data StateChangeData) {
			log.Print(logfmt.FormatMsg("engine", fmt.Sprintf("状态流转 [%d]: %s -> %s", data.Turn, data.From, data.To)))
		},
	}
	return e.runLoop(ctx, userPrompt, "engine", em)
}

// runLoop 是 Run 与 RunStream 共享的主循环内核。
func (e *AgentEngine) runLoop(ctx context.Context, userPrompt string, logPrefix string, em emitter) error {
	log.Print(logfmt.FormatLoopStart(logPrefix, e.workDir, e.maxTurns, e.toolTimeout, e.maxConcurrentTools))

	// 可观测层接入：若未注入 observer 则退化为 noop。
	obs := e.observer
	if obs == nil {
		obs = noopObserver{}
	}
	// 单独加读锁读取 sessionID 用于 span 属性（与下方 sess/comp 快照锁分离，避免持锁时间过长）。
	e.mu.RLock()
	var sessIDForObs string
	if e.session != nil {
		sessIDForObs = e.session.SessionID()
	}
	e.mu.RUnlock()
	var interactionErr error
	turnCount := 0
	ctx = obs.OnInteractionStart(ctx, sessIDForObs, userPrompt)
	defer func() { obs.OnInteractionEnd(ctx, turnCount, interactionErr) }()

	// 在循环开始时快照 session 和 compactor，避免与 TUI goroutine 的 SetSession 产生数据竞争。
	e.mu.RLock()
	sess := e.session
	comp := e.compactor
	planMode := e.planMode
	todoStore := e.todoStore
	e.mu.RUnlock()

	// 启动时从 Session 恢复 TodoStore 状态（跨会话续接未完成任务）。
	if sess != nil && todoStore != nil {
		if todos, err := sess.GetTodos(ctx); err != nil {
			log.Print(logfmt.FormatMsg(logPrefix, fmt.Sprintf("加载 todos 失败: %v", err)))
		} else {
			todoStore.Write(todos)
		}
	}

	// Plan Mode：注入规划行为约束（write_file/edit_file 已由 filterReadOnlyTools 在工具层硬性过滤，
	// 此处只补充 bash 只读限制和 todo_write 输出要求等无法在工具层表达的行为规则）。
	if planMode == planning.PlanModePlan {
		userPrompt = "分析以下请求，用 todo_write 输出一份可直接执行的实现计划，然后用纯文字简述计划后停止。\n" +
			"todo 项要求：每条对应一个具体的实现动作（例如：创建某文件、实现某函数、运行某命令），\n" +
			"而非高层规划描述（禁止写\"需求澄清\"、\"方案设计\"之类无法直接执行的条目）。\n" +
			"如需了解当前代码库，可使用 read_file 或 bash（只读命令：ls、cat、find、grep）。\n" +
			"不要创建文件、执行 build/install 或做任何实际修改。\n\n" +
			userPrompt
	}

	contextHistory, startLen := e.loadHistoryWith(ctx, userPrompt, sess)

	// 结束时将 TodoStore 持久化到 Session（write-replace）。
	defer func() {
		if sess != nil && todoStore != nil {
			if err := sess.SaveTodos(ctx, todoStore.Read()); err != nil {
				log.Print(logfmt.FormatMsg(logPrefix, fmt.Sprintf("保存 todos 失败: %v", err)))
			}
		}
	}()

	overallStart := time.Now()

	// 行为护栏：单次 runLoop 一个守护实例（spec §4.4-§4.7）。
	guard := newLoopGuard(GuardConfig{
		MaxTurns:            e.maxTurns,
		RunTimeout:          e.runTimeout,
		TokenBudget:         e.tokenBudget,
		RepetitionWindow:    e.repWindow,
		RepetitionThreshold: e.repThreshold,
		StallWindow:         e.stallReminderWindow,
		StallText:           e.stallReminderText,
		MemoryInterval:      e.memoryReminderInterval,
		MemoryText:          e.memoryReminderText,
	}, overallStart)

	// 显式状态机：局部值 + 单一入口，多实例并发无竞态（spec §4.2）。
	state := StateIdle
	setState := func(to LoopState) {
		next := transition(state, to, turnCount)
		if next == state {
			return
		}
		from := state
		state = next
		if em.stateChanged != nil {
			em.stateChanged(StateChangeData{From: from, To: next, Turn: turnCount})
		}
		obs.OnStateChange(ctx, from, next, turnCount)
	}

	// 统一受控出口：发终止事件 → 保存历史（修复旧缺陷）→ 记录日志 → 返回 error。
	terminate := func(reason TerminationReason, msg string) error {
		interactionErr = fmt.Errorf("%s", msg)
		setState(StateTerminated)
		if em.terminated != nil {
			em.terminated(TerminationData{Reason: reason, Message: msg})
		}
		log.Print(logfmt.FormatMsg(logPrefix, fmt.Sprintf("受控终止 [%s]: %s", reason, msg)))
		e.saveHistoryWith(ctx, sess, contextHistory, startLen)
		return interactionErr
	}

	for {
		turnCount++
		setState(StateTurnStart)
		turnCtx := obs.OnTurnStart(ctx, turnCount)

		if err := guard.CheckTurn(turnCount); err != nil {
			reason, _ := guard.Terminated()
			return terminate(reason, err.Error())
		}
		select {
		case <-ctx.Done():
			// ctx 取消是外部中断（意外故障类），保持 EventError 语义不变。
			interactionErr = fmt.Errorf("context 已取消: %w", ctx.Err())
			return interactionErr
		default:
		}

		availableTools := e.registry.GetAvailableTools()
		if planMode == planning.PlanModePlan {
			availableTools = filterReadOnlyTools(availableTools)
		}
		toolTokens := memory.EstimateToolTokens(availableTools)

		// Preflight token check: estimate tokens before and after compaction.
		setState(StateCompacting)
		compactedHistory, compactionRecord := e.applyCompactionWith(comp, contextHistory)
		msgTokensAfter := memory.EstimateTokens(compactedHistory)
		totalTokens := msgTokensAfter + toolTokens

		if compactionRecord != nil && compactionRecord.Tier != memory.TierNone {
			em.compaction(*compactionRecord)
		}

		// Report current context token usage to TUI / CLI.
		em.tokenUpdate(totalTokens, e.contextWindow)

		// 三源 Reminder 仲裁（替代原先平铺的两个 nudge if 块）：
		// 重复升级会在这里裁决出终止。
		if txt, err := guard.EvaluateReminders(turnCount); err != nil {
			reason, _ := guard.Terminated()
			return terminate(reason, err.Error())
		} else if txt != "" {
			compactedHistory = appendUserNudge(compactedHistory, txt)
		}

		// Token 预算裁决（压缩后的估算值随响应一并累计）。
		if err := guard.CheckBudget(); err != nil {
			reason, _ := guard.Terminated()
			return terminate(reason, err.Error())
		}
		setState(StateGenerating)

		turnStart := time.Now()
		log.Print(logfmt.FormatTurnStart(logPrefix, turnCount, len(compactedHistory), len(availableTools)))

		llmStart := time.Now()
		responseMsg, usage, err := e.generateWithRetry(turnCtx, em, turnCount, compactedHistory, availableTools)
		if err != nil {
			interactionErr = err
			return fmt.Errorf("模型生成失败 (turn %d): %w", turnCount, err)
		}
		llmDuration := time.Since(llmStart)

		// 累计 input token：实际 usage 优先，缺失以本轮发送的上下文估算兜底。
		guard.AddUsage(usage, totalTokens)

		// 用实际 API 返回的 token 用量更新显示，替代之前的估算值。
		if usage != nil && usage.InputTokens > 0 {
			em.tokenUpdate(usage.InputTokens, e.contextWindow)
		}

		contextHistory = append(contextHistory, *responseMsg)

		if len(responseMsg.ToolCalls) == 0 {
			setState(StateDone)
			log.Print(logfmt.FormatTurnDone(logPrefix, turnCount, llmDuration, time.Since(overallStart)))
			obs.OnTurnEnd(turnCtx, turnCount, false)
			break
		}

		// 停滞计数与重复签名记录移交 guard.RecordToolCalls（含进展打破规则）。
		guard.RecordToolCalls(turnCount, responseMsg.ToolCalls)

		toolStart := time.Now()
		setState(StateToolExecuting)
		toolBudget := e.toolTimeout
		if rem, ok := guard.Remaining(); ok && (toolBudget <= 0 || rem < toolBudget) {
			toolBudget = rem // 单个工具不得冲破墙钟 deadline
		}
		results := e.executeTools(turnCtx, turnCount, responseMsg.ToolCalls, logPrefix, em, toolBudget)
		toolDuration := time.Since(toolStart)

		for i, toolCall := range responseMsg.ToolCalls {
			// 空输出兜底：部分 backend 拒绝空 content 的 tool_result（返回 400，
			// 在无重试时直接杀实例），且空 Observation 无信息可供模型推理、浪费一轮。
			content := results[i].Output
			if content == "" {
				content = "[工具执行完成，无输出]"
			}
			contextHistory = append(contextHistory, schema.Message{
				Role:       schema.RoleUser,
				Content:    content,
				ToolCallID: toolCall.ID,
				// 透传结构化错误信号，供 Provider 设置 tool_result.is_error，强化自愈。
				IsError: results[i].IsError,
			})
		}

		log.Print(logfmt.FormatObservation(logPrefix, turnCount, len(contextHistory), llmDuration, toolDuration, time.Since(turnStart)))
		obs.OnTurnEnd(turnCtx, turnCount, true)
	}

	e.saveHistoryWith(ctx, sess, contextHistory, startLen)
	log.Print(logfmt.FormatLoopEnd(logPrefix, turnCount, time.Since(overallStart)))
	return nil
}

// buildSystemPrompt 返回 system prompt 字符串。
// 若设置了 PromptBuilder 则委托给它，否则回退到内置默认文案。
func (e *AgentEngine) buildSystemPrompt() string {
	if e.promptBuilder != nil {
		return e.promptBuilder.Build()
	}
	return fmt.Sprintf(`你的名字是 harness9。请始终以 "harness9" 自称 — 不要使用 "AI 助手"、"语言模型" 或任何其他通称。

harness9 是一个通用 AI Agent，可完全访问用户的计算机。

能力：
- 执行 Shell 命令：运行程序、管理进程、安装软件包、与操作系统交互
- 读取、写入和编辑文件系统中的文件
- 将多个工具串联使用，自主完成复杂的多步骤任务

工作目录：%s

工作准则：
- 先调查后行动：优先读取文件并运行诊断命令
- 小步可验证地推进：每次重要操作后检查结果
- 命令失败时，诊断根本原因而非猜测
- 优先局部修改而非整体重写；保持现有风格和约定
- 任务描述模糊时，选择最合理的解释后直接推进`, e.workDir)
}

// loadHistoryWith 从 sess 加载历史消息，注入 system prompt 和当前用户输入。
// sess 为 nil 时退化为原有行为（全新 contextHistory）。
// 返回完整历史切片和新消息的起始索引（用于 saveHistoryWith）。
func (e *AgentEngine) loadHistoryWith(ctx context.Context, userPrompt string, sess memory.Session) ([]schema.Message, int) {
	var history []schema.Message
	if sess != nil {
		msgs, err := sess.GetMessages(ctx, 0)
		if err != nil {
			log.Print(logfmt.FormatMsg("engine", fmt.Sprintf("加载会话历史失败: %v", err)))
		} else {
			history = msgs
		}
	}
	// system prompt 不持久化到 DB，每次调用时重新注入
	if len(history) == 0 || history[0].Role != schema.RoleSystem {
		history = append([]schema.Message{{Role: schema.RoleSystem, Content: e.buildSystemPrompt()}}, history...)
	}
	startLen := len(history) // 新消息从此处开始；system prompt 不计入持久化范围
	history = append(history, schema.Message{Role: schema.RoleUser, Content: userPrompt})
	return history, startLen
}

// applyCompactionWith 对消息列表应用压缩策略。comp 为 nil 时原样返回。
// 若 compactor 实现 RecordedCompactor 接口，同时返回压缩审计记录（含 tier、锚点、外存条目等）。
func (e *AgentEngine) applyCompactionWith(comp memory.Compactor, msgs []schema.Message) ([]schema.Message, *memory.CompactionRecord) {
	if comp == nil {
		return msgs, nil
	}
	if rc, ok := comp.(memory.RecordedCompactor); ok {
		result, record := rc.CompactWithRecord(msgs)
		return result, &record
	}
	return comp.Compact(msgs), nil
}

// saveHistoryWith 将本次 Run 新增的消息（msgs[startLen:]）写回 sess。
// sess 为 nil 时为 no-op；失败仅打 warning 日志，不中断主流程。
func (e *AgentEngine) saveHistoryWith(ctx context.Context, sess memory.Session, msgs []schema.Message, startLen int) {
	if sess == nil || startLen >= len(msgs) {
		return
	}
	newMsgs := msgs[startLen:]
	if err := sess.AddMessages(ctx, newMsgs); err != nil {
		log.Print(logfmt.FormatMsg("engine", fmt.Sprintf("保存会话历史失败: %v", err)))
	}
}

// planModeWhitelist 是 Plan Mode 下允许 LLM 调用的工具名称白名单（read-only map，安全并发读）。
//
// 白名单的设计意图：
//   - read_file / bash：允许探索代码库，但 prompt 层约束 bash 只使用只读命令（ls/cat/find/grep）
//   - todo_write：Plan Mode 的核心产出——LLM 通过此工具输出结构化实现计划
//   - use_skill：允许加载 Skills 获取项目规范文档
//   - write_file / edit_file：不在白名单，从工具列表中硬性移除（工具层硬约束，优于 prompt 层软约束）
//   - web_search / web_fetch：不在白名单；Plan Mode 专注于本地代码库探索，
//     网络访问不属于规划阶段所需能力，如需联网研究请使用 Default/AutoEdit 模式
//   - memory_search / memory_write：不在白名单；Plan Mode 是轻量规划阶段，不触发记忆读写，
//     避免规划过程产生副作用（如写入错误的记忆条目）；记忆操作在 Default/AutoEdit 模式下可用
//   - task：不在白名单；Plan Mode 仅生成计划，不触发子代理委派（委派是执行阶段的动作）
var planModeWhitelist = map[string]bool{
	"read_file":  true,
	"bash":       true,
	"use_skill":  true,
	"todo_write": true,
}

// progressToolNames 是被视为"取得实质进展"的工具集合（用于 WithStallReminder 停滞提醒机制）。
// 调用其一即重置停滞计数：它们改变工作区状态（写入/编辑文件），是 Agent 真正推进任务的信号。
// 只读探索（read_file/bash grep 等）不计入进展，连续多轮只读即被判定为停滞。
var progressToolNames = map[string]bool{
	"write_file": true,
	"edit_file":  true,
}

// hasProgressTool 判断本轮工具调用中是否包含进展工具。
func hasProgressTool(calls []schema.ToolCall) bool {
	for _, tc := range calls {
		if progressToolNames[tc.Name] {
			return true
		}
	}
	return false
}

// appendUserNudge 返回在历史副本末尾追加一条 user nudge 消息的新切片。
// 不修改入参、不持久化——nudge 仅对当轮发送给 LLM 的临时副本可见。
func appendUserNudge(history []schema.Message, text string) []schema.Message {
	withNudge := make([]schema.Message, len(history), len(history)+1)
	copy(withNudge, history)
	return append(withNudge, schema.Message{Role: schema.RoleUser, Content: text})
}

// filterReadOnlyTools 从工具定义列表中过滤出 planModeWhitelist 中的子集，
// 在 Plan Mode 下替代完整工具列表传递给 LLM。
// 使用工具层硬约束而非 prompt 层软约束，确保 LLM 无论在何种上下文状态下都无法访问被过滤的工具。
func filterReadOnlyTools(tools []schema.ToolDefinition) []schema.ToolDefinition {
	var result []schema.ToolDefinition
	for _, t := range tools {
		if planModeWhitelist[t.Name] {
			result = append(result, t)
		}
	}
	return result
}

// executeTools 并发执行所有工具调用，每个工具带有独立的超时控制。
// toolBudget 为本轮全部工具共享的超时预算（已由 runLoop 用护栏墙钟剩余时间钳制），
// 0 表示使用 context 原始截止时间。通过预分配切片 + 索引写入保证结果顺序与 ToolCalls 一致。
func (e *AgentEngine) executeTools(ctx context.Context, turn int, toolCalls []schema.ToolCall, logPrefix string, em emitter, toolBudget time.Duration) []schema.ToolResult {
	log.Print(logfmt.FormatParallelTools(logPrefix, turn, len(toolCalls), e.maxConcurrentTools))

	results := make([]schema.ToolResult, len(toolCalls))
	var wg sync.WaitGroup

	var sem chan struct{}
	if e.maxConcurrentTools > 0 {
		sem = make(chan struct{}, e.maxConcurrentTools)
	}

	for i, toolCall := range toolCalls {
		wg.Add(1)
		go func(idx int, tc schema.ToolCall) {
			defer wg.Done()

			if sem != nil {
				sem <- struct{}{}
				defer func() { <-sem }()
			}

			toolCtx := ctx
			var cancel context.CancelFunc
			if toolBudget > 0 {
				toolCtx, cancel = context.WithTimeout(ctx, toolBudget)
				defer cancel()
			}

			if em.approval != nil {
				toolCtx = hooks.WithApprovalFn(toolCtx, em.approval)
			}

			em.toolStart(turn, tc)

			start := time.Now()
			results[idx] = e.registry.Execute(toolCtx, tc)
			em.toolDone(turn, tc, results[idx], time.Since(start))
		}(i, toolCall)
	}

	wg.Wait()
	return results
}
