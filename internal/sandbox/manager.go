// Package sandbox 提供基于 Docker 的沙箱容器生命周期管理。
package sandbox

import (
	"context"
	"crypto/rand"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/harness9/internal/logfmt"
)

// SandboxInfo 是 Sandbox 状态的只读快照，用于 TUI 展示（ListAll 返回）。
type SandboxInfo struct {
	ID       string // Sandbox UUID
	DockerID string // Docker container ID（前 12 位短 hash）
	State    ContainerState
	WorkDir  string
	Image    string
}

// Manager 管理所有活跃 Sandbox 的生命周期，是 Sandbox 系统的单一事实源。
// 线程安全：所有公开方法均可并发调用。
type Manager struct {
	containers map[string]*Container
	mu         sync.RWMutex
	cfg        SandboxConfig
	onUpdate   func([]SandboxInfo)
	// runnerFactory 用于测试注入 mock runner；nil 时使用 realCmdRunner。
	runnerFactory func(id, workDir string, cfg SandboxConfig) cmdRunner
}

// NewManager 创建 Sandbox 管理器。
func NewManager(cfg SandboxConfig) *Manager {
	return &Manager{
		containers: make(map[string]*Container),
		cfg:        cfg,
	}
}

// WithUpdateNotify 设置状态变更通知回调，供 TUI 通过 channel 接收更新。
// 必须在首次 Create 调用之前设置。内部用 m.mu 保护，并发安全。
func (m *Manager) WithUpdateNotify(fn func([]SandboxInfo)) {
	m.mu.Lock()
	m.onUpdate = fn
	m.mu.Unlock()
}

// Create 为一个 Agent 创建独立 Sandbox，返回可用的 Environment。
func (m *Manager) Create(ctx context.Context, workDir string) (Environment, error) {
	id := generateID()

	run := cmdRunner(realCmdRunner)
	if m.runnerFactory != nil {
		run = m.runnerFactory(id, workDir, m.cfg)
	}

	c := newContainer(id, workDir, m.cfg, run)
	if err := c.Start(ctx); err != nil {
		return nil, fmt.Errorf("sandbox: 启动容器失败：%w", err)
	}

	env := newDockerEnvironment(c.DockerID(), id, workDir, run)

	// 依赖 bootstrap：容器就绪后、Agent 开始前执行一次初始化命令（如安装项目依赖），
	// 在独立的较长预算内运行，不受单条 bash 命令超时约束。fail-open：失败仅告警、不阻断创建
	// （Agent 仍可静态分析；接入官方预装镜像后此步通常无需执行）。
	if cmd := strings.TrimSpace(m.cfg.BootstrapCmd); cmd != "" {
		m.runBootstrap(env, cmd, workDir)
	}

	m.mu.Lock()
	m.containers[id] = c
	m.mu.Unlock()

	m.notify()
	return env, nil
}

// runBootstrap 在独立的 BootstrapTimeout 预算内执行一次性初始化命令（fail-open）。
func (m *Manager) runBootstrap(env Environment, cmd, workDir string) {
	timeout := m.cfg.BootstrapTimeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	log.Print(logfmt.FormatMsg("sandbox", fmt.Sprintf("执行 bootstrap（超时 %s）: %s", timeout, cmd)))
	bootCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	// DockerEnvironment.RunBash 始终返回 err==nil（错误转为输出字符串），故仅作记录。
	if _, err := env.RunBash(bootCtx, cmd, workDir); err != nil {
		log.Print(logfmt.FormatMsg("sandbox", fmt.Sprintf("bootstrap 出错（fail-open，继续）: %v", err)))
	}
}

// Destroy 停止并移除指定 Sandbox。ID 不存在时静默返回 nil。
func (m *Manager) Destroy(ctx context.Context, id string) error {
	m.mu.Lock()
	c, ok := m.containers[id]
	if ok {
		delete(m.containers, id)
	}
	m.mu.Unlock()

	if !ok {
		return nil
	}
	err := c.Stop(ctx)
	m.notify()
	return err
}

// DestroyAll 并发停止所有活跃 Sandbox，程序退出时调用（defer）。
func (m *Manager) DestroyAll(ctx context.Context) {
	m.mu.Lock()
	cs := make([]*Container, 0, len(m.containers))
	for _, c := range m.containers {
		cs = append(cs, c)
	}
	m.containers = make(map[string]*Container)
	m.mu.Unlock()

	var wg sync.WaitGroup
	for _, c := range cs {
		wg.Add(1)
		c := c
		go func() {
			defer wg.Done()
			_ = c.Stop(ctx)
		}()
	}
	wg.Wait()
	m.notify()
}

// ReapOrphans 清理上次进程崩溃遗留的孤儿容器（label=harness9=1，全部状态）。
//
// 原实现只清理 status=exited 的容器；进程被 SIGKILL 强杀时 defer 不运行，
// 容器会以 Running 状态残留——持有已删除 tmpDir 的 bind mount，
// 在 macOS Docker Desktop 上会导致 VirtioFS 慢，使后续容器启动超时。
// 修正为清理所有 harness9 标记的容器（无论状态），使用 rm -f 强制停止并删除。
//
// 安全性：跳过当前 Manager 实例自身持有的容器（通过 shortDockerID 比对），
// 避免多个进程并发运行时误杀彼此的活跃容器。
func (m *Manager) ReapOrphans(ctx context.Context) error {
	out, err := realCmdRunner(ctx,
		"ps", "-a",
		"--filter", "label=harness9=1",
		"--format", "{{.ID}}",
	)
	if err != nil {
		return fmt.Errorf("sandbox: 列出孤儿容器失败：%w", err)
	}
	ids := strings.Fields(out)
	if len(ids) == 0 {
		return nil
	}

	// 收集当前 Manager 持有的容器短 ID，跳过这些容器避免误杀。
	// docker ps 返回 12 位短 ID；shortDockerID 取 dockerID 前 12 位做比对。
	m.mu.RLock()
	owned := make(map[string]bool, len(m.containers))
	for _, c := range m.containers {
		c.mu.RLock()
		if c.dockerID != "" {
			owned[shortDockerID(c.dockerID)] = true
		}
		c.mu.RUnlock()
	}
	m.mu.RUnlock()

	var toClean []string
	for _, id := range ids {
		if !owned[id] {
			toClean = append(toClean, id)
		}
	}
	if len(toClean) == 0 {
		return nil
	}
	log.Print(logfmt.FormatMsg("sandbox", fmt.Sprintf("发现 %d 个孤儿容器，正在清理...", len(toClean))))
	for _, id := range toClean {
		if _, err := realCmdRunner(ctx, "rm", "-f", id); err != nil {
			log.Print(logfmt.FormatMsg("sandbox", fmt.Sprintf("清理孤儿容器 %s 失败: %v", id, err)))
		}
	}
	return nil
}

// ListAll 返回所有活跃 Sandbox 的只读快照（线程安全）。
func (m *Manager) ListAll() []SandboxInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()

	infos := make([]SandboxInfo, 0, len(m.containers))
	// 锁序：Manager.mu（读）→ Container.mu（读），严禁逆序以防死锁。
	for _, c := range m.containers {
		c.mu.RLock()
		infos = append(infos, SandboxInfo{
			ID:       c.id,
			DockerID: shortDockerID(c.dockerID),
			State:    c.state,
			WorkDir:  c.workDir,
			Image:    c.cfg.Image,
		})
		c.mu.RUnlock()
	}
	return infos
}

func (m *Manager) notify() {
	// 先持读锁取出 fn，再释放锁后调用，避免 fn 内部再次获取写锁时产生死锁。
	m.mu.RLock()
	fn := m.onUpdate
	m.mu.RUnlock()
	if fn != nil {
		fn(m.ListAll())
	}
}

// generateID 生成 16 位 hex 格式的随机 Sandbox UUID（crypto/rand，无冲突）。
func generateID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("sandbox: crypto/rand 读取失败: %v", err))
	}
	return fmt.Sprintf("%x", b)
}

// shortDockerID 截取 Docker container ID 前 12 位供展示使用。
func shortDockerID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
