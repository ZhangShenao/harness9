// TUI 核心：Bubbletea 模型定义、样式变量与 RunTUI 入口。
// 本文件定义 tuiModel 结构体（Elm Architecture Model）、所有 lipgloss 样式变量、
// newTUIModel 构造器以及 RunTUI 启动函数。
// 状态转换逻辑（Update）位于 tui_update.go；视图渲染（View）位于 tui_view.go。
package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"path/filepath"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/harness9/internal/engine"
	mcppkg "github.com/harness9/internal/mcp"
	"github.com/harness9/internal/memory"
	"github.com/harness9/internal/planning"
	"github.com/harness9/internal/sandbox"
	"github.com/harness9/internal/skills"
	"github.com/harness9/internal/subagent"
)

// package-level lipgloss 样式：在 View() 外定义，避免每帧重复分配。
var (
	userMsgStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("12")).
			Bold(true)

	assistantStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("10")).
			Bold(true)

	dimStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("240"))

	errorStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("9"))

	statusBarStyle = lipgloss.NewStyle().
			Background(lipgloss.Color("235")).
			Foreground(lipgloss.Color("11")).
			Padding(0, 1)

	// 工具执行阶段高亮
	toolRunStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))            // 黄色：运行中（工具名）
	verbRunStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("226"))           // 黄色：spinner + 动词
	toolOKStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))            // 绿色：成功
	toolErrStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))             // 红色：失败
	doneStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("10")).Bold(true) // 绿色粗体：任务完成
	skillStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("14"))            // 青色：技能激活
	cyanStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("81"))
	brandStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("226")).Bold(true)
	sepStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("237"))

	// token 使用率颜色（绿/黄/红，按使用量变化）
	tokenOKStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("10")) // < 50%: 绿
	tokenWarnStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("11")) // 50-80%: 黄
	tokenHighStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))  // > 80%: 红

	// 子代理流式进度样式 — 暗青色，缩进展示，区别于正文与工具进度
	subAgentLineStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("66"))

	// Thinking 块样式 — 深灰色，视觉上明显弱于正文
	thinkingHeaderStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("238")).Italic(true) // « thinking »
	thinkingLineStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("238"))              // │ 内容行
	thinkingEndStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("236"))              // └ 结束线

	// Shell 模式样式 — 对话流中 "!cmd" 输出的行级渲染样式。
	// 与 UI 指示器样式（shellStatusBarStyle 等）分组存放，方便按区域查找：
	//   shellCmdStyle    → "$ git status" 命令行本身（深蓝加粗，#33）
	//   shellOutputStyle → 命令 stdout/stderr 输出内容（浅灰 #250，视觉次要）
	//   shellOKStyle     → "✓ 完成 — 12ms" 成功结束行（绿色 #34）
	//   shellErrStyle    → "✗ 非零退出 — 3ms" 失败结束行（红色 #160）
	shellCmdStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("33")).Bold(true)
	shellOutputStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("250"))
	shellOKStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("34"))
	shellErrStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("160"))

	// Shell 模式 UI 指示器样式 — 控制状态栏、徽章、提示符的视觉主题。
	// 设计要点：两种模式（Default / Shell）通过状态栏背景色明确区分：
	//   Default  → 深灰底 #235（statusBarStyle）
	//   Shell    → 深绿底 #22（shellStatusBarStyle）
	// 颜色切换逻辑集中在 accentStyle() 和 activeStatusBarStyle() 两个方法中。
	shellStatusBarStyle = lipgloss.NewStyle().
				Background(lipgloss.Color("22")).
				Foreground(lipgloss.Color("120")).
				Padding(0, 1)
	// shellModeTagStyle 是输入区左侧 [SHELL] 徽章的样式。
	// 深橄榄底（#58）+ 亮黄文字（#226）：醒目但不刺眼，与对话文本颜色层次分明。
	shellModeTagStyle = lipgloss.NewStyle().
				Background(lipgloss.Color("58")).
				Foreground(lipgloss.Color("226")).
				Bold(true).
				Padding(0, 1)
	// shellModeAccentStyle 是 Shell 模式下状态栏和 footer 中 accent 文字的颜色（亮绿 #83）。
	// 在深绿背景（#22）上保持足够对比度。
	shellModeAccentStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("83"))
	// shellModePromptStyle 是 renderInput 中 "$ " 提示符样式。
	// 预计算为包级变量，避免 View() 每帧调用时重复执行 .Bold(true) 触发内存分配。
	shellModePromptStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("83")).Bold(true)
	// shellModeLabelInBarStyle 是状态栏内 "SHELL" 文本的样式（亮绿加粗）。
	shellModeLabelInBarStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("83")).Bold(true)

	// 审批对话框样式
	approvalBoxStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(lipgloss.Color("160")).
				Padding(0, 2).
				Width(60)
	approvalTitleHighStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("160")) // 高风险：红
	approvalTitleMedStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("208")) // 中风险：橙
	approvalTitleLowStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("220")) // 低风险：黄
	approvalSelectedStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("160"))

	// SandboxBar 样式 — 位于 StatusBar 下方，仅在有活跃 Sandbox 时显示
	sandboxBarBgStyle    = lipgloss.NewStyle().Background(lipgloss.Color("236")).Foreground(lipgloss.Color("242")).Padding(0, 1)
	sandboxRunningStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))  // 绿色：Running
	sandboxPendingStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))  // 黄色：Pending
	sandboxStoppingStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("240")) // 灰色：Stopping/Terminated
	sandboxFailedStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))   // 红色：Failed

	// MCP 状态栏样式 — 位于 SandboxBar 下方（如有），仅在有 MCP Server 时显示
	mcpBarBgStyle     = lipgloss.NewStyle().Background(lipgloss.Color("17")).Foreground(lipgloss.Color("33")).Padding(0, 1)
	mcpConnectedStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("10")) // 绿色：已连接
	mcpPendingStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("11")) // 黄色：连接中
	mcpFailedStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))  // 红色：失败
)

type tuiPhase int

const (
	phaseWelcome tuiPhase = iota
	phaseChat
)

var spinnerVerbs = []string{
	"思考中", "分析中", "处理中", "推理中", "计算中", "评估中",
}

// tuiModel 是 harness9 TUI 的 Bubbletea Elm 模型。
type tuiModel struct {
	// 展示配置（构造时设置，后续不变）
	workDir   string
	modelName string

	// 终端尺寸（由 WindowSizeMsg 更新）
	width, height int

	// Scrollback：所有已渲染行，仅追加
	lines []string

	// 滚动状态：-1 表示自动跟随底部（auto-scroll），≥0 表示手动滚动到该行索引
	viewTop int

	// Footer 组件
	spinner spinner.Model
	input   textinput.Model

	// Phase 状态机
	phase tuiPhase

	// Spinner 动词轮换
	verbIdx   int // 0-5，当前动词索引
	tickCount int // TickMsg 计数，每 30 次递增 verbIdx

	// 当前工具跟踪（用于 spinner 展示）
	// currentTool/toolArgs/toolStart 仅记录最近一次 EventToolStart 的信息，供 spinner 显示用。
	// 并发工具完成时的准确信息由 pendingTools 按 ToolCallID 索引，避免单变量覆盖。
	currentTool  string
	toolStart    time.Time
	toolArgs     json.RawMessage
	pendingTools map[string]pendingToolInfo // ToolCallID → 启动信息

	// Markdown 流式渲染状态：
	// streaming 期间将 delta 追加到 pendingReply，
	// 在工具边界（EventToolStart / EventDone）统一调用 glamour 渲染，
	// 替换 lines[pendingReplyStart:] 中的原始文本。
	pendingReply      string
	pendingReplyStart int

	// Thinking 块流式状态：
	// pendingThinking 累积当前轮次的推理文本；thinkingLineStart 记录 « thinking » 标题行在 lines 中的索引。
	// thinkingLineStart == -1 表示本轮尚未开始 thinking 块。
	pendingThinking   string
	thinkingLineStart int

	// Tab 斜杠命令补全状态
	typedPrefix    string   // 首次按 Tab 时的输入前缀（非空表示正在补全循环中）
	completions    []string // 与 typedPrefix 匹配的技能名列表
	completionIdx  int      // 当前循环位置
	completionHint string   // idle 时状态栏展示的补全提示

	// 运行时
	outerCtx    context.Context // 来自 main.go 的外部 context（SIGTERM 等信号）
	eng         *engine.AgentEngine
	skillsIndex *skills.Index
	eventCh     <-chan engine.Event
	cancelFn    context.CancelFunc
	running     bool

	// Session 管理
	manager   *memory.Manager
	session   memory.Session
	sessionID string // 完整 session ID，用于状态栏

	// Context token 跟踪（由 EventTokenUpdate 更新）
	contextTokens int // 当前估算 context token 数
	contextWindow int // 模型 context window（0 表示未知）

	// /resume 选择模式
	resumeSelecting bool
	resumeSessions  []memory.SessionInfo

	// Plan 跟踪：与 engine 共享同一个 *planning.PlanStore 实例。
	// 每次 plan_write 工具完成后，TUI 从 planStore 读取最新快照并渲染到对话流中。
	planStore *planning.PlanStore

	// 自动执行（autoExecuting）：plan_write 成功写入计划后激活。
	// EventDone 时检查是否有剩余 plan 条目，有则自动 dispatch(execContinuePrompt) 续跑。
	autoExecuting bool
	// autoExecPrevDone 记录上次 dispatch 时已完成的 plan 条目数量，
	// 用于判断本次 EventDone 是否有实际进度（completed 数是否增加）。
	autoExecPrevDone int
	// autoExecStuck 记录连续无进度的 dispatch 次数。
	// 达到 3 次后判定为停滞（LLM 空转），停止自动执行并提示用户手动介入。
	autoExecStuck int

	// pendingShellOutput 缓存本轮对话中用户通过 "!" 前缀执行的 Shell 命令及其输出，
	// 在用户下一次向 LLM 发送消息时由 dispatch() 前置注入 prompt 头部，
	// 使 LLM 可直接引用命令结果进行推理。
	// 每条 entry 格式：  "$ cmd\noutput..."（已在存储时截断至 maxShellContextLen）。
	// dispatch() 消费后置为 nil，避免重复注入。
	pendingShellOutput []string

	// subAgentTracker 是后台子代理任务跟踪器（单一事实源）。后台 goroutine 完成后写入 tracker，
	// dispatch() 在下次向 LLM 发送消息前通过 DrainCompleted 排空并前置注入 prompt（镜像 pendingShellOutput 机制）。
	subAgentTracker *subagent.TaskTracker
	// subAgentReg / subAgentRunner 支撑 @agent 前台直跑：根据名称解析子代理定义并直接驱动其引擎，绕过主 LLM。
	subAgentReg    *subagent.Registry
	subAgentRunner *subagent.Runner
	// directCh 是 @agent 前台直跑期间的进度/完成消息通道（镜像主引擎的 eventCh + readNextEvent 模式）。
	directCh chan subAgentDirectMsg
	// subAgentLines 缓存当前活跃子代理的流式进度行（由 EventSubAgent 追加），
	// 渲染为对话区下方的暗青色缩进块；新一轮用户消息开始时重置。
	subAgentLines []string
	// subAgentStreaming 标记 subAgentLines 末行是否为"正在累积的子代理正文行"。
	// 为 true 时，后续文本增量（delta）追加到末行而非新建行（避免每个 token 一行的刷屏）；
	// 遇到工具/启动/完成等非 delta 事件时置回 false，使下一段正文另起一行。
	subAgentStreaming bool
	// pendingSubAgentInject 缓存已收获、待注入下次 LLM 请求的后台子代理结果块。
	// 后台子代理完成时（subAgentNotifyMsg）即时显示到 scrollback 并写入此缓冲；
	// dispatch() 在下次发送 prompt 前将其前置注入并清空（展示与注入分离，避免与 TaskTracker 双重消费）。
	pendingSubAgentInject []string

	// compacting 为 true 时表示 /compact 命令正在异步执行中。
	// 此期间在输入框上方显示 spinner 进度条，完成后插入压缩通知并恢复为 false。
	compacting bool

	// shellMode 为 true 时表示输入框当前以 "!" 开头，处于 Shell 执行模式。
	// 由 Update() 中的输入实时检测逻辑驱动（非 running 状态下每次按键后检测），
	// View 层据此切换状态栏背景色（深绿）、输入区徽章（[SHELL]）、footer 提示文字。
	shellMode bool

	// 审批对话框状态：approvalPending=true 时渲染审批对话框，屏蔽普通输入
	approvalPending   bool
	approvalRequest   *engine.ApprovalRequest
	approvalCursor    int
	approvalFeedback  string
	approvalInputting bool
	settingsPath      string

	// permMode 是引擎的全局权限策略，影响状态栏显示和审批行为。
	permMode engine.PermissionMode

	// 后台任务面板状态（模态）：Ctrl+T / /tasks 打开。
	taskPanelMode    bool   // 任务面板是否激活（模态）
	taskPanelCursor  int    // 列表光标
	taskDetailID     string // 非空=在看某任务详情；空=看列表
	taskDetailScroll int    // 详情日志滚动偏移

	// Sandbox 状态展示（SandboxBar）
	sandboxes []sandbox.SandboxInfo        // 当前所有活跃 Sandbox 快照
	sandboxCh <-chan []sandbox.SandboxInfo // Manager 状态变更通知 channel（nil = 无 Sandbox）

	// MCP 状态展示（MCPBar）
	mcpServers []mcppkg.ServerStatus        // 当前所有 MCP Server 状态快照
	mcpCh      <-chan []mcppkg.ServerStatus // Manager 状态变更通知 channel（nil = 无 MCP）

	// MCP 工具面板（模态）：/mcp 命令打开，展示服务器 + 工具列表，支持编辑配置。
	mcpPanelMode   bool   // 面板是否激活
	mcpPanelScroll int    // 工具列表滚动偏移
	mcpConfigPath  string // .mcp.json 绝对路径（用于编辑器打开）
}

// pendingToolInfo 记录单个并发工具调用的启动信息，用于 EventToolResult 时精确还原名称和参数。
// 耗时由引擎侧在 toolDone 回调中精确计算并通过 ToolResultData.Duration 携带，此处不再记录 start。
type pendingToolInfo struct {
	name string
	args json.RawMessage
}

// newTUIModel 构造已初始化的 tuiModel：输入框聚焦，spinner 使用 Dot 样式。
func newTUIModel(eng *engine.AgentEngine, idx *skills.Index, mgr *memory.Manager, sess memory.Session, planStore *planning.PlanStore, tracker *subagent.TaskTracker, reg *subagent.Registry, runner *subagent.Runner, outerCtx context.Context, workDir, modelName string, sandboxCh <-chan []sandbox.SandboxInfo, mcpCh <-chan []mcppkg.ServerStatus) tuiModel {
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))

	ti := textinput.New()
	ti.Placeholder = "输入任务..."
	ti.CharLimit = 0
	ti.Focus()

	m := tuiModel{
		workDir:           workDir,
		modelName:         modelName,
		spinner:           sp,
		input:             ti,
		outerCtx:          outerCtx,
		eng:               eng,
		skillsIndex:       idx,
		viewTop:           -1, // -1 = 自动跟随底部
		phase:             phaseWelcome,
		manager:           mgr,
		session:           sess,
		planStore:         planStore,
		pendingTools:      make(map[string]pendingToolInfo),
		thinkingLineStart: -1, // -1 = 本轮尚未开始 thinking 块
		subAgentTracker:   tracker,
		subAgentReg:       reg,
		subAgentRunner:    runner,
		sandboxCh:         sandboxCh,
		mcpCh:             mcpCh,
		mcpConfigPath:     filepath.Join(workDir, ".mcp.json"),
	}
	if sess != nil {
		m.sessionID = sess.SessionID()
	}
	m.settingsPath = filepath.Join(workDir, ".harness9", "settings.json")
	return m
}

// Init 实现 tea.Model，启动输入框光标闪烁；若有 sandboxCh 同时启动 Sandbox 状态监听；若有 mcpCh 同时启动 MCP 状态监听。
func (m tuiModel) Init() tea.Cmd {
	cmds := []tea.Cmd{textinput.Blink}
	if m.sandboxCh != nil {
		cmds = append(cmds, waitSandboxUpdate(m.sandboxCh))
	}
	if m.mcpCh != nil {
		cmds = append(cmds, waitMCPUpdate(m.mcpCh))
	}
	return tea.Batch(cmds...)
}

// RunTUI 以 AltScreen 模式启动 Bubbletea 程序。
// 用户按 Ctrl-C/Ctrl-D（空闲时）退出后返回。
func RunTUI(ctx context.Context, eng *engine.AgentEngine, mgr *memory.Manager, sess memory.Session, idx *skills.Index, planStore *planning.PlanStore, tracker *subagent.TaskTracker, reg *subagent.Registry, runner *subagent.Runner, workDir, modelName string, sandboxCh <-chan []sandbox.SandboxInfo, mcpCh <-chan []mcppkg.ServerStatus) error {
	// TUI 独占终端，将内部日志重定向到静默，避免污染 AltScreen 输出。
	// 退出后恢复原 Writer，避免影响同进程其他逻辑（如测试框架）。
	origWriter := log.Writer()
	log.SetOutput(io.Discard)
	defer log.SetOutput(origWriter)
	m := newTUIModel(eng, idx, mgr, sess, planStore, tracker, reg, runner, ctx, workDir, modelName, sandboxCh, mcpCh)
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithContext(ctx), tea.WithMouseCellMotion())
	// 后台子代理完成时，经 TaskTracker 通知回调向 TUI 投递 subAgentNotifyMsg，触发即时完成提示。
	// p.Send 是 goroutine-safe 的，可从后台 goroutine 调用。
	if tracker != nil {
		tracker.SetNotify(func() { p.Send(subAgentNotifyMsg{}) })
	}
	_, err := p.Run()
	return err
}
