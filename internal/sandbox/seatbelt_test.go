package sandbox

import (
	"context"
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

// 本文件是 SeatbeltEnvironment 的真实沙箱冒烟测试（依赖 macOS sandbox-exec）。
// 非 darwin 或缺少 sandbox-exec 时自动跳过。

func skipNoSeatbelt(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("Seatbelt 仅在 macOS 上可用")
	}
	if _, err := exec.LookPath("sandbox-exec"); err != nil {
		t.Skip("sandbox-exec 不可用")
	}
}

// isDarwin 供纯函数用例判断平台语义。
var isDarwin = runtime.GOOS == "darwin"

// TestSeatbeltWorkspaceWrite 验证工作区写档：workDir 可写、全盘可读。
func TestSeatbeltWorkspaceWrite(t *testing.T) {
	skipNoSeatbelt(t)
	env := NewSeatbeltEnvironment(t.TempDir(), SeatbeltWorkspaceWrite)
	ctx := context.Background()

	out, err := env.RunBash(ctx, `echo -n ok > inside.txt && cat inside.txt`, env.workDir)
	if err != nil {
		t.Fatalf("RunBash: %v", err)
	}
	if strings.TrimSpace(out) != "ok" {
		t.Fatalf("工作区写入+回读失败: %q", out)
	}

	out, err = env.RunBash(ctx, `cat /etc/hosts | head -1`, env.workDir)
	if err != nil {
		t.Fatalf("RunBash(读系统文件): %v", err)
	}
	if strings.TrimSpace(out) == "" {
		t.Fatalf("/etc/hosts 读取结果为空")
	}

	// 可写根之外（HOME 根目录）应被拒绝
	out, _ = env.RunBash(ctx, `echo leak > $HOME/seatbelt_leak_test.txt 2>&1; echo exit=$?`, env.workDir)
	if !strings.Contains(out, "exit=1") || !strings.Contains(out, "not permitted") {
		t.Fatalf("可写根之外写入应被拒绝: %q", out)
	}
}

// TestSeatbeltReadOnly 验证只读档：写操作被拒绝。
func TestSeatbeltReadOnly(t *testing.T) {
	skipNoSeatbelt(t)
	env := NewSeatbeltEnvironment(t.TempDir(), SeatbeltReadOnly)
	ctx := context.Background()

	if env.Policy() != SeatbeltReadOnly {
		t.Fatalf("策略档位不符: %s", env.Policy())
	}

	out, _ := env.RunBash(ctx, `touch write_test.txt 2>&1; echo exit=$?`, env.workDir)
	if !strings.Contains(out, "exit=1") || !strings.Contains(out, "not permitted") {
		t.Fatalf("只读档下 workDir 写入应被拒绝: %q", out)
	}

	// 读操作不受影响
	out, err := env.RunBash(ctx, `cat /etc/hosts | head -1`, env.workDir)
	if err != nil || strings.TrimSpace(out) == "" {
		t.Fatalf("只读档下读取应放行: out=%q err=%v", out, err)
	}
}

// TestSeatbeltNetworkPolicy 验证网络策略：只读禁网、工作区写允许网络访问。
// 用本机回环端口区分"被沙箱拒绝"（not permitted）与"正常拒绝连接"（refused）。
func TestSeatbeltNetworkPolicy(t *testing.T) {
	skipNoSeatbelt(t)
	ctx := context.Background()

	// 只读档：连接尝试被沙箱拦截
	ro := NewSeatbeltEnvironment(t.TempDir(), SeatbeltReadOnly)
	out, _ := ro.RunBash(ctx, `echo > /dev/tcp/127.0.0.1/65530 2>&1; echo exit=$?`, ro.workDir)
	if strings.Contains(out, "exit=0") {
		t.Fatalf("只读档不应有网络出站: %q", out)
	}
	if !strings.Contains(out, "not permitted") && !strings.Contains(out, "exit=1") {
		t.Fatalf("只读档网络拒绝形态不符: %q", out)
	}

	// 工作区写档：允许网络（连未监听端口应得到 refused 而非 not permitted）
	ww := NewSeatbeltEnvironment(t.TempDir(), SeatbeltWorkspaceWrite)
	out, _ = ww.RunBash(ctx, `echo > /dev/tcp/127.0.0.1/65530 2>&1; echo exit=$?`, ww.workDir)
	if strings.Contains(out, "not permitted") {
		t.Fatalf("工作区写档不应拦截网络: %q", out)
	}
}

// TestSeatbeltProfileBuild 纯函数校验 profile 生成规则（无需 sandbox-exec）。
func TestSeatbeltProfileBuild(t *testing.T) {
	if !isDarwin {
		t.Skip("仅验证 darwin 语义")
	}
	workDir := "/tmp/proj"
	ww := NewSeatbeltEnvironment(workDir, SeatbeltWorkspaceWrite)
	profile := ww.buildProfile()
	for _, want := range []string{"(deny default)", "system.sb", "(allow file-read*)", "(allow network*)", "(allow file-write*"} {
		if !strings.Contains(profile, want) {
			t.Fatalf("workspace-write profile 缺少 %q:\n%s", want, profile)
		}
	}

	ro := NewSeatbeltEnvironment(workDir, SeatbeltReadOnly)
	roProfile := ro.buildProfile()
	if strings.Contains(roProfile, "(allow network") || strings.Contains(roProfile, "(allow file-write") {
		t.Fatalf("read-only profile 不应包含写/网络放行:\n%s", roProfile)
	}

	// 可写根必须做符号链接解析（macOS /tmp → /private/tmp）
	roots := ww.writableRoots()
	foundWorkDir := false
	for _, r := range roots {
		if strings.HasSuffix(r, "proj") {
			foundWorkDir = true
		}
	}
	if !foundWorkDir {
		t.Fatalf("可写根应包含 workDir: %v", roots)
	}
}
