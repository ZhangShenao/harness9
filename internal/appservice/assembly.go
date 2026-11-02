package appservice

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"sync"
	"time"

	harctx "github.com/harness9/internal/context"
	"github.com/harness9/internal/engine"
	"github.com/harness9/internal/hooks"
	"github.com/harness9/internal/logfmt"
	"github.com/harness9/internal/ltm"
	mcppkg "github.com/harness9/internal/mcp"
	"github.com/harness9/internal/memory"
	"github.com/harness9/internal/observability"
	"github.com/harness9/internal/permission"
	"github.com/harness9/internal/planning"
	"github.com/harness9/internal/provider"
	"github.com/harness9/internal/sandbox"
	"github.com/harness9/internal/skills"
	"github.com/harness9/internal/subagent"
	"github.com/harness9/internal/tools"
)

// generalPurposeSystemPrompt 是内置 general-purpose 子代理的 system prompt，
// 与 cmd/harness9/main.go 保持同一份文案（internal 包不能反向 import cmd，
// 二期重构时可把该常量下沉到 subagent 包作为唯一事实源）。
const generalPurposeSystemPrompt = `你是一个通用型子代理（general-purpose sub-agent），在与主代理隔离的独立上下文中完成被委派的任务。

工作原则：
1. 你看不到主代理的对话历史，完成任务所需的全部信息都在传入的 prompt 中。若信息不足，基于合理假设继续推进，并在最终结论中显式说明你做出的假设。
2. 你拥有与主代理相同的工具集（读写文件、执行命令、调用 skill 等），可自主决定如何分解并完成任务。优先用工具去探索与验证，不要把未经核实的猜测当作结论。
3. 你的最终回复是返回给主代理的唯一结果，因此必须自包含、结构清晰、聚焦结论：说明你做了什么、得到的关键结果、以及主代理需要据此采取的后续动作。省略无价值的中间过程，但保留关键的文件路径、命令输出与证据引用。
4. 完成任务后立即停止，不要做与任务无关的额外操作，也不要反复确认。`

// agentMaxTurns 是主代理与子代理共用的最大 Turn 数（与 main.go 的约定一致）。
const agentMaxTurns = 500

// sharedStack 是跨会话共享的装配产物。生命周期与 Service 一致。
//
// 划分原则：凡与具体 sessionID 无关、且构造成本高或持有外部资源的对象都放在这里
// （provider 连接池、docker 容器、SQLite 连接、MCP 子进程、LTM 精华视图）。
type sharedStack struct {
	modelWindow int // 模型上下文窗口（token）

	// llm 是最终参与引擎装配的 provider（可能被 TracingProvider 包装）。
	llm provider.LLMProvider

	skillsIndex *skills.Index
	sandboxEnv  sandbox.Environment // nil = 本地执行路径
	sandboxMgr  *sandbox.Manager    // 仅 docker 后端非 nil
	// sandboxBackendLabel 是沙箱后端标签：docker / seatbelt / off / docker-degraded。
	sandboxBackendLabel  string
	sandboxDegradeReason string

	promptBuilder engine.PromptBuilder

	mgr         *memory.Manager
	ltmStore    *ltm.Store
	ltmPrecis   *ltm.Precis
	recordStore *memory.FileRecordStore

	mcpMgr *mcppkg.Manager // 未配置 MCP server 时为 nil
	// mcpReadyCh 在 Manager.Start 返回（全部 server 连接完成或超时失败）后关闭，
	// 供晚连接场景的补注入判断（见 injectMCPToolsOnce）。
	mcpReadyCh    chan struct{}
	mcpInjectOnce sync.Once

	// 公共 hook（会话无关）：permHook 每次调用重读规则文件；dangerHook 无状态；
	// obsHook 仅观测。offloadHook 因绑定 sessionID，在 SessionStack 中构建。
	permHook     hooks.ToolHook
	dangerHook   hooks.ToolHook
	obsHook      hooks.ToolHook
	settingsPath string

	otel *observability.Providers
}

// close 释放共享栈持有的全部外部资源（fail-soft，逐项记录日志）。
func (ss *sharedStack) close(ctx context.Context) {
	if ss.mcpMgr != nil {
		ss.mcpMgr.Stop()
	}
	if ss.sandboxMgr != nil {
		ss.sandboxMgr.DestroyAll(ctx)
	}
	if ss.mgr != nil {
		if err := ss.mgr.Close(); err != nil {
			log.Print(logfmt.FormatMsg("appservice", fmt.Sprintf("关闭会话数据库失败: %v", err)))
		}
	}
	if ss.otel != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = ss.otel.Shutdown(shutdownCtx)
	}
}

// sandboxStatus 返回沙箱状态快照（桌面端 SandboxBadge 轮询使用）。
func (ss *sharedStack) sandboxStatus() []sandbox.SandboxInfo {
	switch ss.sandboxBackendLabel {
	case "docker":
		if ss.sandboxMgr != nil {
			return ss.sandboxMgr.ListAll()
		}
		// 已降级：返回失败快照让 UI 呈现红色降级提示
		return []sandbox.SandboxInfo{{State: sandbox.StateFailed}}
	case "seatbelt":
		return []sandbox.SandboxInfo{{ID: "seatbelt", State: sandbox.StateRunning}}
	default: // off
		return nil
	}
}

// buildSharedStack 按与 cmd/harness9/main.go 一致的顺序装配共享组件。
func (s *Service) buildSharedStack(ctx context.Context) (*sharedStack, error) {
	cfg := s.cfg
	workDir := cfg.WorkDir
	homeDir := cfg.HomeDir
	ss := &sharedStack{}

	// ---- Observability（OTEL）----
	if cfg.DisableObservability {
		ss.otel = observability.NewNoopProviders()
	} else {
		otelCfg := observability.ConfigFromEnv()
		providers, err := observability.Setup(ctx, otelCfg)
		if err != nil {
			log.Print(logfmt.FormatMsg("appservice", fmt.Sprintf("OTEL 初始化失败，已降级为 noop: %v", err)))
			providers = observability.NewNoopProviders()
		}
		ss.otel = providers
	}

	// ---- Skills ----
	skillsIndex, err := skills.LoadSkills(filepath.Join(workDir, "skills"))
	if err != nil {
		return nil, fmt.Errorf("加载 skills：%w", err)
	}
	ss.skillsIndex = skillsIndex

	// ---- Provider ----
	factory := cfg.ProviderFactory
	if factory == nil {
		factory = defaultProviderFactory
	}
	rawLLM, window, err := factory(cfg.Model)
	if err != nil {
		return nil, fmt.Errorf("创建 Provider：%w", err)
	}
	ss.modelWindow = window
	var llm provider.LLMProvider = rawLLM
	if tp, tpErr := observability.NewTracingProvider(llm, ss.otel, cfg.Model); tpErr != nil {
		log.Print(logfmt.FormatMsg("appservice", fmt.Sprintf("TracingProvider 创建失败（使用原始 provider）: %v", tpErr)))
	} else {
		llm = tp
	}
	ss.llm = llm

	// ---- Sandbox（按后端分派；对标 Codex 的 approval/sandbox 策略分离）----
	ss.settingsPath = filepath.Join(workDir, ".harness9", "settings.json")
	switch cfg.SandboxBackend {
	case "off":
		ss.sandboxBackendLabel = "off"
	case "seatbelt":
		// macOS 原生 sandbox-exec 沙箱：无容器生命周期，直接构造即可。
		ss.sandboxEnv = sandbox.NewSeatbeltEnvironment(workDir, sandbox.SeatbeltWorkspaceWrite)
		ss.sandboxBackendLabel = "seatbelt"
	default: // "" / "docker"：与 CLI 完全一致的 Docker 路径
		sandboxCfg := sandbox.DefaultConfig()
		if sandboxCfg.Enabled {
			ss.sandboxMgr = sandbox.NewManager(sandboxCfg)
			if err := ss.sandboxMgr.ReapOrphans(ctx); err != nil {
				log.Print(logfmt.FormatMsg("appservice", fmt.Sprintf("清理孤儿 Sandbox 失败（忽略）: %v", err)))
			}
			env, sandboxErr := ss.sandboxMgr.CreateWithRetry(ctx, workDir)
			if sandboxErr != nil {
				log.Print(logfmt.FormatMsg("appservice", fmt.Sprintf("Sandbox 启动失败，已降级为本地进程模式: %v", sandboxErr)))
				ss.sandboxMgr = nil
				ss.sandboxDegradeReason = fmt.Sprintf("Docker Sandbox 启动失败：%v", sandboxErr)
				ss.sandboxBackendLabel = "docker-degraded"
			} else {
				ss.sandboxEnv = env
				ss.sandboxBackendLabel = "docker"
			}
		} else {
			ss.sandboxBackendLabel = "off"
		}
	}

	// ---- PromptBuilder ----
	pb := harctx.NewPromptBuilder(workDir, skillsIndex).
		WithPlanEnabled(true).
		WithOffloadEnabled(true)
	// 执行环境说明按后端分派：Seatbelt 必须用独立文案——Docker 文案的
	// "root in Ubuntu / 操作不影响真实系统 / apt-get 安装" 对 macOS 原生沙箱
	// 全部不成立，模型据此行动会越界破坏真实工作区。
	switch {
	case ss.sandboxBackendLabel == "seatbelt":
		pb = pb.WithSeatbeltContext(true)
	case ss.sandboxEnv != nil:
		pb = pb.WithSandboxContext(true)
	case ss.sandboxDegradeReason != "":
		pb = pb.WithSandboxDegraded(ss.sandboxDegradeReason)
	}

	// ---- Memory（会话数据库）----
	dbPath := cfg.DBPath
	if dbPath == "" {
		dbPath = filepath.Join(homeDir, ".harness9", "sessions.db")
	}
	toolResultsDir := cfg.ToolResultsDir
	if toolResultsDir == "" {
		toolResultsDir = filepath.Join(workDir, ".harness9", "tool_results")
	}
	compactionRecordsDir := cfg.CompactionRecordsDir
	if compactionRecordsDir == "" {
		compactionRecordsDir = filepath.Join(homeDir, ".harness9", "compaction_records")
	}
	mgr, err := memory.NewManager(dbPath,
		memory.WithToolResultsDir(toolResultsDir),
		memory.WithCompactionRecordsDir(compactionRecordsDir),
	)
	if err != nil {
		return nil, fmt.Errorf("初始化 Memory Manager：%w", err)
	}
	ss.mgr = mgr
	ss.recordStore = memory.NewFileRecordStore(compactionRecordsDir)

	// ---- Long-Term Memory ----
	ltmStore, err := ltm.NewStore(mgr.DB())
	if err != nil {
		ss.close(ctx)
		return nil, fmt.Errorf("初始化长期记忆 Store：%w", err)
	}
	ss.ltmStore = ltmStore
	ltmPrecis := ltm.NewPrecis(ltmStore, filepath.Join(homeDir, ".harness9", "memories", "MEMORY.md"), 5120)
	if _, err := ltmStore.PurgeExpired(ctx); err != nil {
		log.Print(logfmt.FormatMsg("appservice", fmt.Sprintf("回收过期记忆失败: %v", err)))
	}
	if err := ltmPrecis.Regenerate(ctx); err != nil {
		log.Print(logfmt.FormatMsg("appservice", fmt.Sprintf("重建记忆精华失败: %v", err)))
	}
	ss.ltmPrecis = ltmPrecis
	pb = pb.WithLongTermMemory(func() string {
		content, _ := ltmPrecis.Read()
		return content
	})
	ss.promptBuilder = pb

	// ---- MCP（异步启动，fail-soft）----
	mcpCfg, mcpCfgErr := mcppkg.LoadConfig(filepath.Join(workDir, ".mcp.json"))
	if mcpCfgErr != nil {
		log.Print(logfmt.FormatMsg("appservice", fmt.Sprintf("加载 MCP 配置失败（跳过）: %v", mcpCfgErr)))
	}
	if len(mcpCfg.Servers) > 0 {
		mcpMgr := mcppkg.NewManager(mcpCfg)
		ss.mcpMgr = mcpMgr
		ss.mcpReadyCh = make(chan struct{})
		ready := ss.mcpReadyCh
		go func() {
			defer close(ready)
			if err := mcpMgr.Start(ctx); err != nil {
				log.Print(logfmt.FormatMsg("appservice", fmt.Sprintf("MCP Manager 启动失败: %v", err)))
			}
		}()
	}

	// ---- 公共 hooks ----
	ss.permHook = permission.NewFileHook(ss.settingsPath)
	ss.dangerHook = hooks.NewDangerHook()
	if oh, ohErr := observability.NewObservabilityHook(ss.otel); ohErr != nil {
		log.Print(logfmt.FormatMsg("appservice", fmt.Sprintf("ObservabilityHook 创建失败（跳过）: %v", ohErr)))
	} else {
		ss.obsHook = oh
	}

	return ss, nil
}

// injectMCPToolsOnce 在 MCP 连接就绪后把工具注入会话 registry（Service 生命周期内一次）。
//
// 为什么不能只在栈构建时注入一次：buildSharedStack 异步启动 Manager，npx 等
// server 冷启动可达数十秒——构建会话栈时连接往往尚未完成，一次性注入会让该
// 会话永久缺失 MCP 工具。SendMessage 前调用本方法，晚连接的 server 在下一条
// 消息即可用。Registry.Register 对重复注册返回 ErrAlreadyRegistered、
// Manager.InjectTools 会跳过，因此注入天然幂等；once 保证不重复刷 skip 日志。
// 之后新建的栈在 buildSessionStack 里注入（届时必然已就绪）。
func (ss *sharedStack) injectMCPToolsOnce(stack *SessionStack) {
	if ss.mcpMgr == nil || ss.mcpReadyCh == nil {
		return
	}
	select {
	case <-ss.mcpReadyCh:
	default:
		return // 连接尚未完成，下一条消息再试
	}
	ss.mcpInjectOnce.Do(func() {
		if n := ss.mcpMgr.InjectTools(stack.registry); n > 0 {
			log.Print(logfmt.FormatMsg("appservice", fmt.Sprintf("MCP: 补注入 %d 个工具", n)))
		}
	})
}

// formatCompletedTasks 把已完成的后台子代理任务格式化为注入块，
// 语义对标 TUI 的 pendingSubAgentInject（后台结果必须回流到主对话）。
func formatCompletedTasks(completed []subagent.CompletedTask) []string {
	blocks := make([]string, 0, len(completed))
	for _, c := range completed {
		status := "完成"
		if c.IsError {
			status = "失败"
		}
		blocks = append(blocks, fmt.Sprintf(
			"[后台子代理任务%s] agent=%s task=%s\n最终结果：\n%s",
			status, c.AgentName, c.TaskID, c.FinalText))
	}
	return blocks
}

// defaultProviderFactory 是生产默认的 Provider 工厂：NewFromEnv + 模型窗口注册表。
func defaultProviderFactory(model string) (provider.LLMProvider, int, error) {
	p, err := provider.NewFromEnv(model)
	if err != nil {
		return nil, 0, err
	}
	return p, provider.GetModelLimits(model).ContextTokens, nil
}

// SessionStack 是单个会话的完整运行栈：全部与 sessionID 绑定的对象。
// 切换/打开会话时整体重建，保证 offload / plan writer / compaction offloader /
// 压缩记录的 session 归属永远正确。
type SessionStack struct {
	session memory.Session
	info    SessionInfoDTO

	planStore *planning.PlanStore
	offload   hooks.ToolHook
	registry  tools.Registry
	hookReg   tools.Registry
	engine    *engine.AgentEngine
	taskTool  *subagent.TaskTool
	// tracker 是本会话后台子代理任务的单一事实源；桌面端没有 TUI 的轮询面板，
	// 完成结果靠事件泵收割进 pendingInject、下一条消息前置注入（见 events.go）。
	tracker *subagent.TaskTracker
	// pendingInject 是待注入下一条 prompt 的文本块（s.mu 保护）。
	pendingInject []string
}

// buildSessionStack 为指定会话构建全新运行栈。
func (s *Service) buildSessionStack(sess memory.Session) (*SessionStack, error) {
	ss := s.shared
	cfg := s.cfg
	workDir := cfg.WorkDir
	homeDir := cfg.HomeDir
	sessionID := sess.SessionID()

	planStore := planning.NewPlanStore()
	planWriter, err := hooks.NewFilePlanWriter(workDir, homeDir, sessionID)
	if err != nil {
		return nil, fmt.Errorf("初始化 FilePlanWriter：%w", err)
	}
	offloadHook := hooks.NewOffloadHook(workDir, sessionID)

	registry := tools.NewRegistry()
	for _, tool := range []tools.BaseTool{
		tools.NewReadFileTool(workDir, tools.ReadFileWithEnvironment(ss.sandboxEnv)),
		tools.NewWriteFileTool(workDir, tools.WriteFileWithEnvironment(ss.sandboxEnv)),
		tools.NewBashTool(workDir, tools.WithEnvironment(ss.sandboxEnv)),
		tools.NewEditFileTool(workDir, tools.EditFileWithEnvironment(ss.sandboxEnv)),
		skills.NewUseSkillTool(ss.skillsIndex),
		tools.NewPlanWriteTool(planStore, tools.WithPlanWriter(planWriter)),
		tools.NewMemoryWriteTool(ss.ltmStore, ss.ltmPrecis),
		tools.NewMemorySearchTool(ss.ltmStore),
		tools.NewWebFetchTool(),
		tools.NewWebSearchTool(),
	} {
		if err := registry.Register(tool); err != nil {
			return nil, fmt.Errorf("注册工具 %s：%w", tool.Name(), err)
		}
	}

	// MCP 工具注入：把已连接的 MCP server 工具注册进本会话的 registry（fail-soft）。
	if ss.mcpMgr != nil {
		if injected := ss.mcpMgr.InjectTools(registry); injected > 0 {
			log.Print(logfmt.FormatMsg("appservice", fmt.Sprintf("MCP: 注入 %d 个工具", injected)))
		}
	}

	// ---- Sub-Agent 接线（会话级：runner 的 SharedHooks 含会话级 offloadHook）----
	// 沙箱继承：docker 后端沿用 Runner 为每个子代理创建独立容器的路径（SandboxMgr）；
	// Seatbelt 等无 Manager 后端必须显式共享父环境——否则子代理的 bash 直跑宿主机，
	// 父会话被沙箱约束、委派出去却不受限，违反"权限只能更严不能扩权"不变量。
	subAgentEnv := ss.sandboxEnv
	if ss.sandboxMgr != nil {
		subAgentEnv = nil
	}
	subAgentBaseTools := []tools.BaseTool{
		tools.NewReadFileTool(workDir, tools.ReadFileWithEnvironment(subAgentEnv)),
		tools.NewWriteFileTool(workDir, tools.WriteFileWithEnvironment(subAgentEnv)),
		tools.NewBashTool(workDir, tools.WithEnvironment(subAgentEnv)),
		tools.NewEditFileTool(workDir, tools.EditFileWithEnvironment(subAgentEnv)),
		skills.NewUseSkillTool(ss.skillsIndex),
		tools.NewWebFetchTool(),
		tools.NewWebSearchTool(),
	}
	subAgentReg := subagent.NewRegistry()
	if err := subAgentReg.Register(subagent.SubAgentDefinition{
		Name:         "general-purpose",
		Description:  "通用子代理，处理需要兼顾探索与修改、复杂推理或多步依赖的任务。当任务边界清晰、可独立完成、且希望隔离上下文（仅回传最终结论而非冗长中间过程）时使用；在没有更专门的子代理可用时，它是默认兜底选择。继承父代理可用的全部工具与模型。",
		SystemPrompt: generalPurposeSystemPrompt,
		Source:       "builtin",
	}); err != nil {
		log.Print(logfmt.FormatMsg("appservice", fmt.Sprintf("注册内置子代理失败: %v", err)))
	}
	if err := subAgentReg.LoadFromDir(filepath.Join(workDir, ".harness9", "agents")); err != nil {
		log.Print(logfmt.FormatMsg("appservice", fmt.Sprintf("加载文件式子代理失败: %v", err)))
	}

	// 后台任务追踪器会话级新建（桌面端任务面板按会话展示，不跨会话串状态）。
	tracker := subagent.NewTaskTracker()
	runner := subagent.NewRunner(subagent.RunnerConfig{
		BaseTools:             subAgentBaseTools,
		SharedHooks:           []hooks.ToolHook{ss.dangerHook, offloadHook},
		SettingsPath:          ss.settingsPath,
		SkillsIndex:           ss.skillsIndex,
		WorkDir:               workDir,
		DefaultMaxTurns:       agentMaxTurns,
		ToolTimeout:           60 * time.Second,
		MaxConcurrentTools:    0,
		SandboxMgr:            ss.sandboxMgr,
		SandboxDegradedReason: ss.sandboxDegradeReason,
		ProviderFor: func(model string) (provider.LLMProvider, int, error) {
			if model == "" {
				return ss.llm, ss.modelWindow, nil
			}
			if s.cfg.ProviderFactory != nil {
				p, w, err := s.cfg.ProviderFactory(model)
				return p, w, err
			}
			p, err := provider.NewFromEnv(model)
			if err != nil {
				return nil, 0, err
			}
			return p, provider.GetModelLimits(model).ContextTokens, nil
		},
		CompactorFor: func(p provider.LLMProvider, ctxWin int) memory.Compactor {
			return memory.NewProgressiveCompactor(p, ctxWin)
		},
		BaseCtx: s.ctx,
	})
	taskTool := subagent.NewTaskTool(subAgentReg, runner, tracker)
	if err := registry.Register(taskTool); err != nil {
		log.Print(logfmt.FormatMsg("appservice", fmt.Sprintf("注册 task 工具失败: %v", err)))
	}

	// Hook 顺序与 main.go 一致：Permission → Danger → Offload → Observability。
	mainHooks := []hooks.ToolHook{ss.permHook, ss.dangerHook, offloadHook}
	if ss.obsHook != nil {
		mainHooks = append(mainHooks, ss.obsHook)
	}
	hookReg := hooks.NewHookRegistry(registry, mainHooks...)

	// ProgressiveCompactor：同一 LLM 生成摘要；session 归属的 offloader/record store。
	compactionOffloader := memory.NewCompactionOffloader(workDir, sessionID)
	compactor := memory.NewProgressiveCompactor(ss.llm, ss.modelWindow,
		memory.WithProgressiveMemoryExtractor(ltm.NewExtractor(ss.llm, ss.ltmStore)),
		memory.WithProgressiveRecordStore(ss.recordStore),
		memory.WithProgressiveOffloader(compactionOffloader),
		memory.WithProgressiveSessionID(sessionID),
	)

	var engOpts []engine.Option
	if s.cfg.PermissionMode != engine.PermissionModeDefault {
		engOpts = append(engOpts, engine.WithPermissionMode(s.cfg.PermissionMode))
	}
	engOpts = append(engOpts,
		engine.WithPromptBuilder(ss.promptBuilder),
		engine.WithSession(sess),
		engine.WithCompactor(compactor),
		engine.WithContextWindow(ss.modelWindow),
		engine.WithPlanStore(planStore),
		engine.WithMemoryNudge(10, "如果本轮对话中出现了值得跨会话长期保留的信息（用户偏好、稳定的项目知识、关键决策、可复用技能），请调用 memory_write 工具记录；否则忽略此提示。"),
	)
	if eo, eoErr := observability.NewOTELEngineObserver(ss.otel); eoErr == nil {
		engOpts = append(engOpts, engine.WithEngineObserver(eo))
	}
	eng := engine.NewAgentEngine(ss.llm, hookReg, workDir, engOpts...)

	// 读取持久化标题（若已存在）
	title := ""
	if infos, err := ss.mgr.ListSessions(s.ctx); err == nil {
		for _, in := range infos {
			if in.ID == sessionID {
				title = in.Title
				break
			}
		}
	}

	// 恢复持久化的执行计划：引擎要到下一次 run 开始才加载 Plan，
	// 不在此处恢复会让重新打开的会话在 UI 上丢失既有计划进度。
	if items, err := sess.GetPlan(s.ctx); err == nil && len(items) > 0 {
		planStore.Write(items)
	}

	return &SessionStack{
		session:   sess,
		info:      SessionInfoDTO{ID: sessionID, Title: title},
		planStore: planStore,
		offload:   offloadHook,
		registry:  registry,
		hookReg:   hookReg,
		engine:    eng,
		taskTool:  taskTool,
		tracker:   tracker,
	}, nil
}
