// 内置工具：Bash（Shell 命令执行工具）。
//
// 让 Agent 具备完整的命令行操作能力。默认行为仍然是"Trust-the-LLM / YOLO"
// 哲学的延续 —— 不做命令白名单，把大部分判断权交给大模型 —— 但在此基础上
// 增加了一层"基础 deny-list"防线，专门拦截几类"基本没有合法用途"的破坏性
// 模式（rm -rf 系统路径 / fork bomb / mkfs / dd 写块设备 / curl | bash）。
//
// 默认启用 deny-list；调用 NewBashTool(workDir, WithAllowDangerous(true))
// 可以在研究/受信任环境里显式关闭本层防线。
//
// deny-list 采用 fail-closed 语义：
//   - 命令 tokenizer 解析失败（未闭合引号等）→ 直接拦截，不放行
//   - 命中规则 → 拦截，并把人类可读的规则名回给 LLM 作为 Observation
//   - 命中 / 拦截都返回 (string, nil) 而不是 error；agent loop 正常继续，
//     让 LLM 基于拦截信息调整命令（Self-Healing）
//
// 关键安全 / 鲁棒性机制（Safety & Robustness）：
//  1. 时间预算（Time Budgeting）：通过 context 超时阻止长时间运行的命令卡死引擎。
//  2. 错误原样回传（Self-Correction Loopback）：命令失败时不返回 Go error，
//     而是把 stderr 与退出信息拼成文本输出回传给 LLM，触发自愈（Self-Healing）重试。
//  3. 长度截断保护（Length-Cap Guard）：防止巨型输出（如 ls -R /）撑爆上下文窗口。
//  4. 基础 deny-list（Dangerous Pattern Guard）：拦 rm -rf、fork bomb、mkfs、
//     dd 块设备、curl|bash 等模式。命中时作为 Observation 回给 LLM。
//
// 已知限制（Known Limitations）：
//   - deny-list 仅做防"误操作 / LLM 冒失"的基础屏障，不是对抗性 sandbox。
//     攻击者可通过 base64 解码、$() 子 shell、动态命令构造等方式绕过。
//     强沙箱需求请在容器 / Jail / seccomp 等更外层解决。
//   - 手写 tokenizer 不展开变量（`$HOME` 作字面量处理）、不解析子 shell 内容、
//     不处理 heredoc。LLM 的常规输出场景这些都够用。
//   - `sh -c "..."` / `bash -c "..."` 内嵌的字符串参数不做递归解析：tokenize
//     后看到的是 [sh, -c, "内嵌命令"]，内嵌命令作为一整个字符串 token 传给
//     isDangerousRmArgv 时不会命中 rm 本体。和 $() / heredoc 是同一等级的
//     "LLM 有心就能绕"限制，需要强隔离请走容器层。
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

// ============================================================================
// 命令 Tokenizer（Hand-Rolled Lexer）
// ============================================================================
//
// 手撸一个最小 shell 词法分析器：
//   - 按空白（space/tab/newline）切词
//   - 识别 `&&` / `||` / `|` / `;` / `&` 为独立的"操作符 token"
//   - 单引号内按字面量收（不处理反斜杠转义）
//   - 双引号内支持 `\` 对 `"` / `\` 的转义
//   - 外部的 `\` 直接转义下一个字符
//
// 不支持：`$VAR` 展开、`$(...)` 子 shell、heredoc、重定向（> < 等会作为字面量
// 并入相邻 token）。对 LLM 常规命令场景足够；deny-list 的判断在命中边界上选择
// fail-closed，未闭合引号 / 异常结构直接返回 error。

// tokenize 是一个小型 shell 词法分析器，用于 deny-list 判定。
// 返回的 token 列表中，操作符（&&/||/|/;/&）作为独立 token 出现，
// 其他 token 是已解引号的字面量。遇到未闭合引号返回 error（fail-closed 依据）。
func tokenize(s string) ([]string, error) {
	var tokens []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			tokens = append(tokens, cur.String())
			cur.Reset()
		}
	}
	pushOp := func(op string) {
		flush()
		tokens = append(tokens, op)
	}

	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			flush()
			i++

		case c == '\\':
			if i+1 >= len(s) {
				return nil, fmt.Errorf("命令以裸反斜杠结尾")
			}
			cur.WriteByte(s[i+1])
			i += 2

		case c == '\'':
			end := strings.IndexByte(s[i+1:], '\'')
			if end < 0 {
				return nil, fmt.Errorf("未闭合的单引号")
			}
			cur.WriteString(s[i+1 : i+1+end])
			i = i + 1 + end + 1

		case c == '"':
			j := i + 1
			for j < len(s) {
				if s[j] == '\\' && j+1 < len(s) {
					switch s[j+1] {
					case '"', '\\', '$', '`':
						cur.WriteByte(s[j+1])
					default:
						cur.WriteByte(s[j])
						cur.WriteByte(s[j+1])
					}
					j += 2
					continue
				}
				if s[j] == '"' {
					break
				}
				cur.WriteByte(s[j])
				j++
			}
			if j >= len(s) {
				return nil, fmt.Errorf("未闭合的双引号")
			}
			i = j + 1

		case c == '&' && i+1 < len(s) && s[i+1] == '&':
			pushOp("&&")
			i += 2
		case c == '|' && i+1 < len(s) && s[i+1] == '|':
			pushOp("||")
			i += 2
		case c == '|':
			pushOp("|")
			i++
		case c == ';':
			pushOp(";")
			i++
		case c == '&':
			pushOp("&")
			i++

		default:
			cur.WriteByte(c)
			i++
		}
	}
	flush()
	return tokens, nil
}

// operatorToken 判断一个 token 是否是命令分隔操作符。
func operatorToken(t string) bool {
	switch t {
	case "&&", "||", "|", ";", "&":
		return true
	}
	return false
}

// splitArgvOnOperators 把扁平 token 列表按操作符拆成若干子命令 argv。
func splitArgvOnOperators(tokens []string) [][]string {
	var out [][]string
	var cur []string
	for _, t := range tokens {
		if operatorToken(t) {
			if len(cur) > 0 {
				out = append(out, cur)
				cur = nil
			}
			continue
		}
		cur = append(cur, t)
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

// ============================================================================
// Deny-list 规则
// ============================================================================

// denyRule 表示一条 deny-list 规则。label 面向 LLM 可读（命中时回显）。
type denyRule struct {
	label   string
	matchFn func(string) bool
}

var (
	forkBombRe      = regexp.MustCompile(`:\s*\(\s*\)\s*\{[^}]*:\s*\|\s*:\s*&[^}]*\}\s*;\s*:`)
	mkfsRe          = regexp.MustCompile(`\bmkfs(\.[a-z0-9]+)?\s+/dev/`)
	ddBlockDevRe    = regexp.MustCompile(`\bdd\b[^|;]*\bof=/dev/(sd[a-z]|nvme\d+n\d+|hd[a-z]|mmcblk\d+|xvd[a-z])`)
	remoteToShellRe = regexp.MustCompile(`\b(curl|wget)\b[^|]*\|\s*(sudo\s+)?(ba)?sh\b`)
)

// rawRegexRules 在原始命令字符串上生效的规则（对引号/操作符无强依赖）。
var rawRegexRules = []denyRule{
	{label: "fork bomb", matchFn: regexMatcher(forkBombRe)},
	{label: "mkfs 格式化设备", matchFn: regexMatcher(mkfsRe)},
	{label: "dd 写入块设备", matchFn: regexMatcher(ddBlockDevRe)},
	{label: "远程脚本直接 pipe 到 shell 执行", matchFn: regexMatcher(remoteToShellRe)},
}

func regexMatcher(re *regexp.Regexp) func(string) bool {
	return func(s string) bool { return re.MatchString(s) }
}

// rmCommandPrefixes 是 rm 前允许的包装命令：检测时跳过它们去找真正的命令。
var rmCommandPrefixes = map[string]bool{
	"sudo": true, "doas": true, "exec": true, "time": true,
	"command": true, "nohup": true, "stdbuf": true, "ionice": true, "nice": true,
}

// dangerousRmTops 是 rm 目标命中后视为危险的 / 下顶级目录。
var dangerousRmTops = map[string]bool{
	"bin": true, "boot": true, "dev": true, "etc": true,
	"home": true, "lib": true, "lib64": true, "opt": true,
	"proc": true, "root": true, "run": true, "sbin": true,
	"srv": true, "sys": true, "usr": true, "var": true,
}

// isRmBinary 识别 rm 本体（含常见绝对路径形式）。
func isRmBinary(tok string) bool {
	return tok == "rm" || tok == "/bin/rm" || tok == "/usr/bin/rm"
}

// isDangerousRmArgv 判断一个子命令 argv 是否是"带 -rf（或等价）且作用在危险目标
// 上"的 rm。覆盖的写法见包注释顶部。
//
// 不拦截：
//   - 不带 -r 或不带 -f：`rm /some/file`（rm 对非空目录默认拒绝）
//   - 目标看起来是 workdir 内的相对子路径（工具层没有 workdir 感知，交给调用方
//     约束；本函数只拦"路径形状本身就明显危险"的情况）
func isDangerousRmArgv(argv []string) bool {
	if len(argv) == 0 {
		return false
	}

	// 跳过 sudo / exec / env KEY=VAL 等前缀，找到真正的命令。
	i := 0
	for i < len(argv) {
		t := argv[i]
		if rmCommandPrefixes[t] {
			i++
			continue
		}
		if t == "env" {
			i++
			for i < len(argv) && strings.Contains(argv[i], "=") && !strings.HasPrefix(argv[i], "-") {
				i++
			}
			continue
		}
		break
	}
	if i >= len(argv) || !isRmBinary(argv[i]) {
		return false
	}

	args := argv[i+1:]
	hasR, hasF := false, false
	var targets []string
	stopOpts := false
	for _, a := range args {
		if !stopOpts && a == "--" {
			stopOpts = true
			continue
		}
		if !stopOpts && strings.HasPrefix(a, "--") {
			switch a {
			case "--recursive":
				hasR = true
			case "--force":
				hasF = true
			}
			continue
		}
		if !stopOpts && strings.HasPrefix(a, "-") && len(a) > 1 {
			for _, c := range a[1:] {
				switch c {
				case 'r', 'R':
					hasR = true
				case 'f', 'F':
					hasF = true
				}
			}
			continue
		}
		targets = append(targets, a)
	}

	if !(hasR && hasF) {
		return false
	}

	for _, t := range targets {
		if isDangerousRmTarget(t) {
			return true
		}
	}
	return false
}

// isDangerousRmTarget 判断 rm 的目标参数是否危险。
//
// 判断维度（字面量层面，不做变量展开）：
//  1. 根 / 根通配 / 根下系统顶级目录（/, /*, /., /.., /etc, /usr, ...）
//  2. 家目录 / $HOME（~, ~/, $HOME, $HOME/, ${HOME}, ${HOME}/）
//  3. 相对路径上爬（.. 或以 ../ 开头的路径；内部的 /../ 不算 —— 比如
//     `./foo/../bar` 会被 rm 解析成 workdir 内部的相对路径，不拦截）
//  4. 当前目录全量清空（. 或 *）
func isDangerousRmTarget(t string) bool {
	if t == "" {
		return true
	}

	// --- 1. 绝对路径危险形态 ---
	switch t {
	case "/", "/.", "/..", "/*", "/.*":
		return true
	}
	if strings.HasPrefix(t, "/") {
		rest := strings.TrimPrefix(t, "/")
		rest = strings.TrimRight(rest, "/")
		if rest == "" {
			return true
		}
		top := strings.SplitN(rest, "/", 2)[0]
		top = strings.TrimRight(top, "*")
		if dangerousRmTops[top] {
			return true
		}
	}

	// --- 2. 家目录及其子路径 ---
	if t == "~" || strings.HasPrefix(t, "~/") ||
		t == "$HOME" || strings.HasPrefix(t, "$HOME/") ||
		t == "${HOME}" || strings.HasPrefix(t, "${HOME}/") {
		return true
	}

	// --- 3. 相对路径上爬 ---
	// 只拦"目标以 .. 或 ../ 开头"的写法；内部的 /../ 不算危险，
	// `rm -rf ./foo/../bar` 在 workdir 内部解析，并不会逃出工作区。
	if t == ".." || strings.HasPrefix(t, "../") {
		return true
	}

	// --- 4. 当前目录全量清空 ---
	if t == "." || t == "*" {
		return true
	}

	return false
}

// checkDenyList 在命令字符串上应用所有 deny 规则。
//
// 流程（fail-closed）：
//  1. 原始字符串正则规则（fork bomb / mkfs / dd / curl|bash）
//  2. tokenize + 切分子命令 argv。tokenize 失败 → 拦截
//  3. 每个子命令跑 isDangerousRmArgv
//
// 返回 (hit, label)。label 是人类可读的规则名称，用于回给 LLM。
func checkDenyList(cmd string) (bool, string) {
	for _, rule := range rawRegexRules {
		if rule.matchFn(cmd) {
			return true, rule.label
		}
	}

	tokens, err := tokenize(cmd)
	if err != nil {
		return true, fmt.Sprintf("命令 shell 解析失败，fail-closed 拦截（原因: %v）", err)
	}

	for _, argv := range splitArgvOnOperators(tokens) {
		if isDangerousRmArgv(argv) {
			return true, "rm -rf 删除系统/家目录/上级路径"
		}
	}

	return false, ""
}

// ============================================================================
// BashTool
// ============================================================================

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
// （比如自动化测试里故意清理目录，或研究环境里的一次性操作）。生产环境保持
// 默认 false。
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
		Description: "在当前工作区执行任意的 bash 命令。支持链式命令(如 &&)。返回标准输出(stdout)和标准错误(stderr)的合并内容。默认会拒绝几类明显破坏性的模式：rm -rf 系统路径/家目录/上级路径、fork bomb、mkfs、dd 写块设备、curl|bash 远程执行。命令无法安全解析（如未闭合引号）会被 fail-closed 拦截。",
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
	// 命中时返回 (string, nil)，把拒绝信息作为 Observation 回传给 LLM，
	// 让模型自己调整命令，而不是直接中断循环。
	if !t.allowDangerous {
		if hit, label := checkDenyList(input.Command); hit {
			return fmt.Sprintf(
				"Error: 命令被基础 deny-list 拦截（规则: %s）。"+
					"若确需执行，请拆分命令或改用更精确的写法；工具层默认不会放行这类高危操作。",
				label,
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
