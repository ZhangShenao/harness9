// Package engine — AgentEngine 的函数式配置选项。
//
// 所有选项统一采用 `With` 前缀（见 AGENTS.md §3.2 命名规范），在 NewAgentEngine
// 构造时一次性注入。运行期可变的个别状态（session / planMode）另提供线程安全的
// Set* 方法，供 TUI goroutine 跨 goroutine 调用；修改仅对下一次 Run/RunStream 生效。
package engine

import (
	"time"

	"github.com/harness9/internal/memory"
	"github.com/harness9/internal/planning"
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

// WithMemoryNudge 配置长期记忆 nudge：每隔 interval 个 turn 在发送给 LLM 的历史中
// 注入一行 text 提示（仅注入到临时副本，不持久化）。interval<=0 时关闭。
func WithMemoryNudge(interval int, text string) Option {
	return func(e *AgentEngine) {
		e.nudgeInterval = interval
		e.nudgeText = text
	}
}

// WithStallNudge 配置停滞 nudge：当连续 window 个"使用了工具但未调用任何进展工具
// （edit_file/write_file）"的 turn 后，向发送给 LLM 的历史副本注入一次 text 提示，
// 然后重置计数。用于打断"反复静态重读却不收敛"的空转（SWE-bench 轨迹中 xarray-3364、
// pylint-7080 烧满 80 轮即此形态）。
//
// 与 WithMemoryNudge 一致：仅注入到临时副本，绝不持久化、不累积；window<=0 时关闭。
// 进展工具集合见 progressToolNames。
func WithStallNudge(window int, text string) Option {
	return func(e *AgentEngine) {
		e.stallWindow = window
		e.stallText = text
	}
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

// PromptBuilder 构造 Agent 的 system prompt。
// 接口定义在 engine 包（使用者侧），由 internal/context 包实现。
// 引擎通过此接口与 Context Engineering 模块解耦。
type PromptBuilder interface {
	Build() string
}
