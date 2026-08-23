package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLocalEnvironment_ID(t *testing.T) {
	env := NewLocalEnvironment()
	if env.ID() == "" {
		t.Error("ID() 不应为空")
	}
}

func TestLocalEnvironment_RunBash_Success(t *testing.T) {
	env := NewLocalEnvironment()
	out, err := env.RunBash(context.Background(), "echo hello", t.TempDir())
	if err != nil {
		t.Fatalf("RunBash 不应返回 Go error: %v", err)
	}
	if out != "hello\n" {
		t.Errorf("RunBash = %q, 期望 %q", out, "hello\n")
	}
}

func TestLocalEnvironment_RunBash_Failure(t *testing.T) {
	env := NewLocalEnvironment()
	out, err := env.RunBash(context.Background(), "exit 1", t.TempDir())
	if err != nil {
		t.Fatalf("RunBash 失败时不应返回 Go error（Self-Correction Loopback）: %v", err)
	}
	if out == "" {
		t.Error("RunBash 失败时应返回非空错误字符串")
	}
}

func TestLocalEnvironment_ReadWriteFile(t *testing.T) {
	env := NewLocalEnvironment()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")

	if err := env.WriteFile(context.Background(), path, []byte("hello")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	data, err := env.ReadFile(context.Background(), path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "hello" {
		t.Errorf("ReadFile = %q, 期望 hello", data)
	}
}

func TestLocalEnvironment_WriteFile_AutoMkdir(t *testing.T) {
	env := NewLocalEnvironment()
	path := filepath.Join(t.TempDir(), "nested", "dir", "file.txt")

	if err := env.WriteFile(context.Background(), path, []byte("data")); err != nil {
		t.Fatalf("WriteFile（自动创建目录）: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("WriteFile 后文件应存在: %v", err)
	}
}

func TestLocalEnvironment_Close(t *testing.T) {
	env := NewLocalEnvironment()
	if err := env.Close(context.Background()); err != nil {
		t.Errorf("LocalEnvironment.Close() 不应返回 error: %v", err)
	}
}

// TestLocalEnvironment_RunBash_ContextCancel 验证 Environment 接口契约：
// ctx 取消后正在运行的命令必须被终止（exec.CommandContext 绑定），
// RunBash 及时返回而非无限阻塞。修复前此用例失败（exec.Command 不感知 ctx）。
func TestLocalEnvironment_RunBash_ContextCancel(t *testing.T) {
	env := NewLocalEnvironment()
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	var out string
	go func() {
		defer close(done)
		out, _ = env.RunBash(ctx, "sleep 30 && echo never", t.TempDir())
	}()

	cancel() // 立即取消：sleep 应被 SIGKILL 终止

	select {
	case <-done:
		// RunBash 已返回；失败语义通过 Self-Correction Loopback 以文本承载（err==nil）
		if out == "" {
			t.Error("取消后返回的输出不应为空（应包含终止错误信息）")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ctx 取消后 RunBash 应及时返回，命令未被终止（未绑定 ctx）")
	}
}
