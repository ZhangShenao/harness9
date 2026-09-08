//go:build !darwin

// internal/sandbox — daemon_other.go
// 非 macOS 平台的 Docker daemon 拉起实现。
package sandbox

// tryStartDaemon 非 darwin 平台不自动拉起 daemon：
// Linux 下 docker 通常由 systemd 管理，自动启动需要 root 权限，静默提权不可接受。
// 返回 false → ensureDaemonReady 仅做短等待（覆盖 daemon 正在重启的窗口）后快速失败。
func tryStartDaemon() bool { return false }
