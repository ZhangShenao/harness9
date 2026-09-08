//go:build darwin

// internal/sandbox — daemon_darwin.go
// macOS 平台的 Docker daemon 拉起实现。
package sandbox

import "os/exec"

// tryStartDaemon 尝试拉起 Docker Desktop（macOS）。
// open 是幂等的：Docker Desktop 已启动或正在启动时立即成功，无副作用。
// 返回 false 表示拉起未成功（典型：未安装 Docker Desktop），
// 调用方据此采用短等待快速失败，避免未安装 Docker 的用户在启动时白等。
func tryStartDaemon() bool {
	return exec.Command("open", "-a", "Docker").Run() == nil
}
