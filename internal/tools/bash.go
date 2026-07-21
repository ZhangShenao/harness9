// 内置工具：Bash（Shell 命令执行工具）。
//
// 让 Agent 具备完整的命令行操作能力，是 harness9 "YOLO 哲学"（Trust-the-LLM）的核心：
// 不限制可执行命令的种类，把所有判断与决策权完全交给大模型。
//
// 注入 sandbox.Environment 后，命令通过 docker exec 在容器内执行（OS 级隔离）；
// 未注入时（env=nil）走原有本地进程路径，行为与引入 Sandbox 前完全一致。
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"time"
	"unicode/utf8"

	"github.com/harness9/internal/sandbox"
	"github.com/harness9/internal/schema"
)

const maxOutputLen = 16000

// defaultBashTimeout 是单条 bash 命令的默认超时。
// 之前硬编码为 30s，对 SWE-bench 等场景过短：pip install / 项目测试套件
// 普遍超过 30s 而被强杀，使 Agent 无法真正验证修复。提高到 120s 作为更合理的默认值，
// 并允许通过 WithBashTimeout 覆盖、或由 LLM 在单次调用中通过 timeout_secs 临时放宽。
const defaultBashTimeout = 120 * time.Second

// maxBashTimeout 是单次调用 timeout_secs 可请求的上限，防止 Agent 设置过长超时拖死整体预算。
const maxBashTimeout = 600 * time.Second

// BashTool 实现 BaseTool 接口，在 workDir 下执行任意 bash 命令。
type BashTool struct {
	workDir string
	env     sandbox.Environment // nil = 本地执行；非 nil = 路由进 Sandbox 容器
	timeout time.Duration       // 单条命令超时；0 时使用 defaultBashTimeout
}

// BashOption 是 BashTool 的功能选项函数。
type BashOption func(*BashTool)

// WithEnvironment 注入 sandbox.Environment，命令将路由到容器内执行。
// env 为 nil 时无效（等同于不注入）。
func WithEnvironment(env sandbox.Environment) BashOption {
	return func(t *BashTool) { t.env = env }
}

// WithBashTimeout 设置单条 bash 命令的默认超时（覆盖 defaultBashTimeout）。
// 用于 SWE-bench 等需要运行较慢测试套件/安装命令的场景（如 300s）。
// d <= 0 时忽略，沿用默认值。
func WithBashTimeout(d time.Duration) BashOption {
	return func(t *BashTool) {
		if d > 0 {
			t.timeout = d
		}
	}
}

// NewBashTool 创建绑定到指定工作目录的 Bash 工具实例。
func NewBashTool(workDir string, opts ...BashOption) *BashTool {
	t := &BashTool{workDir: workDir}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

// effectiveTimeout 返回本次执行采用的超时：单次 timeout_secs（钳制到 maxBashTimeout）优先，
// 否则用工具配置的 timeout，最后回退到 defaultBashTimeout。
func (t *BashTool) effectiveTimeout(perCallSecs int) time.Duration {
	if perCallSecs > 0 {
		d := time.Duration(perCallSecs) * time.Second
		if d > maxBashTimeout {
			d = maxBashTimeout
		}
		return d
	}
	if t.timeout > 0 {
		return t.timeout
	}
	return defaultBashTimeout
}

func (t *BashTool) Name() string { return "bash" }

func (t *BashTool) Definition() schema.ToolDefinition {
	return schema.ToolDefinition{
		Name:        t.Name(),
		Description: "在当前工作区执行任意的 bash 命令。支持链式命令(如 &&)。返回标准输出(stdout)和标准错误(stderr)的合并内容。",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"command": map[string]interface{}{
					"type":        "string",
					"description": "要执行的 bash 命令，例如: ls -la 或 go test ./... 等等",
				},
				"timeout_secs": map[string]interface{}{
					"type":        "integer",
					"description": fmt.Sprintf("可选：本次命令的超时秒数（默认 %d，上限 %d）。运行较慢的测试套件或安装命令时可适当调大。", int(defaultBashTimeout.Seconds()), int(maxBashTimeout.Seconds())),
				},
			},
			"required": []string{"command"},
		},
	}
}

type bashArgs struct {
	Command     string `json:"command"`
	TimeoutSecs int    `json:"timeout_secs,omitempty"`
}

func (t *BashTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var input bashArgs
	if err := json.Unmarshal(args, &input); err != nil {
		return "", fmt.Errorf("参数解析失败: %w", err)
	}
	if input.Command == "" {
		return "Error: 命令为空字符串", nil
	}

	timeout := t.effectiveTimeout(input.TimeoutSecs)
	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if t.env != nil {
		return t.runInSandbox(timeoutCtx, input.Command, timeout)
	}
	return t.runLocal(timeoutCtx, input.Command, timeout)
}

// runInSandbox 通过注入的 Environment 在容器内执行命令。
//
// 超时处理说明：DockerEnvironment.RunBash 内部将 docker exec 的失败（含 ctx 取消/超时）
// 转换为错误字符串返回（err==nil）。这里显式检查 ctx 是否因超时取消，
// 追加一条明确的超时横幅，使 LLM 能区分"命令本身报错"与"被 harness 因超时强杀"，
// 从而采取不同策略（运行单个测试 / 不要据此误改源码）。
func (t *BashTool) runInSandbox(ctx context.Context, cmd string, timeout time.Duration) (string, error) {
	out, err := t.env.RunBash(ctx, cmd, t.workDir)
	if ctx.Err() == context.DeadlineExceeded {
		return truncateOutput(out) + timeoutBanner(timeout), nil
	}
	if err != nil {
		return fmt.Sprintf("执行报错: %v", err), nil
	}
	if out == "" {
		return "命令执行成功，无终端输出。", nil
	}
	return truncateOutput(out), nil
}

// runLocal 在本地进程中执行命令（Sandbox 关闭时的原有路径）。
//
// 不用 CombinedOutput()：其内部经 pipe + 拷贝 goroutine 承接输出，Wait() 会等待所有
// fd 持有者关闭——当 cmd 是 "A && B &" 这类复合后台任务，bash 为整个 "&&" 链表 fork 的
// 子 shell 继承了该 pipe 的写端，只要后台进程还活着就永久阻塞，直到外层超时才被打断
// （详见 docs/技术调研/terminal-bench-轨迹分析-v1.md §1）。改用临时文件承接输出：
// Go 对 *os.File 类型的 Stdout/Stderr 走原始 fd 直传，不经 pipe/拷贝 goroutine，
// Wait() 只等待直接子进程（bash -c 本身）退出，不受后台化进程影响，也不会误杀它
// （已实测验证：返回时间从挂起 20s 降到 ~5ms，且后台进程验证仍在运行）。
func (t *BashTool) runLocal(ctx context.Context, cmd string, timeout time.Duration) (string, error) {
	tmp, err := os.CreateTemp("", "harness9-bash-*.log")
	if err != nil {
		return "", fmt.Errorf("创建临时输出文件失败: %w", err)
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	c := exec.CommandContext(ctx, "bash", "-c", cmd)
	c.Dir = t.workDir
	c.Stdout = tmp
	c.Stderr = tmp
	runErr := c.Run()

	out, readErr := os.ReadFile(tmp.Name())
	if readErr != nil {
		return "", fmt.Errorf("读取命令输出失败: %w", readErr)
	}
	outputStr := string(out)

	if ctx.Err() == context.DeadlineExceeded {
		// 先截断，再追加警告，避免警告被截断掉
		return truncateOutput(outputStr) + timeoutBanner(timeout), nil
	}
	if runErr != nil {
		return fmt.Sprintf("执行报错: %v\n输出:\n%s", runErr, truncateOutput(outputStr)), nil
	}
	if outputStr == "" {
		return "命令执行成功，无终端输出。", nil
	}
	return truncateOutput(outputStr), nil
}

// timeoutBanner 返回一条清晰、机器可读的超时提示，供 LLM 调整策略。
func timeoutBanner(timeout time.Duration) string {
	return fmt.Sprintf("\n\n[TIMEOUT %s: 命令超时被强制终止，这不是代码错误。"+
		"如在跑测试/安装：尝试只跑单个测试、或用 timeout_secs 调大超时。]", timeout)
}

// truncateOutput 在输出超过 maxOutputLen 时同时保留头部与尾部（UTF-8 安全）。
//
// 关键设计：测试运行器（pytest / go test 等）的 verbose 进度在前、而最关键的
// FAILED/AssertionError/traceback 与 "=== N failed ===" 汇总行在最后。
// 旧实现只保留头部、丢弃尾部，恰好把诊断信息切掉。这里保留 head + tail，
// 中间用标记省略，并在 rune 边界回退，避免切碎多字节字符破坏 JSON/OTLP 序列化。
func truncateOutput(s string) string {
	if len(s) <= maxOutputLen {
		return s
	}
	// 头部约 1/3、尾部约 2/3（尾部信息更关键）。
	headLen := maxOutputLen / 3
	tailLen := maxOutputLen - headLen
	head := trimToValidUTF8Suffix(s[:headLen])
	tail := trimToValidUTF8Prefix(s[len(s)-tailLen:])
	elided := len(s) - len(head) - len(tail)
	return fmt.Sprintf("%s\n\n...[输出过长，中间 %d 字节已截断，保留首尾；如需完整内容请缩小命令范围]...\n\n%s", head, elided, tail)
}

// trimToValidUTF8Suffix 去掉字符串末尾可能被切碎的不完整 UTF-8 字节。
func trimToValidUTF8Suffix(s string) string {
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// trimToValidUTF8Prefix 去掉字符串开头可能被切碎的不完整 UTF-8 字节。
func trimToValidUTF8Prefix(s string) string {
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[1:]
	}
	return s
}
