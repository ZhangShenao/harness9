// internal/sandbox — daemon_test.go
// ensureDaemonReady 单元测试：daemon 探测、自动拉起、有界轮询与取消语义。
// 通过 startDaemonHook 桩替换平台相关的拉起实现，测试不依赖真实 Docker Desktop。
package sandbox

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

var errDaemonDown = errors.New("Cannot connect to the Docker daemon")

// stubStartDaemon 临时替换平台 daemon 拉起实现，返回恢复函数。
func stubStartDaemon(t *testing.T, result bool) func() {
	t.Helper()
	orig := startDaemonHook
	startDaemonHook = func() bool { return result }
	return func() { startDaemonHook = orig }
}

// speedUpDaemonPolling 将轮询参数调小，使测试在毫秒级完成而非真实等待。
func speedUpDaemonPolling(t *testing.T) func() {
	t.Helper()
	origInterval, origGrace, origStart := daemonProbeInterval, daemonRestartGrace, daemonStartTimeout
	daemonProbeInterval = time.Millisecond
	daemonRestartGrace = 5 * time.Millisecond
	daemonStartTimeout = 30 * time.Millisecond
	return func() {
		daemonProbeInterval, daemonRestartGrace, daemonStartTimeout = origInterval, origGrace, origStart
	}
}

// TestEnsureDaemonReady_AlreadyUp 验证 daemon 已就绪时仅探测一次，不触发拉起。
func TestEnsureDaemonReady_AlreadyUp(t *testing.T) {
	restore := speedUpDaemonPolling(t)
	defer restore()

	started := false
	restoreDaemon := stubStartDaemon(t, true)
	defer restoreDaemon()
	startDaemonHook = func() bool { started = true; return true }

	probes := 0
	run := func(_ context.Context, args ...string) (string, error) {
		if args[0] != "info" {
			t.Errorf("探测应使用 docker info，实际 %v", args)
		}
		probes++
		return "29.4.3", nil
	}
	if err := ensureDaemonReady(context.Background(), run); err != nil {
		t.Fatalf("daemon 已就绪时应立即返回 nil，得到: %v", err)
	}
	if probes != 1 {
		t.Errorf("应仅探测一次，实际 %d 次", probes)
	}
	if started {
		t.Error("daemon 已就绪时不应触发自动拉起")
	}
}

// TestEnsureDaemonReady_StartsDaemon 验证首次探测失败 → 自动拉起 → 轮询至就绪。
func TestEnsureDaemonReady_StartsDaemon(t *testing.T) {
	restore := speedUpDaemonPolling(t)
	defer restore()

	restoreDaemon := stubStartDaemon(t, true)
	defer restoreDaemon()

	probes := 0
	run := func(_ context.Context, _ ...string) (string, error) {
		probes++
		if probes == 1 {
			return "", errDaemonDown
		}
		return "29.4.3", nil
	}
	if err := ensureDaemonReady(context.Background(), run); err != nil {
		t.Fatalf("拉起成功后应轮询至就绪，得到: %v", err)
	}
	if probes != 2 {
		t.Errorf("第二次探测应成功，实际探测 %d 次", probes)
	}
}

// TestEnsureDaemonReady_StartFailedShortWait 验证拉起失败（如未安装 Docker Desktop）
// 时仅做短等待后快速失败——未安装 Docker 的用户不应在启动时白等 90s。
func TestEnsureDaemonReady_StartFailedShortWait(t *testing.T) {
	restore := speedUpDaemonPolling(t)
	defer restore()

	restoreDaemon := stubStartDaemon(t, false)
	defer restoreDaemon()

	start := time.Now()
	run := func(_ context.Context, _ ...string) (string, error) {
		return "", errDaemonDown
	}
	err := ensureDaemonReady(context.Background(), run)
	if err == nil {
		t.Fatal("拉起失败且 daemon 不可用时应返回错误")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("拉起失败应走短等待快速失败，实际耗时 %v", elapsed)
	}
	if !strings.Contains(err.Error(), "自动拉起=false") {
		t.Errorf("错误信息应标明拉起未成功，得到: %v", err)
	}
}

// TestEnsureDaemonReady_TimeoutAfterStart 验证拉起成功但 daemon 始终未就绪时，
// 在 maxWait 上限处返回错误而非无限轮询。
func TestEnsureDaemonReady_TimeoutAfterStart(t *testing.T) {
	restore := speedUpDaemonPolling(t)
	defer restore()

	restoreDaemon := stubStartDaemon(t, true)
	defer restoreDaemon()

	run := func(_ context.Context, _ ...string) (string, error) {
		return "", errDaemonDown
	}
	err := ensureDaemonReady(context.Background(), run)
	if err == nil {
		t.Fatal("daemon 持续不可用时应超时返回错误")
	}
	if !strings.Contains(err.Error(), "仍不可用") {
		t.Errorf("错误信息应说明 daemon 超时，得到: %v", err)
	}
}

// TestEnsureDaemonReady_CtxCancel 验证外部取消（如 Ctrl+C）能立即中断等待。
func TestEnsureDaemonReady_CtxCancel(t *testing.T) {
	restore := speedUpDaemonPolling(t)
	defer restore()
	daemonStartTimeout = 10 * time.Second // 长等待，验证 ctx 取消先于超时生效

	restoreDaemon := stubStartDaemon(t, true)
	defer restoreDaemon()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(5 * time.Millisecond)
		cancel()
	}()
	run := func(_ context.Context, _ ...string) (string, error) {
		return "", errDaemonDown
	}
	err := ensureDaemonReady(ctx, run)
	if err == nil {
		t.Fatal("ctx 取消后应返回错误")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("错误应包装 context.Canceled，得到: %v", err)
	}
}
