package main

import (
	"context"
	"fmt"
	"log"
	"sync"

	"github.com/harness9/internal/appservice"
	"github.com/harness9/internal/engine"
	"github.com/harness9/internal/memory"
	"github.com/harness9/internal/planning"
	"github.com/harness9/internal/sandbox"
	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// App 是暴露给前端的绑定对象（Wails Bind）。所有方法都是对 appservice.Service
// 的薄转发，不承载业务逻辑；事件经 agent:event 通道推送给前端。
type App struct {
	homeDir  string
	version  string
	ctx      context.Context
	mu       sync.Mutex
	svc      *appservice.Service
	workDir  string // 当前 Service 对应的 workDir
	settings appservice.Settings
}

// NewApp 构造绑定对象。
func NewApp(homeDir, version string) *App {
	return &App{homeDir: homeDir, version: version}
}

// startup 是 Wails 生命周期回调：保存运行时 ctx，加载持久化设置。
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	if st, err := appservice.LoadSettings(a.homeDir); err == nil {
		a.settings = st
	} else {
		log.Printf("desktop: 加载设置失败（使用默认值）: %v", err)
	}
}

// beforeClose 在任务运行中弹出确认框，防止误关窗口丢任务。
func (a *App) beforeClose(ctx context.Context) bool {
	if a.svc == nil || !a.svc.IsRunning() {
		return true
	}
	choice, err := wailsruntime.MessageDialog(ctx, wailsruntime.MessageDialogOptions{
		Type:          wailsruntime.QuestionDialog,
		Title:         "任务正在运行",
		Message:       "Agent 任务尚未结束，退出将中断当前任务。确定退出吗？",
		Buttons:       []string{"退出", "取消"},
		DefaultButton: "取消",
		CancelButton:  "取消",
	})
	if err != nil {
		// 对话框异常时宁可阻止关闭，避免误中断
		return false
	}
	return choice == "退出"
}

// shutdown 释放服务层资源。
func (a *App) shutdown(ctx context.Context) {
	a.mu.Lock()
	svc := a.svc
	a.mu.Unlock()
	if svc != nil {
		svc.Close()
	}
}

// InitResult 是 Initialize 的返回视图：前端冷启动所需的全量状态。
type InitResult struct {
	WorkDir         string                      `json:"work_dir"`
	Version         string                      `json:"version"`
	ActiveSession   appservice.SessionInfoDTO   `json:"active_session"`
	Sessions        []appservice.SessionInfoDTO `json:"sessions"`
	Items           []appservice.HistoryItem    `json:"items"`
	Plan            []planning.PlanItem         `json:"plan"`
	ModelInfo       appservice.ModelInfoDTO     `json:"model_info"`
	SandboxLabel    string                      `json:"sandbox_label"`
	SandboxStatus   []sandbox.SandboxInfo       `json:"sandbox_status"`
	DegradedReason  string                      `json:"degraded_reason,omitempty"`
	Skills          []string                    `json:"skills"`
	Settings        appservice.Settings         `json:"settings"`
	PermissionModes []PermissionModeOption      `json:"permission_modes"`
}

// PermissionModeOption 是权限模式选择器的选项。
type PermissionModeOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
	Desc  string `json:"desc"`
}

// permissionModeOptions 与 internal/engine 的 PermissionMode 枚举对齐。
func permissionModeOptions() []PermissionModeOption {
	return []PermissionModeOption{
		{Value: "default", Label: "默认", Desc: "危险操作弹窗审批，其余放行"},
		{Value: "auto_approve", Label: "自动批准", Desc: "跳过全部审批弹窗"},
		{Value: "read_only", Label: "只读", Desc: "拒绝一切写操作"},
		{Value: "bypass_all", Label: "完全信任", Desc: "不沙箱拦截、不审批"},
	}
}

// Initialize 装载指定工作目录（切换目录时重建 Service），创建新会话并返回全量状态。
func (a *App) Initialize(workDir string) (InitResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if workDir == "" {
		return InitResult{}, fmt.Errorf("workDir 不能为空")
	}

	// workDir 变更 → 释放旧 Service 后重建
	if a.svc != nil && a.workDir != workDir {
		a.svc.Close()
		a.svc = nil
	}

	if a.svc == nil {
		svc, err := appservice.NewService(context.Background(), appservice.Config{
			WorkDir:        workDir,
			HomeDir:        a.homeDir,
			Model:          a.settings.Model,
			PermissionMode: permissionModeFromSetting(a.settings.PermissionMode),
			SandboxBackend: backendFromSetting(a.settings.SandboxBackend),
		})
		if err != nil {
			return InitResult{}, err
		}
		a.svc = svc
		a.workDir = workDir
		// 服务事件 → Wails 事件桥
		svc.SetListener(func(evt appservice.Event) {
			wailsruntime.EventsEmit(a.ctx, "agent:event", evt)
		})
		svc.AddRecentWorkDir(workDir)
	}

	if _, err := a.svc.NewSession(); err != nil {
		return InitResult{}, err
	}
	return a.snapshotLocked()
}

// snapshotLocked 组装 InitResult（调用方须持有 a.mu）。
func (a *App) snapshotLocked() (InitResult, error) {
	svc := a.svc
	sessions, err := svc.ListSessions()
	if err != nil {
		return InitResult{}, err
	}
	active, _ := svc.ActiveSession()
	info, items, plan, err := svc.OpenSession(active.ID)
	if err != nil {
		return InitResult{}, err
	}
	_ = info
	return InitResult{
		WorkDir:         a.workDir,
		Version:         a.version,
		ActiveSession:   active,
		Sessions:        sessions,
		Items:           items,
		Plan:            plan,
		ModelInfo:       svc.GetModelInfo(),
		SandboxLabel:    svc.SandboxBackendLabel(),
		SandboxStatus:   svc.GetSandboxStatus(),
		DegradedReason:  svc.SandboxDegradedReason(),
		Skills:          svc.SkillsNames(),
		Settings:        svc.GetSettings(),
		PermissionModes: permissionModeOptions(),
	}, nil
}

// PickWorkDir 弹出原生目录选择框。
func (a *App) PickWorkDir() (string, error) {
	return wailsruntime.OpenDirectoryDialog(a.ctx, wailsruntime.OpenDialogOptions{
		Title: "选择工作目录（Agent 的沙箱根目录）",
	})
}

// RecentWorkDirs 返回最近打开的工作目录。
func (a *App) RecentWorkDirs() ([]string, error) {
	st, err := appservice.LoadSettings(a.homeDir)
	if err != nil {
		return nil, err
	}
	return st.RecentWorkDirs, nil
}

// ListSessions 返回全部会话。
func (a *App) ListSessions() ([]appservice.SessionInfoDTO, error) {
	svc, err := a.service()
	if err != nil {
		return nil, err
	}
	return svc.ListSessions()
}

// NewSession 创建并切换到新会话，返回刷新后的会话列表与活跃会话。
func (a *App) NewSession() ([]appservice.SessionInfoDTO, appservice.SessionInfoDTO, error) {
	svc, err := a.service()
	if err != nil {
		return nil, appservice.SessionInfoDTO{}, err
	}
	info, err := svc.NewSession()
	if err != nil {
		return nil, appservice.SessionInfoDTO{}, err
	}
	sessions, err := svc.ListSessions()
	if err != nil {
		return nil, info, err
	}
	return sessions, info, nil
}

// OpenSession 打开历史会话，返回时间线条目与当前计划。
func (a *App) OpenSession(id string) (appservice.SessionInfoDTO, []appservice.HistoryItem, []planning.PlanItem, error) {
	svc, err := a.service()
	if err != nil {
		return appservice.SessionInfoDTO{}, nil, nil, err
	}
	return svc.OpenSession(id)
}

// DeleteSession 删除会话并返回刷新后的列表。
func (a *App) DeleteSession(id string) ([]appservice.SessionInfoDTO, error) {
	svc, err := a.service()
	if err != nil {
		return nil, err
	}
	if err := svc.DeleteSession(id); err != nil {
		return nil, err
	}
	return svc.ListSessions()
}

// RenameSession 重命名会话。
func (a *App) RenameSession(id, title string) error {
	svc, err := a.service()
	if err != nil {
		return err
	}
	return svc.RenameSession(id, title)
}

// SendMessage 发送用户消息，启动 agent run（事件经 agent:event 推送）。
func (a *App) SendMessage(prompt string) error {
	svc, err := a.service()
	if err != nil {
		return err
	}
	return svc.SendMessage(prompt)
}

// Interrupt 中断当前任务。
func (a *App) Interrupt() {
	if svc, err := a.service(); err == nil {
		svc.Interrupt()
	}
}

// IsRunning 返回任务运行状态。
func (a *App) IsRunning() bool {
	svc, err := a.service()
	return err == nil && svc.IsRunning()
}

// Compact 手动强制压缩，返回压缩记录。
func (a *App) Compact() (memory.CompactionRecord, error) {
	svc, err := a.service()
	if err != nil {
		return memory.CompactionRecord{}, err
	}
	return svc.Compact()
}

// RespondApproval 应答审批请求。
func (a *App) RespondApproval(id string, approved bool, feedback string, remember bool) error {
	svc, err := a.service()
	if err != nil {
		return err
	}
	return svc.RespondApproval(id, approved, feedback, remember)
}

// PendingApprovals 返回待审批列表（前端重连恢复用）。
func (a *App) PendingApprovals() ([]appservice.ApprovalData, error) {
	svc, err := a.service()
	if err != nil {
		return nil, err
	}
	return svc.PendingApprovals(), nil
}

// GetCurrentPlan 返回当前计划。
func (a *App) GetCurrentPlan() ([]planning.PlanItem, error) {
	svc, err := a.service()
	if err != nil {
		return nil, err
	}
	return svc.GetCurrentPlan(), nil
}

// GetSessionDiff 返回工作目录 git 变更。
func (a *App) GetSessionDiff() (appservice.DiffResult, error) {
	svc, err := a.service()
	if err != nil {
		return appservice.DiffResult{}, err
	}
	return svc.GetSessionDiff()
}

// GetModelInfo 返回模型元信息。
func (a *App) GetModelInfo() (appservice.ModelInfoDTO, error) {
	svc, err := a.service()
	if err != nil {
		return appservice.ModelInfoDTO{}, err
	}
	return svc.GetModelInfo(), nil
}

// GetSandboxStatus 返回沙箱状态快照。
func (a *App) GetSandboxStatus() ([]sandbox.SandboxInfo, error) {
	svc, err := a.service()
	if err != nil {
		return nil, err
	}
	return svc.GetSandboxStatus(), nil
}

// GetSkills 返回技能名列表。
func (a *App) GetSkills() ([]string, error) {
	svc, err := a.service()
	if err != nil {
		return nil, err
	}
	return svc.SkillsNames(), nil
}

// GetSkillContent 返回技能全文。
func (a *App) GetSkillContent(name string) (string, error) {
	svc, err := a.service()
	if err != nil {
		return "", err
	}
	return svc.GetSkillContent(name), nil
}

// SaveSettings 持久化设置并返回重启生效提示（空串表示即时生效）。
func (a *App) SaveSettings(model, permissionMode, sandboxBackend string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.svc == nil {
		// 服务未创建（欢迎页阶段）：仅落盘
		a.settings.Model = model
		a.settings.PermissionMode = permissionMode
		a.settings.SandboxBackend = sandboxBackend
		return "", appservice.SaveSettings(a.homeDir, a.settings)
	}
	st := a.svc.GetSettings()
	st.Model = model
	st.PermissionMode = permissionMode
	st.SandboxBackend = sandboxBackend
	hint, err := a.svc.SaveSettings(st)
	if err == nil {
		a.settings = st
	}
	return hint, err
}

// service 返回当前 Service；未初始化（前端未走 Initialize 流程）时返回错误。
func (a *App) service() (*appservice.Service, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.svc == nil {
		return nil, fmt.Errorf("尚未初始化：请先选择工作目录")
	}
	return a.svc, nil
}

// permissionModeFromSetting / backendFromSetting 把设置字符串转为配置值
// （非法值静默回退默认，保证冷启动可用）。
func permissionModeFromSetting(s string) engine.PermissionMode {
	switch s {
	case "auto_approve":
		return engine.PermissionModeAutoApprove
	case "read_only":
		return engine.PermissionModeReadOnly
	case "bypass_all":
		return engine.PermissionModeBypassAll
	default:
		return engine.PermissionModeDefault
	}
}

func backendFromSetting(s string) string {
	switch s {
	case "seatbelt", "off":
		return s
	default:
		return "docker"
	}
}
