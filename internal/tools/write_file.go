// 内置工具：WriteFile（文件写入工具）。
//
// 提供受限工作区（Sandboxed Workspace）内的安全文件写入能力，关键安全机制：
//  1. 沙箱边界（Sandbox Boundary）：所有路径通过 safePath 校验，
//     拒绝试图通过 "../" 等方式逃逸出工作区的路径遍历攻击（Path Traversal Attack）。
//  2. 自动建目录（Auto-Mkdir）：父级目录不存在时使用 0755 权限自动创建，
//     避免 LLM 因 ENOENT 错误而频繁重试。
//  3. 覆盖写入（Overwrite Semantics）：与 os.WriteFile 一致，目标文件已存在时直接覆盖，
//     LLM 需在 prompt 层自行判断是否应先 read_file 检查内容再写入。
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/harness9/internal/schema"
)

// WriteFileTool 实现了 BaseTool 接口，提供受限工作区内的安全文件写入能力。
type WriteFileTool struct {
	// workDir 工具允许写入的根目录（Sandbox Boundary，沙箱边界），
	// 所有写入操作被限制在此目录树内。
	workDir string
}

// NewWriteFileTool 创建绑定到指定工作区的文件写入工具。
// workDir 会被 filepath.Clean 清洗，确保路径规范化（Path Normalization）。
func NewWriteFileTool(workDir string) *WriteFileTool {
	return &WriteFileTool{workDir: filepath.Clean(workDir)}
}

// Name 返回工具标识符 "write_file"。
func (t *WriteFileTool) Name() string {
	return "write_file"
}

// Definition 返回工具的元信息，包含描述和 JSON Schema 参数定义。
// LLM 会根据此定义决定何时调用该工具以及如何构造参数。
func (t *WriteFileTool) Definition() schema.ToolDefinition {
	return schema.ToolDefinition{
		Name:        t.Name(),
		Description: "创建或覆盖写入一个文件，如果父级目录不存在会自动创建。请提供相对于工作区的相对路径。",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path": map[string]interface{}{
					"type":        "string",
					"description": "要写入的文件路径，如 src/main.go",
				},
				"content": map[string]interface{}{
					"type":        "string",
					"description": "要写入的完整文件内容",
				},
			},
			"required": []string{"path", "content"},
		},
	}
}

// writeFileArgs 定义 write_file 工具的 JSON 参数结构（Argument Payload），
// 对应 LLM 在 ToolCall 中传递的 Arguments 载荷。
type writeFileArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// Execute 执行文件写入操作。流程如下：
//  1. 反序列化 JSON 参数，提取目标路径与文件内容
//  2. 通过 safePath 校验并解析为绝对路径（含沙箱边界检查）
//  3. 自动创建父级目录（若不存在）
//  4. 以 0644 权限覆盖写入文件
func (t *WriteFileTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var input writeFileArgs
	if err := json.Unmarshal(args, &input); err != nil {
		return "", fmt.Errorf("参数解析失败: %w", err)
	}

	// 沙箱边界校验：阻止路径遍历攻击（Path Traversal Attack）。
	// 与 read_file 复用同一份 safePath 实现，保持安全策略一致。
	fullPath, err := safePath(t.workDir, input.Path)
	if err != nil {
		return "", err
	}

	// 自动创建父级目录（Auto-Mkdir），避免 LLM 因父目录缺失而反复试错。
	if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
		return "", fmt.Errorf("创建父目录失败: %w", err)
	}

	// 覆盖写入（Overwrite Semantics）：与 os.WriteFile 一致，文件已存在时直接覆盖。
	if err := os.WriteFile(fullPath, []byte(input.Content), 0644); err != nil {
		return "", fmt.Errorf("写入文件失败: %w", err)
	}

	return fmt.Sprintf("成功将 %d 字节写入到文件: %s", len(input.Content), input.Path), nil
}
