// internal/sandbox — daemon.go
// Docker daemon 可用性保障：启动 Sandbox 前确保 daemon 就绪。
//
// 背景：Sandbox 启用（默认启用）时，若 daemon 短暂不可用（Docker Desktop 尚未启动/
// 正在重启、冷启动中），单次 Create 失败会导致整个会话静默降级为本地执行——Agent 与
// 用户都难以察觉。ensureDaemonReady 通过"探测 → 自动拉起 → 有界轮询"消除这类瞬时故障。
package sandbox

import (
	"context"
	"fmt"
	"time"
)

// daemon 就绪等待参数。声明为变量以便单元测试缩短真实等待时间。
var (
	// daemonProbeInterval 是轮询 docker info 的间隔。
	daemonProbeInterval = 500 * time.Millisecond
	// daemonRestartGrace 是不尝试拉起时的等待上限（覆盖 daemon 正在重启的窗口）。
	daemonRestartGrace = 2 * time.Second
	// daemonStartTimeout 是自动拉起成功后的等待上限（Docker Desktop 冷启动较慢）。
	daemonStartTimeout = 90 * time.Second
)

// startDaemonHook 允许单元测试替换平台相关的 daemon 拉起实现。
var startDaemonHook = tryStartDaemon

// ensureDaemonReady 确保 Docker daemon 可用，返回 nil 后可安全执行 docker run。
//
// 流程：
//  1. docker info 探测，就绪则立即返回；
//  2. 探测失败 → 尝试拉起 daemon（见 tryStartDaemon，平台相关）；
//  3. 有界轮询至就绪：拉起成功给长上限（daemon 冷启动慢），
//     拉起失败给短上限（大概率未安装，避免用户启动时白等）。
//
// ctx 取消（如启动时 Ctrl+C）会立即中断等待。
func ensureDaemonReady(ctx context.Context, run cmdRunner) error {
	probe := func() error {
		_, err := run(ctx, "info", "--format", "{{.ServerVersion}}")
		return err
	}
	if probe() == nil {
		return nil
	}

	started := startDaemonHook()
	maxWait := daemonRestartGrace
	if started {
		maxWait = daemonStartTimeout
	}

	deadline := time.Now().Add(maxWait)
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("等待 docker daemon 就绪时被取消: %w", ctx.Err())
		case <-time.After(daemonProbeInterval):
		}
		if probe() == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("docker daemon 在 %v 内仍不可用（自动拉起=%v）", maxWait, started)
		}
	}
}
