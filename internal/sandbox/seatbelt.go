package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// SeatbeltPolicy 是 Seatbelt 沙箱的访问策略档位（对标 Codex 的 sandbox mode）。
//
// 两档策略覆盖桌面端的核心场景：
//   - SeatbeltReadOnly：只读探索——可读全盘、可执行进程，禁止写入与网络；
//   - SeatbeltWorkspaceWrite：工作区写——在只读基础上开放 workDir/TMPDIR/~/.harness9
//     的写入与网络访问，适配常规编码任务。
//
// 定位是行为护栏而非硬边界（与 NetworkBlockedHosts 一致的非对抗性威胁模型）：
// sandbox-exec 已被 Apple 标记 deprecated 但仍随 macOS 出货，OpenAI Codex 亦采用
// 同机制。真正的硬边界场景（对抗性代码执行）应使用 Docker 后端。
type SeatbeltPolicy string

const (
	// SeatbeltReadOnly 只读探索档：禁写、禁网。
	SeatbeltReadOnly SeatbeltPolicy = "read-only"
	// SeatbeltWorkspaceWrite 工作区写档：workDir/TMPDIR/~/.harness9 可写，允许网络。
	SeatbeltWorkspaceWrite SeatbeltPolicy = "workspace-write"
)

// SeatbeltEnvironment 是 macOS sandbox-exec 原生沙箱环境。
// 每次 RunBash 都以 deny-by-default 的 Seatbelt profile 拉起子进程；
// 文件读写与 LocalEnvironment 一致走宿主 IO（profile 约束的是子进程，
// Go 侧的 ReadFile/WriteFile 仍由 safePath 沙箱负责路径校验）。
//
// 与 Docker 后端的差异：无容器生命周期（不经过 Manager，无 Create/Destroy），
// 启动零等待、零守护进程依赖，适合桌面产品的一等默认后端。
type SeatbeltEnvironment struct {
	id      string
	policy  SeatbeltPolicy
	workDir string
	// extraWritableRoots 是策略之外追加的可写根（如用户自定义目录），可为空。
	extraWritableRoots []string
}

// NewSeatbeltEnvironment 构造 Seatbelt 沙箱环境。
// workDir 是可写根与默认执行目录；homeDir 用于放行 ~/.harness9 状态目录，
// 传空时自动从 os.UserHomeDir() 取。
func NewSeatbeltEnvironment(workDir string, policy SeatbeltPolicy) *SeatbeltEnvironment {
	if policy != SeatbeltReadOnly && policy != SeatbeltWorkspaceWrite {
		policy = SeatbeltWorkspaceWrite
	}
	return &SeatbeltEnvironment{
		id:      "seatbelt-" + generateID(),
		policy:  policy,
		workDir: workDir,
	}
}

// WithExtraWritableRoots 追加可写根目录。
func (e *SeatbeltEnvironment) WithExtraWritableRoots(roots ...string) *SeatbeltEnvironment {
	e.extraWritableRoots = append(e.extraWritableRoots, roots...)
	return e
}

// ID 返回环境唯一标识（seatbelt 前缀）。
func (e *SeatbeltEnvironment) ID() string { return e.id }

// Policy 返回当前策略档位。
func (e *SeatbeltEnvironment) Policy() SeatbeltPolicy { return e.policy }

// Close 释放资源（Seatbelt 无持外部资源，no-op）。
func (e *SeatbeltEnvironment) Close(_ context.Context) error { return nil }

// RunBash 在 Seatbelt profile 约束下执行 bash 命令。
// 语义与 LocalEnvironment 一致：非零退出返回 (错误文本, nil)，
// 保持工具层的 Self-Correction Loopback。
func (e *SeatbeltEnvironment) RunBash(ctx context.Context, cmd, workDir string) (string, error) {
	profile := e.buildProfile()
	c := exec.CommandContext(ctx, "sandbox-exec", "-p", profile, "bash", "-c", cmd)
	c.Dir = workDir
	out, err := c.CombinedOutput()
	if err != nil {
		return fmt.Sprintf("执行报错: %v\n输出:\n%s", err, out), nil
	}
	return string(out), nil
}

// ReadFile 从宿主文件系统读取文件（由工具层 safePath 负责路径校验）。
func (e *SeatbeltEnvironment) ReadFile(_ context.Context, path string) ([]byte, error) {
	return os.ReadFile(path)
}

// WriteFile 向宿主文件系统写入文件，自动创建父目录。
func (e *SeatbeltEnvironment) WriteFile(_ context.Context, path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// buildProfile 生成 Seatbelt profile 文本（sandbox-exec -p 的内联 profile）。
//
// 结构（deny-by-default）：
//
//	(version 1)
//	(deny default)
//	(import "system.sb")      —— 放行 macOS 系统基础设施（mach service 等）
//	(allow process-exec*)     —— 允许执行任意可读二进制（bash 及其子进程）
//	(allow process-fork)
//	(allow file-read*)        —— 全盘可读（含 /usr/bin、SDK、包管理器缓存）
//	(allow sysctl-read)       —— uname/argv 等系统信息查询
//	(allow mach-lookup)       —— mDNSResponder(DNS)、日志等用户态服务
//	[workspace-write 追加]
//	(allow file-write* 可写根…)
//	(allow network*)
func (e *SeatbeltEnvironment) buildProfile() string {
	var b strings.Builder
	b.WriteString("(version 1)\n")
	b.WriteString("(deny default)\n")
	b.WriteString(`(import "system.sb")` + "\n")
	b.WriteString("(allow process-exec*)\n")
	b.WriteString("(allow process-fork)\n")
	b.WriteString("(allow file-read*)\n")
	b.WriteString("(allow sysctl-read)\n")
	b.WriteString("(allow mach-lookup)\n")

	if e.policy == SeatbeltWorkspaceWrite {
		b.WriteString("(allow file-write*\n")
		for _, root := range e.writableRoots() {
			b.WriteString(fmt.Sprintf("    (subpath %q)\n", root))
		}
		b.WriteString(")\n")
		b.WriteString("(allow network*)\n")
	}
	return b.String()
}

// writableRoots 返回去重、符号链接解析后的可写根列表。
//
// 路径解析是必须的：macOS 的 /tmp、/var 是指向 /private 下的符号链接，
// Seatbelt 按 resolve 后的真实路径匹配，未解析的路径会导致可写根静默失效。
func (e *SeatbeltEnvironment) writableRoots() []string {
	candidates := []string{"/dev", e.workDir}
	if tmp := os.TempDir(); tmp != "" {
		candidates = append(candidates, tmp)
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".harness9"))
	}
	candidates = append(candidates, e.extraWritableRoots...)

	seen := make(map[string]bool, len(candidates))
	roots := make([]string, 0, len(candidates))
	for _, c := range candidates {
		if c == "" {
			continue
		}
		resolved := resolveSubpath(c)
		if seen[resolved] {
			continue
		}
		seen[resolved] = true
		roots = append(roots, resolved)
	}
	return roots
}

// resolveSubpath 尽力解析符号链接；失败时回退原路径（EvalSymlinks 对不存在的
// 路径返回错误，但可写根本身允许尚不存在的目录）。
func resolveSubpath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	// 逐级解析已存在的祖先目录，再拼接剩余部分
	parent := filepath.Dir(path)
	if resolvedParent, err := filepath.EvalSymlinks(parent); err == nil {
		return filepath.Join(resolvedParent, filepath.Base(path))
	}
	return filepath.Clean(path)
}
