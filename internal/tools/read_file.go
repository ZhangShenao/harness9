// 内置工具：ReadFile — 安全的文件读取工具。
// 提供 Sandbox 沙箱路径限制，防止路径遍历攻击（Path Traversal），
// 并限制最大读取长度以保护系统资源。
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/harness9/internal/schema"
)

// maxReadLen 单次文件读取的最大字节数。超出部分会被截断并附加提示信息，
// 防止意外将超大文件内容注入到 LLM 上下文窗口中导致 Token 爆炸。
const maxReadLen = 4096

// ReadFileTool 实现了 BaseTool 接口，提供受限工作区内的安全文件读取能力。
// 安全机制：所有路径解析后会校验是否在工作区（WorkDir）范围内，
// 拒绝类似 "../../etc/passwd" 的路径遍历攻击（Path Traversal Attack）。
type ReadFileTool struct {
	// workDir 工具允许访问的根目录（Sandbox Boundary），
	// 所有读取操作被限制在此目录树内。
	workDir string
}

// NewReadFileTool 创建绑定到指定工作区的文件读取工具。
// workDir 会被 filepath.Clean 清洗，确保路径规范化。
func NewReadFileTool(workDir string) *ReadFileTool {
	return &ReadFileTool{workDir: filepath.Clean(workDir)}
}

// Name 返回工具标识符 "read_file"。
func (t *ReadFileTool) Name() string {
	return "read_file"
}

// Definition 返回工具的元信息，包含描述和 JSON Schema 参数定义。
// LLM 会根据此定义决定何时调用该工具以及如何构造参数。
func (t *ReadFileTool) Definition() schema.ToolDefinition {
	return schema.ToolDefinition{
		Name:        t.Name(),
		Description: "读取指定路径的文件内容。请提供相对工作区的路径。",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path": map[string]interface{}{
					"type":        "string",
					"description": "要读取的文件路径，如 cmd/harness9/main.go",
				},
			},
			"required": []string{"path"},
		},
	}
}

// readFileArgs 定义 read_file 工具的 JSON 参数结构，
// 对应 LLM 在 ToolCall 中传递的 Arguments 载荷（Payload）。
type readFileArgs struct {
	Path string `json:"path"`
}

// Execute 执行文件读取操作。流程如下：
//  1. 反序列化 JSON 参数，提取目标路径
//  2. 通过 safePath 校验并解析为绝对路径（含沙箱边界检查）
//  3. 读取文件内容，超过 maxReadLen 的部分被截断
func (t *ReadFileTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var input readFileArgs
	if err := json.Unmarshal(args, &input); err != nil {
		return "", fmt.Errorf("参数解析失败: %w", err)
	}

	fullPath, err := t.safePath(input.Path)
	if err != nil {
		return "", err
	}

	file, err := os.Open(fullPath)
	if err != nil {
		return "", fmt.Errorf("打开文件失败: %w", err)
	}
	defer file.Close()

	// 使用 LimitReader 限制读取量，+1 用于检测是否超出上限
	content, err := io.ReadAll(io.LimitReader(file, maxReadLen+1))
	if err != nil {
		return "", fmt.Errorf("读取文件内容失败: %w", err)
	}

	if len(content) > maxReadLen {
		return string(content[:maxReadLen]) + fmt.Sprintf("\n\n...[内容过长，已截断至前 %d 字节]...", maxReadLen), nil
	}

	return string(content), nil
}

// safePath 将用户输入的相对路径解析为绝对路径，并校验是否在工作区范围内。
// 这是防止路径遍历攻击（Path Traversal Attack）的核心安全屏障：
// 任何试图通过 "../" 逃逸工作区的路径都会被拒绝。
//
// 例如：workDir="/project"，输入 "../../etc/passwd" 会被拒绝，
// 输入 "src/main.go" 会被解析为 "/project/src/main.go"。
func (t *ReadFileTool) safePath(inputPath string) (string, error) {
	cleanWorkDir := filepath.Clean(t.workDir)
	joined := filepath.Join(cleanWorkDir, inputPath)
	absPath, err := filepath.Abs(joined)
	if err != nil {
		return "", fmt.Errorf("解析路径失败: %w", err)
	}

	// 安全校验：确保解析后的绝对路径仍以工作区目录为前缀
	if !strings.HasPrefix(absPath, cleanWorkDir+string(os.PathSeparator)) && absPath != cleanWorkDir {
		return "", fmt.Errorf("路径 '%s' 超出工作区范围", inputPath)
	}

	return absPath, nil
}
