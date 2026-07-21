package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestBashTool_Name(t *testing.T) {
	tool := NewBashTool("/tmp")
	if tool.Name() != "bash" {
		t.Errorf("expected 'bash', got %q", tool.Name())
	}
}

func TestBashTool_Definition(t *testing.T) {
	tool := NewBashTool("/tmp")
	def := tool.Definition()
	if def.Name != "bash" {
		t.Errorf("definition name mismatch: %q", def.Name)
	}
	if def.Description == "" {
		t.Error("definition should have a description")
	}
	if def.InputSchema == nil {
		t.Error("definition should have an input schema")
	}
}

func TestBashTool_Execute_BasicCommand(t *testing.T) {
	tool := NewBashTool("/tmp")
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"command":"echo hello"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "hello") {
		t.Errorf("expected output containing 'hello', got %q", out)
	}
}

func TestBashTool_Execute_EmptyCommand(t *testing.T) {
	tool := NewBashTool("/tmp")
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"command":""}`))
	if err != nil {
		t.Fatalf("empty command should not return Go error, got: %v", err)
	}
	if !strings.Contains(out, "Error") {
		t.Errorf("empty command should return error string, got: %q", out)
	}
}

func TestBashTool_Execute_NonZeroExitCode(t *testing.T) {
	tool := NewBashTool("/tmp")
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"command":"exit 1"}`))
	if err != nil {
		t.Fatalf("non-zero exit should not return Go error (Self-Correction Loopback), got: %v", err)
	}
	if !strings.Contains(out, "执行报错") {
		t.Errorf("non-zero exit should contain error info, got: %q", out)
	}
}

func TestBashTool_Execute_EmptyOutput(t *testing.T) {
	tool := NewBashTool("/tmp")
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"command":"true"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "成功") {
		t.Errorf("silent command should say success, got: %q", out)
	}
}

func TestBashTool_Execute_LargeOutput(t *testing.T) {
	dir := t.TempDir()
	content := strings.Repeat("x", maxOutputLen+100)
	if err := os.WriteFile(dir+"/large.txt", []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	tool := NewBashTool(dir)
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"command":"cat large.txt"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "截断") {
		t.Errorf("large output should include truncation notice, got length=%d", len(out))
	}
}

func TestBashTool_Execute_PipeCommand(t *testing.T) {
	tool := NewBashTool("/tmp")
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"command":"printf 'hello' | tr 'a-z' 'A-Z'"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "HELLO") {
		t.Errorf("pipe command should work, got: %q", out)
	}
}

func TestBashTool_Execute_BadJSON(t *testing.T) {
	tool := NewBashTool("/tmp")
	_, err := tool.Execute(context.Background(), json.RawMessage(`not_json`))
	if err == nil {
		t.Fatal("expected error for malformed JSON args")
	}
}

// TestBashTool_Execute_ParentContextCancelled verifies that a long-running command is
// killed when the parent context times out, and the result is returned as a string (not
// a Go error), preserving the Self-Correction Loopback contract.
//
// When the parent context's DeadlineExceeded propagates to the child timeoutCtx, bash.go
// detects it as DeadlineExceeded and emits a timeout warning rather than an exec error
// string — both are acceptable informative outputs, so this test only checks for no
// panic and no Go error.
func TestBashTool_Execute_ParentContextCancelled(t *testing.T) {
	tool := NewBashTool("/tmp")
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	out, err := tool.Execute(ctx, json.RawMessage(`{"command":"sleep 10"}`))
	if err != nil {
		t.Fatalf("bash should never return Go error, got: %v", err)
	}
	// The killed command should produce some informative output (timeout warning or error
	// string) — never an empty string or a spurious success message.
	if out == "" || strings.Contains(out, "成功") {
		t.Errorf("killed command should return informative output, got: %q", out)
	}
}

// TestBashTool_Execute_BackgroundedProcessDoesNotHang 验证形如 "A && B &" 的复合后台任务
// 命令能立刻返回（不等待后台进程退出），且后台进程本身不会被误杀。
// 复现 docs/技术调研/terminal-bench-轨迹分析-v1.md §1（R1）：CombinedOutput() 内部经
// pipe + 拷贝 goroutine，Wait() 会等待所有 fd 持有者（包括 bash 为 "&&" 链表 fork 的
// 子 shell）关闭；只要后台任务还活着，就永久阻塞直到外层超时。
func TestBashTool_Execute_BackgroundedProcessDoesNotHang(t *testing.T) {
	dir := t.TempDir()
	tool := NewBashTool(dir, WithBashTimeout(5*time.Second))

	// 用 echo $! 拿到后台进程的精确 PID，之后按 PID 而非命令行子串核对存活状态——
	// 避免 pgrep -f 的子串匹配在共享 CI 机器上误命中不相关进程（历史上出现过的
	// flakiness 来源）。
	cmd := fmt.Sprintf(`cd %s && nohup sleep 20 > bg.log 2>&1 & echo "PID:$!"`, dir)
	args, err := json.Marshal(bashArgs{Command: cmd})
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	out, execErr := tool.Execute(context.Background(), args)
	elapsed := time.Since(start)

	if execErr != nil {
		t.Fatalf("bash should never return Go error, got: %v", execErr)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("应在后台进程退出前就返回（不应等待 sleep 20），实际耗时 %v", elapsed)
	}
	pidLine := strings.TrimSpace(out)
	if !strings.HasPrefix(pidLine, "PID:") {
		t.Fatalf("应捕获到前台部分的输出（含后台进程 PID），got %q", out)
	}
	pid, err := strconv.Atoi(strings.TrimPrefix(pidLine, "PID:"))
	if err != nil {
		t.Fatalf("解析后台进程 PID 失败: %v, 原始输出: %q", err, out)
	}

	t.Cleanup(func() {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	})

	// 确认后台进程没有被误杀——不能用"杀进程组"解决挂起，否则会杀掉任务明确要求
	// "保持运行"的后台服务（如启动一个长驻服务器）。signal 0 只检查进程是否存在，
	// 不会真的发信号。
	time.Sleep(200 * time.Millisecond)
	if err := syscall.Kill(pid, 0); err != nil {
		t.Errorf("后台进程（PID %d）应仍在运行，不应被本次修复误杀: %v", pid, err)
	}
}

// TestBashTool_Truncation_KeepsHeadAndTail 验证大输出同时保留头部与尾部，
// 不再像旧实现那样只留头部丢尾部（测试失败摘要/traceback 在尾部）。
func TestBashTool_Truncation_KeepsHeadAndTail(t *testing.T) {
	head := "HEAD_MARKER_START"
	tail := "TAIL_MARKER_FAILED_ASSERTION"
	mid := strings.Repeat("x", maxOutputLen+1000)
	full := head + mid + tail
	env := &mockEnv{runOut: full}
	tool := NewBashTool("/tmp", WithEnvironment(env))

	out, err := tool.Execute(context.Background(), json.RawMessage(`{"command":"pytest"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, head) {
		t.Error("截断后应保留头部")
	}
	if !strings.Contains(out, tail) {
		t.Error("截断后必须保留尾部（pytest 的 FAILED/traceback 在尾部）")
	}
	if !strings.Contains(out, "截断") {
		t.Error("应包含截断标记")
	}
}

// TestBashTool_PerCallTimeout 验证 timeout_secs 能放宽单次命令超时。
func TestBashTool_PerCallTimeout(t *testing.T) {
	tool := NewBashTool("/tmp")
	// 默认 120s 足够；这里只验证 timeout_secs 被解析且命令正常完成。
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"command":"echo ok","timeout_secs":5}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "ok") {
		t.Errorf("expected ok, got %q", out)
	}
}

// TestBashTool_EffectiveTimeout 验证超时优先级：per-call > 工具配置 > 默认。
func TestBashTool_EffectiveTimeout(t *testing.T) {
	base := NewBashTool("/tmp")
	if got := base.effectiveTimeout(0); got != defaultBashTimeout {
		t.Errorf("default timeout = %v, want %v", got, defaultBashTimeout)
	}
	configured := NewBashTool("/tmp", WithBashTimeout(300*time.Second))
	if got := configured.effectiveTimeout(0); got != 300*time.Second {
		t.Errorf("configured timeout = %v, want 300s", got)
	}
	if got := configured.effectiveTimeout(45); got != 45*time.Second {
		t.Errorf("per-call timeout should win: got %v, want 45s", got)
	}
	// 钳制到上限
	if got := base.effectiveTimeout(99999); got != maxBashTimeout {
		t.Errorf("per-call timeout should be clamped to %v, got %v", maxBashTimeout, got)
	}
}

// TestBashTool_TimeoutBanner 验证本地超时路径追加清晰的 TIMEOUT 横幅。
func TestBashTool_TimeoutBanner(t *testing.T) {
	tool := NewBashTool("/tmp", WithBashTimeout(120*time.Second))
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	out, err := tool.Execute(ctx, json.RawMessage(`{"command":"sleep 10"}`))
	if err != nil {
		t.Fatalf("bash 不应返回 Go error: %v", err)
	}
	if !strings.Contains(out, "TIMEOUT") {
		t.Errorf("超时应包含 TIMEOUT 横幅，got: %q", out)
	}
}

// mockEnv 是 sandbox.Environment 的测试 mock，记录所有 RunBash 调用。
type mockEnv struct {
	runOut string
	runErr error
	Calls  []string
}

func (m *mockEnv) RunBash(_ context.Context, cmd, _ string) (string, error) {
	m.Calls = append(m.Calls, cmd)
	return m.runOut, m.runErr
}
func (m *mockEnv) ReadFile(_ context.Context, _ string) ([]byte, error)  { return nil, nil }
func (m *mockEnv) WriteFile(_ context.Context, _ string, _ []byte) error { return nil }
func (m *mockEnv) ID() string                                            { return "mock-env" }
func (m *mockEnv) Close(_ context.Context) error                         { return nil }

func TestBashTool_WithEnvironment_RoutesToEnv(t *testing.T) {
	env := &mockEnv{runOut: "container output\n"}
	tool := NewBashTool("/tmp", WithEnvironment(env))

	out, err := tool.Execute(context.Background(), json.RawMessage(`{"command":"echo hello"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out != "container output\n" {
		t.Errorf("应路由到 env.RunBash，output = %q", out)
	}
	if len(env.Calls) != 1 || env.Calls[0] != "echo hello" {
		t.Errorf("env.RunBash 未被调用，Calls = %v", env.Calls)
	}
}

func TestBashTool_NilEnvironment_UsesLocal(t *testing.T) {
	tool := NewBashTool("/tmp")
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"command":"echo local"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "local") {
		t.Errorf("nil env 应走本地执行，output = %q", out)
	}
}

func TestBashTool_WithEnvironment_LargeOutputTruncated(t *testing.T) {
	largeOut := strings.Repeat("x", maxOutputLen+100)
	env := &mockEnv{runOut: largeOut}
	tool := NewBashTool("/tmp", WithEnvironment(env))

	out, err := tool.Execute(context.Background(), json.RawMessage(`{"command":"big"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "截断") {
		t.Errorf("大输出应被截断，output length = %d", len(out))
	}
}

func TestBashTool_WithEnvironment_EmptyOutput(t *testing.T) {
	env := &mockEnv{runOut: ""}
	tool := NewBashTool("/tmp", WithEnvironment(env))

	out, err := tool.Execute(context.Background(), json.RawMessage(`{"command":"true"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "成功") {
		t.Errorf("空输出应提示成功，got: %q", out)
	}
}

func TestBashTool_WithEnvironment_EnvError(t *testing.T) {
	// RunBash 返回非空 error（环境级错误，非命令失败）
	env := &mockEnv{runOut: "", runErr: fmt.Errorf("container not found")}
	tool := NewBashTool("/tmp", WithEnvironment(env))

	out, err := tool.Execute(context.Background(), json.RawMessage(`{"command":"echo hi"}`))
	if err != nil {
		t.Fatalf("Execute 不应返回 Go error: %v", err)
	}
	if !strings.Contains(out, "执行报错") {
		t.Errorf("env error 应转为错误字符串，got: %q", out)
	}
}
