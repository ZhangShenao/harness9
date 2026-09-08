// Package sandbox 提供基于 Docker 的沙箱容器生命周期管理。
//
// 设计原则：
//   - cmdRunner 函数类型注入，解耦真实 docker CLI 与单元测试
//   - 五状态机保证状态转换的线程安全性
//   - Start 使用轮询等待容器真正就绪，而非一次性检查
//   - Stop 无论 docker stop 结果如何，都尝试 docker rm 回收资源
package sandbox

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// ContainerState 表示 Sandbox 容器的当前生命周期状态。
type ContainerState int

const (
	StatePending    ContainerState = iota // 容器创建中，等待就绪
	StateRunning                          // 容器正常运行，接受工具调用
	StateStopping                         // 容器停止中（docker stop 已发出）
	StateTerminated                       // 容器已停止并移除
	StateFailed                           // 发生不可恢复错误
)

// String 返回状态的可读名称，供 TUI 展示。
func (s ContainerState) String() string {
	switch s {
	case StatePending:
		return "Pending"
	case StateRunning:
		return "Running"
	case StateStopping:
		return "Stopping"
	case StateTerminated:
		return "Terminated"
	case StateFailed:
		return "Failed"
	default:
		return "Unknown"
	}
}

// cmdRunner 是可注入的 docker 命令执行函数，便于单元测试 mock 替换真实 docker CLI。
type cmdRunner func(ctx context.Context, args ...string) (string, error)

// realCmdRunner 调用真实的 docker CLI，返回 stdout+stderr 合并输出（去首尾空白）。
func realCmdRunner(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// Container 管理单个 Docker 容器的完整生命周期：创建→运行→停止→回收。
type Container struct {
	id       string // Sandbox UUID（由 Manager 生成）
	dockerID string // docker container ID（docker run 返回的完整 ID）
	state    ContainerState
	mu       sync.RWMutex
	cfg      SandboxConfig
	workDir  string
	run      cmdRunner
	err      error // StateFailed 时记录原因
}

// newContainer 构造处于 Pending 状态的容器实例，run 可注入 mock 便于测试。
func newContainer(id, workDir string, cfg SandboxConfig, run cmdRunner) *Container {
	return &Container{
		id:      id,
		state:   StatePending,
		workDir: workDir,
		cfg:     cfg,
		run:     run,
	}
}

// ID 返回 Sandbox UUID。
func (c *Container) ID() string { return c.id }

// DockerID 返回 Docker container ID（线程安全）。
func (c *Container) DockerID() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.dockerID
}

// State 返回当前容器状态（线程安全）。
func (c *Container) State() ContainerState {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.state
}

// Err 返回 Failed 状态的错误原因（其他状态返回 nil）。
func (c *Container) Err() error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.err
}

// setState 线程安全地更新状态与错误原因。
func (c *Container) setState(s ContainerState, err error) {
	c.mu.Lock()
	c.state = s
	c.err = err
	c.mu.Unlock()
}

// Start 启动容器并等待就绪：docker run → 轮询 docker inspect → StateRunning。
//
// 若 docker run 失败立即转 StateFailed；若轮询超时（StartTimeout）也转 StateFailed。
func (c *Container) Start(ctx context.Context) error {
	startCtx, cancel := context.WithTimeout(ctx, c.cfg.StartTimeout)
	defer cancel()

	dockerID, err := c.run(startCtx,
		"run", "-d",
		"--name", "harness9-"+c.id,
		"--label", "harness9=1",
		"--cap-drop", "all",
		"--cap-add", "DAC_OVERRIDE",
		"--cap-add", "SETUID",
		"--cap-add", "SETGID",
		"--security-opt", "no-new-privileges:true",
		"--pids-limit", fmt.Sprintf("%d", c.cfg.PidsLimit),
		"--cpus", c.cfg.CPUs,
		"--memory", c.cfg.Memory,
		"--tmpfs", "/tmp:size=256m,nosuid,noexec,nodev",
		// -v 语法不依赖逗号分隔，对含逗号的路径安全；等价于 --mount type=bind。
		"-v", fmt.Sprintf("%s:%s", c.workDir, c.workDir),
		c.cfg.Image,
		"sleep", "infinity",
	)
	if err != nil {
		// dockerID 此处实际为 CombinedOutput，包含 docker 的错误信息
		c.setState(StateFailed, fmt.Errorf("docker run 失败: %w，输出: %s", err, dockerID))
		return c.err
	}
	c.mu.Lock()
	c.dockerID = dockerID
	c.mu.Unlock()

	// 轮询等待容器 Running=true，超时则转 Failed
	for {
		out, inspectErr := c.run(startCtx, "inspect", "--format={{.State.Running}}", dockerID)
		if inspectErr == nil && out == "true" {
			break
		}
		select {
		case <-startCtx.Done():
			c.setState(StateFailed, fmt.Errorf("等待容器就绪超时（%v）", c.cfg.StartTimeout))
			// docker run 已成功，容器此刻真实存在——不清理会以 Running 状态泄漏到下次
			// 会话才被 ReapOrphans 回收，期间持有 bind mount 拖慢 macOS Docker Desktop。
			// startCtx 已过期，须用独立短超时执行清理（fail-soft：失败不影响错误返回）。
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cleanupCancel()
			_, _ = c.run(cleanupCtx, "rm", "-f", dockerID)
			return c.err
		case <-time.After(200 * time.Millisecond):
		}
	}

	c.setState(StateRunning, nil)
	return nil
}

// Stop 停止并移除容器：docker stop（带宽限期）→ docker rm → StateTerminated。
//
// docker stop 失败不视为致命错误，仍继续执行 docker rm 以确保资源回收。
// 已处于 Terminated 或 Failed 状态的容器调用 Stop 直接返回 nil（重入守卫）。
func (c *Container) Stop(ctx context.Context) error {
	// 已终止或失败的容器无需重复停止
	if s := c.State(); s == StateTerminated || s == StateFailed {
		return nil
	}
	c.setState(StateStopping, nil)

	stopCtx, cancel := context.WithTimeout(ctx, c.cfg.StopTimeout)
	defer cancel()

	dockerID := c.DockerID()
	// docker stop 宽限期内发 SIGTERM，超时后 SIGKILL
	_, _ = c.run(stopCtx, "stop", "-t", "5", dockerID)
	// docker rm 无论 stop 是否成功都尝试
	_, _ = c.run(stopCtx, "rm", dockerID)

	c.setState(StateTerminated, nil)
	return nil
}
