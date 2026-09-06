// Package context 实现 harness9 的上下文工程（Context Engineering）模块。
//
// DefaultPromptBuilder 按 Progressive Disclosure 原则组装 Agent 的 System Prompt：
//  1. harness9 基础 prompt（角色定义 + 工作目录声明）
//  2. workdir/AGENTS.md 内容（用户项目规范，不存在时静默跳过）
//  3. Skills 索引摘要（LLM 按需通过 use_skill 工具加载全文，空时跳过）
package context

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/harness9/internal/skills"
)

// DefaultPromptBuilder 实现了 engine.PromptBuilder 接口。
// Go 通过结构类型（Structural Typing）隐式满足接口，无需显式声明或 import engine 包。
type DefaultPromptBuilder struct {
	workDir        string
	skillsIndex    *skills.Index
	planEnabled    bool
	offloadEnabled bool
	ltmReader      func() string
	sandboxEnabled bool
}

// NewPromptBuilder 创建绑定到指定工作目录和 Skills Index 的 PromptBuilder。
// skillsIndex 为 nil 时，跳过 skills 段落注入。
func NewPromptBuilder(workDir string, idx *skills.Index) *DefaultPromptBuilder {
	return &DefaultPromptBuilder{workDir: workDir, skillsIndex: idx}
}

// WithPlanEnabled 在 system prompt 中添加规划准则（plan_write 使用指引）。
// 仅在 plan_write 已注册时调用。
func (b *DefaultPromptBuilder) WithPlanEnabled(enabled bool) *DefaultPromptBuilder {
	b.planEnabled = enabled
	return b
}

// WithOffloadEnabled 在 system prompt 中添加 context offload 检索指引。
// 仅在 OffloadHook 已配置时调用。
func (b *DefaultPromptBuilder) WithOffloadEnabled(enabled bool) *DefaultPromptBuilder {
	b.offloadEnabled = enabled
	return b
}

// WithLongTermMemory 注入长期记忆精华读取器（返回 MEMORY.md 物化视图内容）。
// reader 在每次 Build() 时被调用，确保注入的精华始终反映最新写入；返回空串时跳过整段。
// 仅在 LTM 启用时调用。
func (b *DefaultPromptBuilder) WithLongTermMemory(reader func() string) *DefaultPromptBuilder {
	b.ltmReader = reader
	return b
}

// WithSandboxContext 启用 Sandbox 执行环境提示。
// 当启用时，在 system prompt 中添加 Sandbox 特定指引，说明可用的环境能力。
func (b *DefaultPromptBuilder) WithSandboxContext(enabled bool) *DefaultPromptBuilder {
	b.sandboxEnabled = enabled
	return b
}

// Build 组装并返回完整的 System Prompt 字符串。
func (b *DefaultPromptBuilder) Build() string {
	var parts []string

	// 1. 基础 prompt
	parts = append(parts, fmt.Sprintf(
		"你的名字是 harness9 — 一个具备强大编码能力的通用 AI Agent，可完全访问用户的计算机，"+
			"能自主完成从代码开发、运行调试到系统管理的全谱任务。\n"+
			"请始终以 \"harness9\" 自称，不使用\"AI 助手\"、\"语言模型\"或任何其他通称。\n\n"+
			"核心能力：\n"+
			"- 代码开发：编写、运行、调试代码，支持 Go、Python、TypeScript、Rust、Shell 等主流语言，"+
			"端到端完成从编码到验证的全流程\n"+
			"- 系统操作：执行 Shell 命令，管理进程，安装工具和依赖包，与操作系统交互\n"+
			"- 文件操作：精确读取、写入和编辑文件，保持代码风格和约定\n"+
			"- 自主协作：将多工具串联，自主完成复杂多步骤任务\n\n"+
			"工作目录：%s\n"+
			"当前日期：%s\n\n"+
			"工作准则：\n"+
			"- 先调查后行动：优先读取相关文件、运行诊断命令，理解上下文后再修改\n"+
			"- 写代码后必须运行验证：编写或修改代码后，执行编译 / 运行 / 测试，确认实际可工作；"+
			"若工具缺失，先安装再验证\n"+
			"- 小步推进，每步验证：每次重要操作后检查结果，不堆积未验证的变更\n"+
			"- 命令失败时，诊断根本原因而非猜测，安装缺失工具后重试\n"+
			"- 优先局部修改而非整体重写；保持现有代码风格和约定\n"+
			"- 任务描述模糊时，选择最合理的解释后直接推进",
		b.workDir,
		time.Now().Format("2006-01-02"),
	))

	// 2. AGENTS.md（不存在或为空时静默跳过）
	agentsPath := filepath.Join(b.workDir, "AGENTS.md")
	if data, err := os.ReadFile(agentsPath); err == nil && len(data) > 0 {
		parts = append(parts, "## 项目规范（AGENTS.md）\n\n"+string(data))
	}

	// 3. Skills 索引（空 Index 或 nil 时跳过整块）
	if b.skillsIndex != nil && !b.skillsIndex.IsEmpty() {
		parts = append(parts,
			"## 可用 Skills\n\n"+
				"需要时使用 `use_skill` 工具加载任意 Skill 的完整内容。\n\n"+
				b.skillsIndex.Summary(),
		)
	}

	// 4. 规划准则（仅在 plan_write 已注册时注入；规划是 Agent 的原生能力，
	// 由 LLM 自主判断何时规划，无工具过滤、无 prompt 前缀、无运行时检测）
	if b.planEnabled {
		parts = append(parts,
			"## 规划（Planning）\n\n"+
				"面对复杂多步任务（多文件改动、有依赖链的步骤、需要探索后实施）时，"+
				"先用 `plan_write` 制定执行计划，再逐步执行：\n"+
				"- 计划条目必须对应具体可执行动作（创建文件、实现函数、运行命令），"+
				"禁止\"需求澄清\"、\"方案设计\"类无法直接执行的条目\n"+
				"- 开始某条目前将其标记为 in_progress，完成后立即标记为 completed\n"+
				"- 计划是权威状态：即使对话上下文被压缩，计划始终可见，以计划为准继续\n"+
				"- 简单任务（1-2 步、问答、单命令）无需规划，直接执行",
		)
	}

	// 5. Offload 检索指引（仅在 OffloadHook 启用时注入）
	if b.offloadEnabled {
		parts = append(parts,
			"## 大输出文件检索\n\n"+
				"当工具输出超过阈值时，完整内容会自动保存到文件系统，context 中仅显示路径引用和预览。\n"+
				"如需查看完整输出，使用 read_file 工具并指定 offset/limit 参数分页读取：\n"+
				"- offset：起始字节位置（默认 0）\n"+
				"- limit：读取字节数（默认 8192，即 read_file 工具的 maxReadLen）\n"+
				"示例：read_file({\"path\": \".harness9/tool_results/{sessionID}/{toolCallID}.txt\", \"offset\": 8192, \"limit\": 8192})",
		)
	}

	// 5b. Sandbox 执行环境提示（仅在启用 Sandbox 时注入，与 Offload 提示平级）
	if b.sandboxEnabled {
		parts = append(parts,
			"## Sandbox 执行环境\n\n"+
				"你当前在一个隔离的 Docker 容器（Ubuntu 22.04）内执行所有工具调用：\n"+
				"- 这是与宿主机完全隔离的临时环境，容器内的任何操作都不会影响用户的真实系统\n"+
				"- 容器有完整的网络访问权限，可访问公网\n"+
				"- 你拥有容器内的完整权限（root）\n"+
				"- 缺少运行时或工具时（如 Go、Node.js、Python、Rust、gcc 等），"+
				"直接使用 apt-get / wget / curl 安装，无需请示用户\n"+
				"- 安装的软件在本次会话期间持续有效\n"+
				"- 代码验证（编译、运行、测试）是开发任务的必要步骤，不得跳过；"+
				"遇到工具缺失时，先安装后验证",
		)
	}

	// 6. 长期记忆精华（每次 Build 时调用 ltmReader 读取最新内容；reader 为 nil 或返回空时跳过）
	if b.ltmReader != nil {
		if content := b.ltmReader(); content != "" {
			parts = append(parts,
				"## 长期记忆\n\n"+
					"以下是跨会话积累的长期记忆精华。需要更多历史细节时，使用 `memory_search` 工具检索；"+
					"发现值得长期保留的新信息时，使用 `memory_write` 工具记录。\n\n"+
					content,
			)
		}
	}

	return strings.Join(parts, "\n\n")
}
