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

const maxReadLen = 4096

type ReadFileTool struct {
	workDir string
}

func NewReadFileTool(workDir string) *ReadFileTool {
	return &ReadFileTool{workDir: filepath.Clean(workDir)}
}

func (t *ReadFileTool) Name() string {
	return "read_file"
}

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

type readFileArgs struct {
	Path string `json:"path"`
}

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

	content, err := io.ReadAll(io.LimitReader(file, maxReadLen+1))
	if err != nil {
		return "", fmt.Errorf("读取文件内容失败: %w", err)
	}

	if len(content) > maxReadLen {
		return string(content[:maxReadLen]) + fmt.Sprintf("\n\n...[内容过长，已截断至前 %d 字节]...", maxReadLen), nil
	}

	return string(content), nil
}

func (t *ReadFileTool) safePath(inputPath string) (string, error) {
	cleanWorkDir := filepath.Clean(t.workDir)
	joined := filepath.Join(cleanWorkDir, inputPath)
	absPath, err := filepath.Abs(joined)
	if err != nil {
		return "", fmt.Errorf("解析路径失败: %w", err)
	}

	if !strings.HasPrefix(absPath, cleanWorkDir+string(os.PathSeparator)) && absPath != cleanWorkDir {
		return "", fmt.Errorf("路径 '%s' 超出工作区范围", inputPath)
	}

	return absPath, nil
}
