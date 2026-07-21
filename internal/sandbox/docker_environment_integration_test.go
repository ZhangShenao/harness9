//go:build integration

// internal/sandbox/docker_environment_integration_test.go
package sandbox

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestDockerEnvironmentIntegration_RunBash(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Image = "ubuntu:22.04"
	cfg.StartTimeout = 60 * time.Second

	c := newContainer("integ-test", t.TempDir(), cfg, realCmdRunner)
	if err := c.Start(context.Background()); err != nil {
		t.Skipf("Docker 不可用，跳过集成测试: %v", err)
	}
	defer c.Stop(context.Background())

	env := newDockerEnvironment(c.dockerID, c.id, t.TempDir(), realCmdRunner)
	out, err := env.RunBash(context.Background(), "echo hello_from_container", "/tmp")
	if err != nil {
		t.Fatalf("RunBash: %v", err)
	}
	if !strings.Contains(out, "hello_from_container") {
		t.Errorf("RunBash = %q, 期望包含 hello_from_container", out)
	}
}

// TestDockerEnvironmentIntegration_RunBash_BackgroundedProcessDoesNotHang 是一个回归
// 守卫测试，而非某个已确认缺陷的复现：internal/tools/bash.go 的 runLocal 对
// "A && B &" 复合后台任务命令存在真实的、已复现的挂起缺陷（CombinedOutput() 的
// pipe/Wait 语义），曾推测 docker exec 路径（realCmdRunner）存在同构缺陷，但
// 实测（本测试 + 独立仓库外的多组复现程序）未能复现——docker exec 是连接
// dockerd 的瘦客户端，I/O 通过守护进程的 API/socket 协议多路复用，不是宿主机
// 层面的 OS pipe fd 继承，其 Wait() 语义与本地 fork 的 bash -c 不同。结论：
// realCmdRunner 未被修改，本测试作为回归守卫保留——如果未来某个 Docker/OS
// 组合确实出现了挂起，这个测试会失败并报警。详见
// docs/技术调研/terminal-bench-轨迹分析-v1.md §4 item 3（已订正）。
func TestDockerEnvironmentIntegration_RunBash_BackgroundedProcessDoesNotHang(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Image = "ubuntu:22.04"
	cfg.StartTimeout = 60 * time.Second

	c := newContainer("integ-test-bg", t.TempDir(), cfg, realCmdRunner)
	if err := c.Start(context.Background()); err != nil {
		t.Skipf("Docker 不可用，跳过集成测试: %v", err)
	}
	defer c.Stop(context.Background())

	env := newDockerEnvironment(c.dockerID, c.id, t.TempDir(), realCmdRunner)

	start := time.Now()
	out, err := env.RunBash(context.Background(),
		`nohup sleep 20 > /tmp/bg.log 2>&1 & echo "started"`, "/tmp")
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("RunBash: %v", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("应在容器内后台进程退出前就返回，实际耗时 %v", elapsed)
	}
	if !strings.Contains(out, "started") {
		t.Errorf("应捕获到前台部分的输出，got %q", out)
	}

	// 确认容器内后台进程没有被误杀。
	checkOut, checkErr := env.RunBash(context.Background(), "pgrep -f 'sleep 20'", "/tmp")
	if checkErr != nil || strings.TrimSpace(checkOut) == "" {
		t.Error("容器内后台进程应仍在运行，不应被本次修复误杀")
	}
}
