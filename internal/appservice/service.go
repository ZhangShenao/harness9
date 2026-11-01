// Package appservice 提供 harness9 的应用服务层（application service layer）。
//
// 设计对标 Codex 的 app-server 架构：把「引擎 + 工具 + 沙箱 + 记忆 + 钩子」的
// 装配逻辑从入口程序（cmd/harness9/main.go）中抽取为可编程的服务接口，
// 让多种客户端（桌面 GUI、未来的 JSON-RPC app-server、测试）共用同一套
// 装配与运行时语义，避免每个前端各自复制一份 main.go。
//
// 职责边界：
//   - 装配：Config → sharedStack（跨会话共享：provider/sandbox/memory/ltm/mcp）
//   - SessionStack（会话级：engine/hooks/planStore/compactor，切换会话时重建）
//   - 运行：SendMessage 启动 RunStream 并用后台 goroutine 泵事件（审批请求
//     入队而非阻塞泵，规避引擎无缓冲事件通道在多工具并发审批下的死锁）
//   - 审批：RespondApproval 把 UI 决策送达引擎的 ResponseCh（恰好一次）
//   - 查询：会话 CRUD、Plan、git diff、沙箱/MCP 状态、设置持久化
//
// 并发模型：单个 Service 实例同一时刻至多一个活跃 SessionStack、至多一个
// 在跑的 agent run（与 TUI 的 m.running 单飞语义一致）。所有公开方法并发安全。
//
// 该层的所有对外 DTO 均为可 JSON 序列化的纯数据结构，为二期把服务平移为
// JSON-RPC over stdio 的独立进程（对标 codex app-server）预留接缝。
package appservice

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/harness9/internal/engine"
	"github.com/harness9/internal/env"
	"github.com/harness9/internal/logfmt"
	"github.com/harness9/internal/memory"
	"github.com/harness9/internal/planning"
	"github.com/harness9/internal/provider"
	"github.com/harness9/internal/sandbox"
)

// ErrNoActiveSession / ErrRunInProgress 是 Service 的哨兵错误。
var (
	// ErrNoActiveSession 表示当前没有激活的会话（需先 NewSession/OpenSession）。
	ErrNoActiveSession = errors.New("没有激活的会话")
	// ErrRunInProgress 表示已有 agent run 在执行中（单飞语义）。
	ErrRunInProgress = errors.New("已有任务在执行中")
	// ErrServiceClosed 表示 Service 已关闭。
	ErrServiceClosed = errors.New("服务已关闭")
)

// Config 是 Service 的构造配置。零值字段自动取与 cmd/harness9/main.go 相同的默认值。
type Config struct {
	// WorkDir 是 Agent 的沙箱根目录与配置锚点（.env / AGENTS.md / skills / .mcp.json / .harness9/）。必填。
	WorkDir string
	// HomeDir 是用户级状态目录的根（默认 os.UserHomeDir()，存放 sessions.db 与 memories）。
	HomeDir string
	// Model 是 LLM 模型名（默认 $LLM_MODEL，兜底 openai/gpt-4o-mini）。
	Model string
	// PermissionMode 是引擎权限模式（默认 PermissionModeDefault）。
	PermissionMode engine.PermissionMode
	// SandboxBackend 选择沙箱后端："" / "docker"（默认，与 CLI 一致）| "seatbelt" | "off"。
	SandboxBackend string

	// ---- 以下为测试接缝，生产代码留零值 ----

	// ProviderFactory 覆盖默认的 provider.NewFromEnv + GetModelLimits 装配（测试注入 mock）。
	// 返回值为 (provider, contextWindowTokens, error)。
	ProviderFactory func(model string) (provider.LLMProvider, int, error)
	// DBPath 覆盖会话数据库路径（测试隔离用）。
	DBPath string
	// ToolResultsDir / CompactionRecordsDir 覆盖对应目录（测试隔离用）。
	ToolResultsDir       string
	CompactionRecordsDir string
	// DisableEnvLoad 跳过 workDir/.env 加载（测试隔离用）。
	DisableEnvLoad bool
	// DisableObservability 跳过 OTEL 初始化（测试隔离用）。
	DisableObservability bool
}

// SessionInfoDTO 是会话元数据的可序列化视图（memory.SessionInfo 的镜像）。
type SessionInfoDTO struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	CreatedAt int64  `json:"created_at"` // Unix 秒
	UpdatedAt int64  `json:"updated_at"` // Unix 秒
	MsgCount  int    `json:"msg_count"`
}

// ModelInfoDTO 是当前模型元信息。
type ModelInfoDTO struct {
	Model         string `json:"model"`
	ContextWindow int    `json:"context_window"`
}

// Service 是面向桌面客户端的应用服务，持有跨会话共享栈与当前活跃会话栈。
type Service struct {
	cfg Config

	ctx    context.Context
	cancel context.CancelFunc

	// shared 是跨会话共享的装配产物（provider/sandbox/memory/ltm/mcp/hooks 公共件）。
	shared *sharedStack

	mu               sync.Mutex
	stack            *SessionStack      // 当前活跃会话栈；nil = 无活跃会话
	running          bool               // 是否有 agent run 在执行
	runCancel        context.CancelFunc // 当前 run 的取消函数；idle 时 nil
	closed           bool
	pendingApprovals map[string]*engine.ApprovalRequest
	approvalSeq      int64

	listener atomic.Value // listenerFn，可随时替换；nil 表示无监听者
}

// listenerFn 是事件监听回调的类型别名（供 atomic.Value 使用）。
type listenerFn func(Event)

// SetListener 注册事件监听者（桌面端把事件转发到 Wails EventsEmit）。
// 传 nil 注销。重复调用以最后一次为准。
func (s *Service) SetListener(fn func(Event)) {
	if fn == nil {
		s.listener.Store(listenerFn(nil))
		return
	}
	s.listener.Store(listenerFn(fn))
}

// emit 向监听者派发事件；recover 防御监听者 panic 拖垮事件泵。
func (s *Service) emit(evt Event) {
	v := s.listener.Load()
	if v == nil {
		return
	}
	fn, ok := v.(listenerFn)
	if !ok || fn == nil {
		return
	}
	defer func() { _ = recover() }()
	fn(evt)
}

// NewService 按序装配共享栈（env → observability → skills → provider → sandbox →
// memory → ltm → mcp），顺序与 cmd/harness9/main.go 保持一致。
// 调用方在拿到 Service 后需先 NewSession 或 OpenSession 才能发送消息。
func NewService(parent context.Context, cfg Config) (*Service, error) {
	if cfg.WorkDir == "" {
		return nil, fmt.Errorf("config: workDir 不能为空")
	}
	if !cfg.DisableEnvLoad {
		// internal/env 语义：系统环境变量优先，.env 只补缺；文件不存在不算错误。
		if err := env.Load(filepath.Join(cfg.WorkDir, ".env")); err != nil {
			return nil, fmt.Errorf("加载环境配置：%w", err)
		}
	}
	if cfg.HomeDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("获取 home 目录：%w", err)
		}
		cfg.HomeDir = home
	}
	if cfg.Model == "" {
		cfg.Model = os.Getenv("LLM_MODEL")
		if cfg.Model == "" {
			cfg.Model = "openai/gpt-4o-mini"
		}
	}

	ctx, cancel := context.WithCancel(parent)
	s := &Service{
		cfg:              cfg,
		ctx:              ctx,
		cancel:           cancel,
		pendingApprovals: make(map[string]*engine.ApprovalRequest),
	}
	s.listener.Store(listenerFn(nil))

	shared, err := s.buildSharedStack(ctx)
	if err != nil {
		cancel()
		return nil, err
	}
	s.shared = shared
	log.Print(logfmt.FormatMsg("appservice", fmt.Sprintf("服务装配完成 │ workDir=%s model=%s sandbox=%s",
		cfg.WorkDir, cfg.Model, shared.sandboxBackendLabel)))
	return s, nil
}

// ActiveSession 返回当前活跃会话的信息快照；无活跃会话时返回 ok=false。
func (s *Service) ActiveSession() (SessionInfoDTO, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stack == nil {
		return SessionInfoDTO{}, false
	}
	return s.stack.info, true
}

// IsRunning 返回是否有 agent run 在执行。
func (s *Service) IsRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// ListSessions 返回全部会话（updated_at 降序）。
func (s *Service) ListSessions() ([]SessionInfoDTO, error) {
	infos, err := s.shared.mgr.ListSessions(s.ctx)
	if err != nil {
		return nil, err
	}
	out := make([]SessionInfoDTO, 0, len(infos))
	for _, in := range infos {
		out = append(out, sessionInfoToDTO(in))
	}
	return out, nil
}

// NewSession 创建并激活一个新会话（原活跃会话保持持久化，随时可重新打开）。
func (s *Service) NewSession() (SessionInfoDTO, error) {
	if err := s.checkOpen(); err != nil {
		return SessionInfoDTO{}, err
	}
	sess, err := s.shared.mgr.NewSession(s.ctx)
	if err != nil {
		return SessionInfoDTO{}, err
	}
	if err := s.activateSession(sess); err != nil {
		return SessionInfoDTO{}, err
	}
	s.mu.Lock()
	info := s.stack.info
	s.mu.Unlock()
	log.Print(logfmt.FormatMsg("appservice", "已创建新会话 "+info.ID))
	return info, nil
}

// OpenSession 打开已有会话并激活。返回历史消息渲染所需的条目时间线与当前计划。
// 会话栈整体重建（engine/hooks/offload/compactor 均绑定新 sessionID），
// 因此打开即修复"切换会话后 offload 仍写旧 sessionID"的错位问题。
func (s *Service) OpenSession(id string) (SessionInfoDTO, []HistoryItem, []planning.PlanItem, error) {
	if err := s.checkOpen(); err != nil {
		return SessionInfoDTO{}, nil, nil, err
	}
	sess, err := s.shared.mgr.OpenSession(s.ctx, id)
	if err != nil {
		return SessionInfoDTO{}, nil, nil, err
	}
	if err := s.activateSession(sess); err != nil {
		return SessionInfoDTO{}, nil, nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	stack := s.stack
	msgs, err := sess.GetMessages(s.ctx, 0)
	if err != nil {
		return SessionInfoDTO{}, nil, nil, fmt.Errorf("读取会话历史：%w", err)
	}
	return stack.info, historyToItems(msgs), stack.planStore.Read(), nil
}

// DeleteSession 删除指定会话。若删除的是活跃会话，则同时清空活跃栈（不影响在跑任务语义：
// 删除活跃会话要求当前无在跑任务）。
func (s *Service) DeleteSession(id string) error {
	if err := s.checkOpen(); err != nil {
		return err
	}
	s.mu.Lock()
	if s.running && s.stack != nil && s.stack.session.SessionID() == id {
		s.mu.Unlock()
		return fmt.Errorf("会话正在执行任务，无法删除")
	}
	s.mu.Unlock()
	if err := s.shared.mgr.DeleteSession(s.ctx, id); err != nil {
		return err
	}
	s.mu.Lock()
	if s.stack != nil && s.stack.session.SessionID() == id {
		s.stack = nil
	}
	s.mu.Unlock()
	return nil
}

// RenameSession 设置会话标题（同时更新活跃栈缓存）。
func (s *Service) RenameSession(id, title string) error {
	if err := s.checkOpen(); err != nil {
		return err
	}
	if err := s.shared.mgr.SetTitle(s.ctx, id, title); err != nil {
		return err
	}
	s.mu.Lock()
	if s.stack != nil && s.stack.session.SessionID() == id {
		s.stack.info.Title = title
	}
	s.mu.Unlock()
	return nil
}

// SendMessage 以单飞语义启动一次 agent run：构建独立的可取消 ctx，
// 通过 RunStream 获取事件通道并交由后台 goroutine 泵送给监听者。
// prompt 中的斜杠命令展开（/skill 等）由调用方（桌面端）预处理。
func (s *Service) SendMessage(prompt string) error {
	if err := s.checkOpen(); err != nil {
		return err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrServiceClosed
	}
	if s.running {
		s.mu.Unlock()
		return ErrRunInProgress
	}
	stack := s.stack
	s.running = true
	runCtx, cancel := context.WithCancel(s.ctx)
	s.runCancel = cancel
	// 首条消息自动生成标题（首行截断），失败不影响发送。
	if stack.info.Title == "" {
		if title := deriveTitle(prompt); title != "" {
			if err := s.shared.mgr.SetTitle(s.ctx, stack.session.SessionID(), title); err == nil {
				stack.info.Title = title
			}
		}
	}
	s.mu.Unlock()

	ch, err := stack.engine.RunStream(runCtx, prompt)
	if err != nil {
		s.mu.Lock()
		s.running = false
		s.runCancel = nil
		s.mu.Unlock()
		cancel()
		s.emit(Event{Type: string(engine.EventError), Data: err.Error()})
		return fmt.Errorf("启动 agent run：%w", err)
	}
	go s.pump(stack, ch, cancel)
	return nil
}

// Interrupt 中断当前在跑的任务（等价 TUI 的 Ctrl+C：ctx 取消，部分轨迹不持久化）。
func (s *Service) Interrupt() {
	s.mu.Lock()
	cancel := s.runCancel
	s.mu.Unlock()
	if cancel != nil {
		log.Print(logfmt.FormatMsg("appservice", "收到中断请求，取消当前任务"))
		cancel()
	}
}

// Compact 手动触发强制压缩并广播压缩事件；返回压缩记录。
func (s *Service) Compact() (memory.CompactionRecord, error) {
	s.mu.Lock()
	stack := s.stack
	running := s.running
	s.mu.Unlock()
	if stack == nil {
		return memory.CompactionRecord{}, ErrNoActiveSession
	}
	if running {
		return memory.CompactionRecord{}, ErrRunInProgress
	}
	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Minute)
	defer cancel()
	rec, err := stack.engine.Compact(ctx)
	if err != nil {
		return memory.CompactionRecord{}, err
	}
	s.emit(Event{Type: string(engine.EventCompaction), Data: rec})
	return rec, nil
}

// GetCurrentPlan 返回活跃会话的当前计划（无活跃会话时返回 nil）。
func (s *Service) GetCurrentPlan() []planning.PlanItem {
	s.mu.Lock()
	stack := s.stack
	s.mu.Unlock()
	if stack == nil {
		return nil
	}
	return stack.planStore.Read()
}

// GetModelInfo 返回当前模型与上下文窗口。
func (s *Service) GetModelInfo() ModelInfoDTO {
	return ModelInfoDTO{Model: s.cfg.Model, ContextWindow: s.shared.modelWindow}
}

// GetSandboxStatus 返回沙箱后端的当前状态快照（seatbelt 为静态快照，docker 查询 Manager）。
func (s *Service) GetSandboxStatus() []sandbox.SandboxInfo {
	return s.shared.sandboxStatus()
}

// SandboxBackendLabel 返回沙箱后端标签（"docker" / "seatbelt" / "off" / "docker-degraded"）。
func (s *Service) SandboxBackendLabel() string { return s.shared.sandboxBackendLabel }

// SandboxDegradedReason 返回 docker 沙箱启动失败降级的原因（空串表示未降级）。
func (s *Service) SandboxDegradedReason() string { return s.shared.sandboxDegradeReason }

// SkillsNames 返回已加载的技能名（供前端斜杠补全）。
func (s *Service) SkillsNames() []string { return s.shared.skillsIndex.Names() }

// GetSkillContent 按名称取技能全文（前端展开 /skill 命令用）；不存在返回空串。
func (s *Service) GetSkillContent(name string) string {
	content, err := s.shared.skillsIndex.GetFullContent(name)
	if err != nil {
		return ""
	}
	return content
}

// Close 释放全部资源：取消生命周期 ctx、停 MCP、销毁 docker 沙箱、关数据库。
// 幂等。
func (s *Service) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	// 中断在跑任务
	if s.runCancel != nil {
		s.runCancel()
	}
	s.mu.Unlock()

	s.cancel()
	if s.shared != nil {
		s.shared.close(s.ctx)
	}
	log.Print(logfmt.FormatMsg("appservice", "服务已关闭"))
}

// checkOpen 是公开方法的公共前置校验。
func (s *Service) checkOpen() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrServiceClosed
	}
	return nil
}

// activateSession 构建 SessionStack 并切换为活跃（须在持有 s.mu 之外调用；
// 内部自行加锁）。构建失败时保持原活跃栈不变。
func (s *Service) activateSession(sess memory.Session) error {
	stack, err := s.buildSessionStack(sess)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.stack = stack
	s.mu.Unlock()
	return nil
}

// sessionInfoToDTO 把 memory.SessionInfo 转为 DTO。
func sessionInfoToDTO(in memory.SessionInfo) SessionInfoDTO {
	return SessionInfoDTO{
		ID:        in.ID,
		Title:     in.Title,
		CreatedAt: in.CreatedAt.Unix(),
		UpdatedAt: in.UpdatedAt.Unix(),
		MsgCount:  in.MsgCount,
	}
}

// deriveTitle 从首条用户消息生成会话标题：取第一行、去掉 Markdown 前缀符号、截断 30 字符。
func deriveTitle(prompt string) string {
	const maxRunes = 30
	// 取第一行
	for i, r := range prompt {
		if r == '\n' || r == '\r' {
			prompt = prompt[:i]
			break
		}
	}
	runes := []rune(prompt)
	if len(runes) > maxRunes {
		runes = runes[:maxRunes]
	}
	return string(runes)
}
