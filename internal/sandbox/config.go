// Package sandbox 提供 Docker 容器级执行环境抽象，
// 为 Agent 工具调用提供 OS 级隔离（网络、进程空间、文件系统）。
package sandbox

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// SandboxConfig 是 Sandbox 系统的全局配置。
// 通过 DefaultConfig 从环境变量读取，支持 .env 文件覆盖（internal/env 先行加载）。
type SandboxConfig struct {
	// Enabled 是否启用 Docker Sandbox。false 时工具使用本地进程执行，行为与原始版本完全一致。
	Enabled bool
	// Image Docker 镜像名称。
	Image string
	// CPUs 容器可用 CPU 核数（docker --cpus 参数）。
	CPUs string
	// Memory 容器内存上限（docker --memory 参数）。
	Memory string
	// PidsLimit 容器进程数上限，防 fork bomb（docker --pids-limit 参数）。
	PidsLimit int
	// StartTimeout 等待容器就绪的超时时间。
	StartTimeout time.Duration
	// StopTimeout docker stop 的宽限期，超时后 SIGKILL。
	StopTimeout time.Duration
	// BootstrapCmd 是容器就绪后、Agent 开始前执行一次的初始化命令（在独立的、较长的
	// BootstrapTimeout 预算内运行，不受单条 bash 命令超时约束）。空字符串表示不执行。
	// 典型用途：为 SWE-bench 仓库安装依赖（如 `pip install -e . -q`），或激活预置 conda 环境。
	// 这是接入官方 SWE-bench 每实例镜像（仓库已预装）的关键接缝——届时只需配置镜像 + 此命令。
	BootstrapCmd string
	// BootstrapTimeout 是 BootstrapCmd 的超时（默认 600s）。
	BootstrapTimeout time.Duration
}

// DefaultConfig 从环境变量读取配置，未设置时使用内置安全默认值。
func DefaultConfig() SandboxConfig {
	return SandboxConfig{
		Enabled:   strings.ToLower(os.Getenv("SANDBOX_ENABLED")) != "false",
		Image:     getenvOr("SANDBOX_IMAGE", "ubuntu:22.04"),
		CPUs:      getenvOr("SANDBOX_CPUS", "1.0"),
		Memory:    getenvOr("SANDBOX_MEMORY", "512m"),
		PidsLimit: 256,
		// StartTimeout 默认 60s：macOS Docker Desktop 冷启动/镜像首次挂载（VirtioFS）较慢，
		// 30s 偶发超时导致会话启动即降级；配合 Manager.CreateWithRetry 的重试兜底。
		StartTimeout:     60 * time.Second,
		StopTimeout:      10 * time.Second,
		BootstrapCmd:     os.Getenv("SANDBOX_BOOTSTRAP_CMD"),
		BootstrapTimeout: time.Duration(getenvIntOr("SANDBOX_BOOTSTRAP_TIMEOUT_SECS", 600)) * time.Second,
	}
}

// getenvOr 从环境变量读取 key，未设置或为空时返回 fallback。
func getenvOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// getenvIntOr 从环境变量读取整型 key，未设置/非法时返回 fallback。
func getenvIntOr(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return fallback
}
