// 内置工具：Bash（Shell 命令执行工具）。
//
// 让 Agent 具备完整的命令行操作能力，是 harness9 "YOLO 哲学"（Trust-the-LLM）的核心：
// 不限制可执行命令的种类，把所有判断与决策权完全交给大模型。
//
// 与文件读写工具不同，Bash 工具默认不做命令白名单（Allow List）：
// 模型可以执行 git、go、npm、curl 等任何命令，并通过 stderr/exit code 自行判断成败。
//
// 关键安全 / 鲁棒性机制（Safety & Robustness）：
//  1. 时间预算（Time Budgeting）：通过 context 超时阻止长时间运行的命令卡死引擎。
//  2. 错误原样回传（Self-Correction Loopback）：命令失败时不返回 Go error，
//     而是把 stderr 与退出信息拼成文本输出回传给 LLM，触发自愈（Self-Healing）重试。
//  3. 长度截断保护（Length-Cap Guard）：防止巨型输出（如 ls -R /）撑爆上下文窗口。
//  4. 基础 deny-list（Dangerous Pattern Guard）：拦截 rm -rf /、fork bomb 等
//     明显破坏性的命令模式。关闭 allowDangerous 可完全禁用本层防线。
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/harness9/internal/schema"
)

// defaultMaxOutputLen 命令输出（合并 stdout + stderr）的默认最大字节数。
// 超出部分会被截断并附加提示信息，防止占满 LLM 的上下文窗口（Context Window）。
const defaultMaxOutputLen = 8000

// defaultBashHardTimeout 本工具强制施加的硬性超时上限（Hard Timeout Ceiling）。
//
// 设计动机：bash 命令可能产生长期阻塞进程（如 top、tail -f、Web 服务），
// 仅依赖父级 context 不够稳健；此处再加一层"安全网"。
// 实际生效的超时时间为 min(parent.deadline, now+hardTimeout)。
const defaultBashHardTimeout = 30 * time.Second

// dangerousPatterns 是 bash 工具在 allowDangerous=false 时拒绝执行的命令模式。
//
// 仅拦截明显的"基本没有合法用途"的破坏性操作；不做宽泛的命令黑名单，
// 保持 YOLO 哲学的核心。正则匹配在去除前导空白后的完整命令串上进行。
//
// 命中示例：
//   - `rm -rf /`、`rm -rf /*`、`rm --recursive --force /`
//   - `:(){ :|:& };:`（经典 fork bomb）
//   - `mkfs.*`、`dd if=/dev/zero of=/dev/sda ...`
//   - `curl ... | bash` / `wget ... | sh`（远程脚本直接 pipe 到 shell 执行）
//
// 如 LLM 确有合法需求（比如研究环境里想清空某个子目录），应调用方通过
// WithAllowDangerous(true) 关闭本层防线，而不是让工具悄悄放行。
var dangerousPatterns = []*regexp.Regexp{
	// rm -rf / 或 rm -rf /* 变体
	regexp.MustCompile(`\brm\s+(-[a-zA-Z]*r[a-zA-Z]*f[a-zA-Z]*|--recursive\s+--force|-rf|-fr)\b[^|&;]*\s+/\S*`),
	// fork bomb
	regexp.MustCompile(`:\s*\(\s*\)\s*\{[^}]*:\s*\|\s*:\s*&[^}]*\}\s*;\s*:`),
	// mkfs（格式化任意设备）
	regexp.MustCompile(`\bmkfs(\.[a-z0-9]+)?\s+/dev/`),
	// dd 写入 /dev/sd*/ /dev/nvme*/ /dev/hd* 等块设备（典型数据毁坏操作）
	regexp.MustCompile(`\bdd\b[^|;]*\bof=/dev/(sd[a-z]|nvme\d+n\d+|hd[a-z]|mmcblk\d+|xvd[a-z])`),
	// curl | bash / wget | sh
	regexp.MustCompile(`\b(curl|wget)\b[^|]*\|\s*(sudo\s+)?(ba)?sh\b`),
}

// BashTool 实现 BaseTool 接口，在 workDir 下以子进程方式执行任意 bash 命令。
type BashTool struct {
	// workDir 命令执行的初始工作目录（Initial CWD）。
	// 注意：bash 命令本身可以通过 `cd` 切换目录，本字段仅设置初始位置，
	// 不构成强沙箱（Sandbox）。如需路径安全请使用 read_file / write_file。
	workDir string

	// maxOutputLen 命令输出（合并 stdout + stderr）的最大字节数。
	maxOutputLen int

	// hardTimeout 单个命令执行的硬性超时上限。
	hardTimeout time.Duration

	// allowDangerous 为 true 时跳过 deny-list 校验。默认 false，
	// 即开启基础防线。仅建议在研究 / 受信任环境里显式开启。
	allowDangerous bool
}

// BashOption 是 NewBashTool 的函数选项，允许调用方覆盖默认行为。
type BashOption func(*BashTool)

// WithBashMaxOutputLen 覆盖命令输出截断上限。<= 0 会被忽略。
func WithBashMaxOutputLen(n int) BashOption {
	return func(t *BashTool) {
		if n > 0 {
			t.maxOutputLen = n
		}
	}
}

// WithBashHardTimeout 覆盖命令执行硬性超时。<= 0 会被忽略。
func WithBashHardTimeout(d time.Duration) BashOption {
	return func(t *BashTool) {
		if d > 0 {
			t.hardTimeout = d
		}
	}
}

// WithAllowDangerous 关闭 deny-list 检查。仅用于显式需要执行高危命令的场景
// （比如自动化测试里故意清理目录）。生产环境保持默认 false。
func WithAllowDangerous(allow bool) BashOption {
	return func(t *BashTool) {
		t.allowDangerous = allow
	}
}

// NewBashTool 创建绑定到指定工作目录的 Bash 工具实例，默认启用 deny-list。
func NewBashTool(workDir string, opts ...BashOption) *BashTool {
	t := &BashTool{
		workDir:        workDir,
		maxOutputLen:   defaultMaxOutputLen,
		hardTimeout:    defaultBashHardTimeout,
		allowDangerous: false,
	}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

// Name 返回工具标识符 "bash"。
func (t *BashTool) Name() string {
	return "bash"
}

// Definition 返回工具的元信息，包含描述和 JSON Schema 参数定义。
func (t *BashTool) Definition() schema.ToolDefinition {
	return schema.ToolDefinition{
		Name:        t.Name(),
		Description: "在当前工作区执行任意的 bash 命令。支持链式命令(如 &&)。返回标准输出(stdout)和标准错误(stderr)的合并内容。默认会拒绝明显破坏性的命令模式（rm -rf /、fork bomb、curl | bash 等）。",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"command": map[string]interface{}{
					"type":        "string",
					"description": "要执行的 bash 命令，例如: ls -la 或 go test ./... 等等",
				},
			},
			"required": []string{"command"},
		},
	}
}

// bashArgs 定义 bash 工具的 JSON 参数结构（Argument Payload），
// 对应 LLM 在 ToolCall 中传递的 Arguments 载荷。
type bashArgs struct {
	Command string `json:"command"`
}

// isDangerous 报告命令是否命中 deny-list。
// 匹配在去除前后空白的完整命令串上进行，支持多命令 via && / ; / | 的常见场景。
func isDangerous(cmd string) (bool, string) {
	trimmed := strings.TrimSpace(cmd)
	for _, re := range dangerousPatterns {
		if re.MatchString(trimmed) {
			return true, re.String()
		}
	}
	return false, ""
}

// Execute 执行 bash 命令。流程如下：
//  1. 反序列化 JSON 参数，提取要执行的命令字符串
//  2. deny-list 预检（除非 allowDangerous=true）
//  3. 在父级 context 之上叠加硬性超时
//  4. 通过 `bash -c` 执行命令以支持管道、重定向、&&/|| 等复杂语法
//  5. 捕获 stdout + stderr 合并输出（CombinedOutput）
//  6. 应用 Self-Correction Loopback 与 Length-Cap Guard 处理异常
//
// 重要：命令执行失败时本方法仍返回 (string, nil) 而非 (string, error)。
// 这是有意为之 — 把错误内容作为可读文本回传给 LLM，让模型自行修正命令后重试，
// 而非直接中断 agent loop。
func (t *BashTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var input bashArgs
	if err := json.Unmarshal(args, &input); err != nil {
		return "", fmt.Errorf("参数解析失败: %w", err)
	}

	if input.Command == "" {
		return "Error: 命令为空字符串", nil
	}

	// 基础 deny-list：拦截明显破坏性的命令模式。
	// 注意：命中时返回 (string, nil)，把拒绝信息作为 Observation 回传给 LLM，
	// 让模型自己调整命令，而不是直接中断循环。
	if !t.allowDangerous {
		if hit, pattern := isDangerous(input.Command); hit {
			return fmt.Sprintf(
				"Error: 命令被基础 deny-list 拦截（命中模式: %s）。"+
					"若确需执行，请拆分命令或改用更精确的写法；工具层不会放行这类高危操作。",
				pattern,
			), nil
		}
	}

	// 时间预算（Time Budgeting）：在父级 context 之上叠加硬性超时上限，
	// 防止 LLM 调用 `top` / `tail -f` / 启动 Web 服务等阻塞型命令卡死引擎。
	// 实际超时时间为 min(parent.deadline, now+hardTimeout)。
	timeoutCtx, cancel := context.WithTimeout(ctx, t.hardTimeout)
	defer cancel()

	// 通过 `bash -c` 包裹命令，支持管道(|)、逻辑与/或(&&、||)、环境变量等复杂 Shell 语法。
	cmd := exec.CommandContext(timeoutCtx, "bash", "-c", input.Command)
	cmd.Dir = t.workDir

	// CombinedOutput 同时捕获 stdout 与 stderr，模拟终端用户视角，便于 LLM 阅读。
	out, err := cmd.CombinedOutput()
	outputStr := string(out)

	// 超时分支：DeadlineExceeded 时附加显式提示，让 LLM 知道是被强制终止而非命令本身报错。
	if timeoutCtx.Err() == context.DeadlineExceeded {
		return outputStr + fmt.Sprintf("\n[警告: 命令执行超时(%s)，已被系统强制终止。]", t.hardTimeout), nil
	}

	// 错误原样回传（Self-Correction Loopback）：
	// 命令以非零退出码结束时，把 Go error（如 "exit status 1"）连同 stderr 一并回传，
	// 利用 LLM 的自纠错能力（Self-Healing）让模型自己分析报错并修正命令。
	if err != nil {
		return fmt.Sprintf("执行报错: %v\n输出:\n%s", err, outputStr), nil
	}

	if outputStr == "" {
		return "命令执行成功，无终端输出。", nil
	}

	// 长度截断保护（Length-Cap Guard）：防止超大输出撑爆 LLM 的上下文窗口。
	if len(outputStr) > t.maxOutputLen {
		return fmt.Sprintf("%s\n\n...[终端输出过长，已截断至前 %d 字节]...", outputStr[:t.maxOutputLen], t.maxOutputLen), nil
	}

	return outputStr, nil
}
