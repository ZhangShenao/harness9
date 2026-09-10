// internal/sandbox/container_test.go
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// mockCmdRunner 捕获 docker 命令调用，按顺序返回预设响应。
type mockCmdRunner struct {
	mu        sync.Mutex
	responses []mockResponse
	idx       int
	Calls     [][]string
}

type mockResponse struct {
	out string
	err error
}

func newMock(pairs ...interface{}) *mockCmdRunner {
	m := &mockCmdRunner{}
	for i := 0; i+1 < len(pairs); i += 2 {
		var errVal error
		if pairs[i+1] != nil {
			errVal = pairs[i+1].(error)
		}
		m.responses = append(m.responses, mockResponse{
			out: pairs[i].(string),
			err: errVal,
		})
	}
	return m
}

func errNil() error { return nil }

func (m *mockCmdRunner) run(_ context.Context, args ...string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Calls = append(m.Calls, append([]string{}, args...))
	if m.idx >= len(m.responses) {
		return "", nil
	}
	r := m.responses[m.idx]
	m.idx++
	return r.out, r.err
}

func testCfg() SandboxConfig {
	return SandboxConfig{
		Image:        "ubuntu:22.04",
		CPUs:         "1.0",
		Memory:       "512m",
		PidsLimit:    256,
		StartTimeout: 5 * time.Second,
		StopTimeout:  5 * time.Second,
	}
}

func TestContainerStart_Success(t *testing.T) {
	mock := newMock(
		"abc123def456", errNil(), // docker run → containerID
		"true", errNil(), // docker inspect → running
	)
	c := newContainer("test-uuid", t.TempDir(), testCfg(), mock.run)

	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	if c.State() != StateRunning {
		t.Errorf("State = %v, 期望 Running", c.State())
	}
	if c.dockerID != "abc123def456" {
		t.Errorf("dockerID = %q, 期望 abc123def456", c.dockerID)
	}
}

func TestContainerStart_DockerRunFails(t *testing.T) {
	mock := newMock(
		"", errors.New("daemon not found"),
	)
	c := newContainer("test-uuid", t.TempDir(), testCfg(), mock.run)

	err := c.Start(context.Background())
	if err == nil {
		t.Fatal("docker run 失败时 Start() 应返回 error")
	}
	if c.State() != StateFailed {
		t.Errorf("State = %v, 期望 Failed", c.State())
	}
}

func TestContainerStop(t *testing.T) {
	mock := newMock(
		"abc123", errNil(), // docker run
		"true", errNil(), // docker inspect
		"", errNil(), // docker stop
		"", errNil(), // docker rm
	)
	c := newContainer("test-uuid", t.TempDir(), testCfg(), mock.run)
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if err := c.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() error: %v", err)
	}
	if c.State() != StateTerminated {
		t.Errorf("State = %v, 期望 Terminated", c.State())
	}
}

func TestContainerStateString(t *testing.T) {
	cases := []struct {
		state ContainerState
		want  string
	}{
		{StatePending, "Pending"},
		{StateRunning, "Running"},
		{StateStopping, "Stopping"},
		{StateTerminated, "Terminated"},
		{StateFailed, "Failed"},
	}
	for _, tc := range cases {
		if got := tc.state.String(); got != tc.want {
			t.Errorf("ContainerState(%d).String() = %q, 期望 %q", tc.state, got, tc.want)
		}
	}
}

func TestContainerStart_InspectTimeout(t *testing.T) {
	// inspect 始终返回 "false"，StartTimeout 设置极短触发超时
	mock := newMock(
		"abc123", errNil(), // docker run 成功
		"false", errNil(), // inspect 返回 false（未就绪）
	)
	cfg := testCfg()
	cfg.StartTimeout = 100 * time.Millisecond // 极短超时
	c := newContainer("timeout-uuid", t.TempDir(), cfg, mock.run)

	err := c.Start(context.Background())
	if err == nil {
		t.Fatal("inspect 持续返回 false 时 Start() 应返回 error")
	}
	if c.State() != StateFailed {
		t.Errorf("State = %v, 期望 Failed", c.State())
	}
	if c.Err() == nil {
		t.Error("Err() 应记录超时原因")
	}
}

func TestContainerDockerRunArgs(t *testing.T) {
	mock := newMock(
		"abc123", errNil(),
		"true", errNil(),
	)
	cfg := testCfg()
	workDir := t.TempDir()
	c := newContainer("my-uuid", workDir, cfg, mock.run)
	_ = c.Start(context.Background())

	// 验证 docker run 调用参数包含安全加固选项
	runArgs := mock.Calls[0]
	mustContain := []string{
		"--cap-drop", "all",
		"--security-opt", "no-new-privileges:true",
		"--label", "harness9=1",
		"--name", "harness9-my-uuid",
	}
	argStr := fmt.Sprintf("%v", runArgs)
	for _, want := range mustContain {
		found := false
		for _, arg := range runArgs {
			if arg == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("docker run 参数缺少 %q，完整参数: %s", want, argStr)
		}
	}
}

// TestContainerStart_InspectTimeoutCleansUp 验证：就绪轮询超时（docker run 已成功、
// 容器已存在）时 best-effort 清理容器，避免泄漏持有 bind mount 的孤儿容器
// （孤儿容器在下次会话才被 ReapOrphans 回收，期间拖慢 macOS Docker Desktop）。
func TestContainerStart_InspectTimeoutCleansUp(t *testing.T) {
	mock := newMock(
		"abc123", errNil(), // docker run 成功（容器已创建）
		"false", errNil(), // inspect 返回 false（未就绪）
		"", errNil(), // docker rm -f 清理
	)
	cfg := testCfg()
	cfg.StartTimeout = 100 * time.Millisecond // 短于 200ms 轮询间隔，首次 inspect 后即超时
	c := newContainer("cleanup-uuid", t.TempDir(), cfg, mock.run)

	if err := c.Start(context.Background()); err == nil {
		t.Fatal("inspect 持续返回 false 时 Start() 应返回 error")
	}
	if len(mock.Calls) == 0 {
		t.Fatal("应有 docker 调用记录")
	}
	last := mock.Calls[len(mock.Calls)-1]
	if last[0] != "rm" || last[1] != "-f" || last[2] != "abc123" {
		t.Errorf("Start 超时后应对已创建容器执行 docker rm -f，实际最后调用: %v", last)
	}
}

// TestContainerDockerRunBlockedHosts 验证：配置 NetworkBlockedHosts 后，docker run
// 以 --add-host <host>:0.0.0.0 屏蔽对应域名（DNS 层断网，容器内解析即失败；
// pypi 等未列域名不受影响）。这是 SWE-bench 防上游答案污染的落地机制（P0-1）。
func TestContainerDockerRunBlockedHosts(t *testing.T) {
	mock := newMock(
		"abc123", errNil(),
		"true", errNil(),
	)
	cfg := testCfg()
	cfg.NetworkBlockedHosts = []string{"github.com", "raw.githubusercontent.com"}
	c := newContainer("my-uuid", t.TempDir(), cfg, mock.run)
	_ = c.Start(context.Background())

	runArgs := mock.Calls[0]
	for _, host := range cfg.NetworkBlockedHosts {
		want := host + ":0.0.0.0"
		found := false
		for i, arg := range runArgs {
			if arg == "--add-host" && i+1 < len(runArgs) && runArgs[i+1] == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("docker run 参数缺少 --add-host %q，完整参数: %v", want, runArgs)
		}
	}
}

// TestContainerDockerRunNoAddHostByDefault 验证：默认配置（未设置 NetworkBlockedHosts）
// 的 docker run 不携带任何 --add-host，行为与引入前完全一致（向后兼容）。
func TestContainerDockerRunNoAddHostByDefault(t *testing.T) {
	mock := newMock(
		"abc123", errNil(),
		"true", errNil(),
	)
	c := newContainer("my-uuid", t.TempDir(), testCfg(), mock.run)
	_ = c.Start(context.Background())

	for _, arg := range mock.Calls[0] {
		if arg == "--add-host" {
			t.Fatalf("默认配置不应出现 --add-host，完整参数: %v", mock.Calls[0])
		}
	}
}
