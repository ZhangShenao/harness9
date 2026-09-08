// internal/sandbox/manager_test.go
package sandbox

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestManager_BootstrapCmd_Runs 验证：配置 BootstrapCmd 时，容器就绪后通过 docker exec
// 执行一次该命令（接入官方预装镜像 / 依赖安装的关键接缝）。
func TestManager_BootstrapCmd_Runs(t *testing.T) {
	cfg := testCfg()
	cfg.BootstrapCmd = "pip install -e . -q"
	cfg.BootstrapTimeout = 5 * time.Second
	mgr := NewManager(cfg)

	var mock *mockCmdRunner
	mgr.runnerFactory = func(id, workDir string, c SandboxConfig) cmdRunner {
		mock = newMock(
			id[:8], errNil(), // docker run
			"true", errNil(), // docker inspect → running
			"ok", errNil(), // bootstrap docker exec
		)
		return mock.run
	}
	if _, err := mgr.Create(context.Background(), t.TempDir()); err != nil {
		t.Fatalf("Create: %v", err)
	}

	found := false
	for _, call := range mock.Calls {
		joined := strings.Join(call, " ")
		if strings.Contains(joined, "exec") && strings.Contains(joined, "pip install -e . -q") {
			found = true
		}
	}
	if !found {
		t.Errorf("bootstrap 命令应通过 docker exec 执行，Calls=%v", mock.Calls)
	}
}

// TestManager_NoBootstrapByDefault 验证：默认（BootstrapCmd 为空）不触发任何 exec 调用。
func TestManager_NoBootstrapByDefault(t *testing.T) {
	mgr := newTestManager() // testCfg 不设 BootstrapCmd
	var mock *mockCmdRunner
	mgr.runnerFactory = func(id, workDir string, c SandboxConfig) cmdRunner {
		mock = newMock(id[:8], errNil(), "true", errNil())
		return mock.run
	}
	if _, err := mgr.Create(context.Background(), t.TempDir()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, call := range mock.Calls {
		if len(call) > 0 && call[0] == "exec" {
			t.Errorf("未配置 BootstrapCmd 时不应有 exec 调用，Calls=%v", mock.Calls)
		}
	}
}

// newTestManager 创建使用 mock runner 的 Manager，不真实启动 Docker。
func newTestManager() *Manager {
	cfg := testCfg()
	mgr := NewManager(cfg)
	mgr.runnerFactory = func(id, workDir string, c SandboxConfig) cmdRunner {
		return newMock(
			id[:8], errNil(), // docker run → dockerID = id prefix（id 是 16 位 hex，前 8 位够用）
			"true", errNil(), // docker inspect → running
			"", errNil(), // docker stop
			"", errNil(), // docker rm
		).run
	}
	return mgr
}

func TestManager_CreateAndListAll(t *testing.T) {
	mgr := newTestManager()
	workDir := t.TempDir()

	env, err := mgr.Create(context.Background(), workDir)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if env == nil {
		t.Fatal("Create 应返回非 nil Environment")
	}

	infos := mgr.ListAll()
	if len(infos) != 1 {
		t.Errorf("ListAll 应有 1 个 Sandbox，实际 %d", len(infos))
	}
	if infos[0].State != StateRunning {
		t.Errorf("Sandbox 状态应为 Running，实际 %v", infos[0].State)
	}
	if infos[0].WorkDir != workDir {
		t.Errorf("WorkDir = %q, 期望 %q", infos[0].WorkDir, workDir)
	}
}

func TestManager_Destroy(t *testing.T) {
	mgr := newTestManager()
	env, _ := mgr.Create(context.Background(), t.TempDir())

	if err := mgr.Destroy(context.Background(), env.ID()); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if len(mgr.ListAll()) != 0 {
		t.Error("Destroy 后 ListAll 应为空")
	}
}

func TestManager_DestroyAll(t *testing.T) {
	mgr := newTestManager()
	for i := 0; i < 3; i++ {
		if _, err := mgr.Create(context.Background(), t.TempDir()); err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
	}
	if len(mgr.ListAll()) != 3 {
		t.Fatalf("期望 3 个 Sandbox")
	}

	mgr.DestroyAll(context.Background())
	if len(mgr.ListAll()) != 0 {
		t.Error("DestroyAll 后 ListAll 应为空")
	}
}

func TestManager_ConcurrentCreate(t *testing.T) {
	mgr := newTestManager()
	var wg sync.WaitGroup
	const n = 10

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := mgr.Create(context.Background(), t.TempDir()); err != nil {
				t.Errorf("并发 Create 失败: %v", err)
			}
		}()
	}
	wg.Wait()

	infos := mgr.ListAll()
	if len(infos) != n {
		t.Errorf("并发 Create 后应有 %d 个 Sandbox，实际 %d", n, len(infos))
	}

	// 验证所有 ID 唯一
	ids := make(map[string]bool)
	for _, info := range infos {
		if ids[info.ID] {
			t.Errorf("Sandbox ID 重复: %s", info.ID)
		}
		ids[info.ID] = true
	}
}

func TestManager_WithUpdateNotify(t *testing.T) {
	mgr := newTestManager()
	var mu sync.Mutex
	var received [][]SandboxInfo
	mgr.WithUpdateNotify(func(infos []SandboxInfo) {
		mu.Lock()
		received = append(received, infos)
		mu.Unlock()
	})

	mgr.Create(context.Background(), t.TempDir())

	mu.Lock()
	defer mu.Unlock()
	if len(received) == 0 {
		t.Error("Create 后应触发 onUpdate 通知")
	}
}

// TestManager_CreateWithRetry_RetriesAfterFailure 验证：daemon 就绪后首次 Create 失败
// （如 Docker Desktop 冷启动时镜像首次挂载超时）自动重试一次并成功。
// 单次失败即永久降级的代价（整个会话本地执行）远高于多等几秒，这是启动路径的韧性要求。
func TestManager_CreateWithRetry_RetriesAfterFailure(t *testing.T) {
	mgr := NewManager(testCfg())
	mgr.daemonRunner = func(_ context.Context, _ ...string) (string, error) {
		return "29.4.3", nil
	}
	attempts := 0
	mgr.runnerFactory = func(id, workDir string, c SandboxConfig) cmdRunner {
		attempts++
		if attempts == 1 {
			// 首次：docker run 失败
			return newMock("", errors.New("startup timeout")).run
		}
		return newMock(
			id[:8], errNil(), // docker run
			"true", errNil(), // inspect → running
			"", errNil(), // stop
			"", errNil(), // rm
		).run
	}
	env, err := mgr.CreateWithRetry(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("首次失败后重试应成功: %v", err)
	}
	if env == nil || env.ID() == "" {
		t.Errorf("应返回可用的 Environment，得到 %+v", env)
	}
	if attempts != 2 {
		t.Errorf("应恰好尝试创建 2 次，实际 %d 次", attempts)
	}
}

// TestManager_CreateWithRetry_DaemonUnavailable 验证：daemon 不可用且拉起失败时
// 快速返回错误，不发起容器创建。
func TestManager_CreateWithRetry_DaemonUnavailable(t *testing.T) {
	restore := speedUpDaemonPolling(t)
	defer restore()
	restoreDaemon := stubStartDaemon(t, false)
	defer restoreDaemon()

	mgr := NewManager(testCfg())
	mgr.daemonRunner = func(_ context.Context, _ ...string) (string, error) {
		return "", errDaemonDown
	}
	created := false
	mgr.runnerFactory = func(id, workDir string, c SandboxConfig) cmdRunner {
		created = true
		return newMock(id[:8], errNil(), "true", errNil()).run
	}
	_, err := mgr.CreateWithRetry(context.Background(), t.TempDir())
	if err == nil {
		t.Fatal("daemon 不可用时应返回错误")
	}
	if created {
		t.Error("daemon 不可用时不应发起容器创建")
	}
}

// TestManager_CreateWithRetry_BothAttemptsFail 验证：重试后仍失败时返回错误，
// 且错误中保留首次失败信息便于诊断。
func TestManager_CreateWithRetry_BothAttemptsFail(t *testing.T) {
	mgr := NewManager(testCfg())
	mgr.daemonRunner = func(_ context.Context, _ ...string) (string, error) {
		return "29.4.3", nil
	}
	mgr.runnerFactory = func(id, workDir string, c SandboxConfig) cmdRunner {
		return newMock("", errors.New("first error detail")).run
	}
	_, err := mgr.CreateWithRetry(context.Background(), t.TempDir())
	if err == nil {
		t.Fatal("两次创建均失败时应返回错误")
	}
	if !strings.Contains(err.Error(), "first error detail") {
		t.Errorf("错误应保留首次失败信息便于诊断，得到: %v", err)
	}
}
