// 内置工具：ReadFile（文件读取工具）。
//
// 提供受限工作区（Sandboxed Workspace）内的安全文件读取能力，关键安全机制：
//  1. 沙箱边界（Sandbox Boundary）：所有路径通过 safePath 校验，
//     拒绝类似 "../../etc/passwd" 的路径遍历攻击（Path Traversal Attack）
//  2. 长度截断保护（Length-Cap Guard）：限制单次读取量，
//     防止超大文件占满 LLM 的上下文窗口（Context Window）导致 Token 爆炸
//  3. 分页读取（Offset / Limit）：LLM 可通过 offset（起始字节）和 limit（读取字节数）
//     显式分段读取大文件，避免"读一次看不全"的死循环
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/harness9/internal/schema"
)

// defaultMaxReadLen 单次文件读取的默认最大字节数（Default Max Read Bytes）。
// 旧版本硬编码为 4096 字节，对大多数源码文件都偏小；提高到 64 KB 后，
// 常规 .go / .md / .json 文件多数都能一次读完，减少 LLM 反复重试的成本。
// 仍然保留上限以防 `cat` 整个巨型文件撑爆上下文。
const defaultMaxReadLen = 64 * 1024

// ReadFileTool 实现了 BaseTool 接口，提供受限工作区内的安全文件读取能力。
type ReadFileTool struct {
	// workDir 工具允许访问的根目录（Sandbox Boundary，沙箱边界），
	// 所有读取操作被限制在此目录树内。
	workDir string

	// maxReadLen 单次读取上限。0 表示使用 defaultMaxReadLen。
	// 由 ReadFileOption（WithMaxReadLen）覆盖。
	maxReadLen int
}

// ReadFileOption 是 NewReadFileTool 的函数选项，允许调用方覆盖默认行为。
type ReadFileOption func(*ReadFileTool)

// WithMaxReadLen 覆盖单次读取的最大字节数。传入 <= 0 会被忽略。
func WithMaxReadLen(n int) ReadFileOption {
	return func(t *ReadFileTool) {
		if n > 0 {
			t.maxReadLen = n
		}
	}
}

// NewReadFileTool 创建绑定到指定工作区的文件读取工具。
// workDir 会被 filepath.Clean 清洗，确保路径规范化（Path Normalization）。
// 可通过 ReadFileOption（如 WithMaxReadLen）覆盖默认配置。
func NewReadFileTool(workDir string, opts ...ReadFileOption) *ReadFileTool {
	t := &ReadFileTool{
		workDir:    filepath.Clean(workDir),
		maxReadLen: defaultMaxReadLen,
	}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

// Name 返回工具标识符 "read_file"。
func (t *ReadFileTool) Name() string {
	return "read_file"
}

// Definition 返回工具的元信息，包含描述和 JSON Schema 参数定义。
// LLM 会根据此定义决定何时调用该工具以及如何构造参数。
func (t *ReadFileTool) Definition() schema.ToolDefinition {
	return schema.ToolDefinition{
		Name: t.Name(),
		Description: fmt.Sprintf(
			"读取指定路径的文件内容，请提供相对工作区的相对路径。"+
				"支持分页读取：通过 offset（起始字节，默认 0）和 limit（读取字节数，"+
				"默认 %d，最大 %d）分段访问大文件。",
			t.maxReadLen, t.maxReadLen,
		),
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path": map[string]interface{}{
					"type":        "string",
					"description": "要读取的文件路径，如 cmd/harness9/main.go",
				},
				"offset": map[string]interface{}{
					"type":        "integer",
					"description": "从文件哪个字节开始读取，默认 0",
					"minimum":     0,
				},
				"limit": map[string]interface{}{
					"type": "integer",
					"description": fmt.Sprintf(
						"最多读取多少字节，默认 %d，上限 %d。超出将被截断并提示",
						t.maxReadLen, t.maxReadLen,
					),
					"minimum": 1,
				},
			},
			"required": []string{"path"},
		},
	}
}

// readFileArgs 定义 read_file 工具的 JSON 参数结构（Argument Payload），
// 对应 LLM 在 ToolCall 中传递的 Arguments 载荷。
type readFileArgs struct {
	Path   string `json:"path"`
	Offset int64  `json:"offset"`
	Limit  int    `json:"limit"`
}

// Execute 执行文件读取操作。流程如下：
//  1. 反序列化 JSON 参数，提取目标路径、offset、limit
//  2. 通过 safePath 校验并解析为绝对路径（含沙箱边界检查）
//  3. 应用 offset（Seek）和 limit（min(limit, maxReadLen)）
//  4. 读取对应字节范围，超出 limit 时截断并附加提示
func (t *ReadFileTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var input readFileArgs
	if err := json.Unmarshal(args, &input); err != nil {
		return "", fmt.Errorf("参数解析失败: %w", err)
	}

	if input.Offset < 0 {
		return "", fmt.Errorf("offset 必须 >= 0，实际 %d", input.Offset)
	}

	// 本次读取上限：limit 未指定或超出 maxReadLen 时，限制为 maxReadLen。
	effectiveLimit := input.Limit
	if effectiveLimit <= 0 || effectiveLimit > t.maxReadLen {
		effectiveLimit = t.maxReadLen
	}

	fullPath, err := safePath(t.workDir, input.Path)
	if err != nil {
		return "", err
	}

	file, err := os.Open(fullPath)
	if err != nil {
		return "", fmt.Errorf("打开文件失败: %w", err)
	}
	defer file.Close()

	// 获取文件大小，便于给出 "已读到第 X 字节 / 共 Y 字节" 的提示。
	fi, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("读取文件信息失败: %w", err)
	}
	totalSize := fi.Size()

	// offset 超过文件大小时直接返回空（而非报错），便于 LLM 用二分之类的思路探测。
	if input.Offset >= totalSize {
		return fmt.Sprintf("[文件共 %d 字节，offset=%d 已到达或超出文件末尾，无内容可读]",
			totalSize, input.Offset), nil
	}

	if input.Offset > 0 {
		if _, err := file.Seek(input.Offset, io.SeekStart); err != nil {
			return "", fmt.Errorf("定位 offset=%d 失败: %w", input.Offset, err)
		}
	}

	// 多读 1 字节用于检测是否真的超出 limit，从而决定是否附加截断提示。
	content, err := io.ReadAll(io.LimitReader(file, int64(effectiveLimit)+1))
	if err != nil {
		return "", fmt.Errorf("读取文件内容失败: %w", err)
	}

	truncated := len(content) > effectiveLimit
	if truncated {
		content = content[:effectiveLimit]
	}

	header := ""
	if input.Offset > 0 || truncated {
		header = fmt.Sprintf("[文件共 %d 字节，本次读取 offset=%d limit=%d]\n",
			totalSize, input.Offset, effectiveLimit)
	}

	if truncated {
		nextOffset := input.Offset + int64(effectiveLimit)
		return header + string(content) + fmt.Sprintf(
			"\n\n...[内容过长，已截断至 %d 字节。如需继续，调用 offset=%d]...",
			effectiveLimit, nextOffset), nil
	}

	return header + string(content), nil
}
