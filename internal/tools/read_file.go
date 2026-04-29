// 内置工具：ReadFile（文件读取工具）。
//
// 提供受限工作区（Sandboxed Workspace）内的安全文件读取能力，关键安全机制：
//  1. 沙箱边界（Sandbox Boundary）：所有路径通过 safePath 校验，
//     拒绝类似 "../../etc/passwd" 的路径遍历攻击（Path Traversal Attack）
//  2. 长度截断保护（Length-Cap Guard）：使用 io.LimitReader 限制单次读取量，
//     防止超大文件占满 LLM 的上下文窗口（Context Window）导致 Token 爆炸
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

// maxReadLen 单次文件读取的最大字节数（Max Read Bytes）。
// 超出部分会被截断并附加提示信息，避免无意中将超大文件内容注入到 LLM 上下文窗口。
const maxReadLen = 4096

// ReadFileTool 实现了 BaseTool 接口，提供受限工作区内的安全文件读取能力。
type ReadFileTool struct {
	// workDir 工具允许访问的根目录（Sandbox Boundary，沙箱边界），
	// 所有读取操作被限制在此目录树内。
	workDir string
}

// NewReadFileTool 创建绑定到指定工作区的文件读取工具。
// workDir 会被 filepath.Clean 清洗，确保路径规范化（Path Normalization）。
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
		Description: "读取指定路径的文件内容。请提供相对工作区的相对路径。",
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

// readFileArgs 定义 read_file 工具的 JSON 参数结构（Argument Payload），
// 对应 LLM 在 ToolCall 中传递的 Arguments 载荷。
type readFileArgs struct {
	Path string `json:"path"`
}

// Execute 执行文件读取操作。流程如下：
//  1. 反序列化 JSON 参数，提取目标路径
//  2. 通过 safePath 校验并解析为绝对路径（含沙箱边界检查）
//  3. 读取文件内容，超过 maxReadLen 的部分被截断并附加提示
func (t *ReadFileTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var input readFileArgs
	if err := json.Unmarshal(args, &input); err != nil {
		return "", fmt.Errorf("参数解析失败: %w", err)
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

	// 使用 LimitReader 限制读取量；多读 1 字节用于检测是否真的超出上限。
	content, err := io.ReadAll(io.LimitReader(file, maxReadLen+1))
	if err != nil {
		return "", fmt.Errorf("读取文件内容失败: %w", err)
	}

	if len(content) > maxReadLen {
		return string(content[:maxReadLen]) + fmt.Sprintf("\n\n...[内容过长，已截断至前 %d 字节]...", maxReadLen), nil
	}

	return string(content), nil
}
