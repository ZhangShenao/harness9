// 内置工具：ReadFile（文件读取工具）。
//
// 提供受限工作区（Sandboxed Workspace）内的安全文件读取能力，关键安全机制：
//  1. 沙箱边界（Sandbox Boundary）：所有路径通过 safePath 校验，
//     拒绝类似 "../../etc/passwd" 的路径遍历攻击（Path Traversal Attack）
//  2. 长度截断保护（Length-Cap Guard）：使用 io.LimitReader 限制单次读取量，
//     防止超大文件占满 LLM 的上下文窗口（Context Window）导致 Token 爆炸
//  3. 分页支持：offset（字节偏移）+ limit（读取字节数）可选参数，
//     配合 OffloadHook 实现超大 offload 文件的分段检索
package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/harness9/internal/sandbox"
	"github.com/harness9/internal/schema"
)

// maxReadLen 单次文件读取的最大字节数（Max Read Bytes）。
// 未指定 limit 参数时的默认上限。SWE-bench 源文件普遍较大，4096 字节（约 60-80 行）
// 会造成频繁分页（每次一个 LLM turn），故提高到 8192。
const maxReadLen = 8192

// ReadFileTool 实现了 BaseTool 接口，提供受限工作区内的安全文件读取能力。
type ReadFileTool struct {
	workDir string
	// TODO: 当需要将文件操作路由至容器内执行时，接入 env.ReadFile/WriteFile。
	// 目前文件操作通过 bind mount 在宿主机侧执行，与容器内视图一致，无需路由。
	env sandbox.Environment
}

// ReadFileOption 是 ReadFileTool 的功能选项函数。
type ReadFileOption func(*ReadFileTool)

// ReadFileWithEnvironment 注入执行环境（当前文件工具通过 bind mount 无需路由，预留扩展）。
func ReadFileWithEnvironment(env sandbox.Environment) ReadFileOption {
	return func(t *ReadFileTool) { t.env = env }
}

// NewReadFileTool 创建绑定到指定工作区的文件读取工具。
func NewReadFileTool(workDir string, opts ...ReadFileOption) *ReadFileTool {
	t := &ReadFileTool{workDir: filepath.Clean(workDir)}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

// Name 返回工具标识符 "read_file"。
func (t *ReadFileTool) Name() string { return "read_file" }

// Definition 返回工具元信息，包含描述和 JSON Schema 参数定义。
func (t *ReadFileTool) Definition() schema.ToolDefinition {
	return schema.ToolDefinition{
		Name: t.Name(),
		Description: "读取指定路径的文件内容。" +
			"推荐使用 start_line/end_line 按行号读取片段（最直观）：返回的每行带 `行号→Tab→内容` 前缀，便于定位。" +
			"⚠️ 行号前缀仅供显示，调用 edit_file 时 source_text 必须是不含行号前缀的原始代码。" +
			"也支持 offset/limit 字节偏移分页（单位为字节，不是行号）。",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path": map[string]interface{}{
					"type":        "string",
					"description": "要读取的文件路径，如 src/main.py",
				},
				"start_line": map[string]interface{}{
					"type":        "integer",
					"description": "从第 N 行开始读（1-based，含）。与 end_line 配合使用。设置后 offset 参数无效。",
				},
				"end_line": map[string]interface{}{
					"type":        "integer",
					"description": "读到第 N 行结束（1-based，含）。需配合 start_line 使用；不设则读到文件末尾（上限 200 行）。",
				},
				"offset": map[string]interface{}{
					"type":        "integer",
					"description": "起始字节偏移，单位为字节（不是行号）。默认 0。与 start_line 互斥，优先使用 start_line。",
				},
				"limit": map[string]interface{}{
					"type":        "integer",
					"description": fmt.Sprintf("最多读取的字节数（默认 %d）。与 offset 配合使用；使用 start_line/end_line 时此参数无效。", maxReadLen),
				},
			},
			"required": []string{"path"},
		},
	}
}

// readFileArgs 定义 read_file 工具的 JSON 参数结构。
// StartLine/EndLine 与 Offset/Limit 互斥：设置了 StartLine 时优先按行号读取。
type readFileArgs struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line,omitempty"` // 1-based，含；设置后走行号模式
	EndLine   int    `json:"end_line,omitempty"`   // 1-based，含；0 表示读到末尾（上限 200 行）
	Offset    int64  `json:"offset,omitempty"`
	Limit     int    `json:"limit,omitempty"`
}

// maxLineRead 行号模式下单次最多读取的行数（防止大文件占满上下文）。
// 200 行对 SWE-bench 大源文件偏小、需多次往返；提高到 500 行减少分页抖动。
const maxLineRead = 500

// Execute 执行文件读取操作，支持行号模式（start_line/end_line）和字节偏移模式（offset/limit）。
// 设置了 start_line 时优先走行号模式。
func (t *ReadFileTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var input readFileArgs
	if err := json.Unmarshal(args, &input); err != nil {
		return "", fmt.Errorf("参数解析失败：%w", err)
	}

	fullPath, err := safePath(t.workDir, input.Path)
	if err != nil {
		return "", err
	}

	unlock := RLockPath(fullPath)
	defer unlock()

	// 行号模式：start_line > 0 时按行读取，忽略 offset/limit。
	if input.StartLine > 0 {
		return readFileByLines(fullPath, input.StartLine, input.EndLine)
	}

	// 字节偏移模式（原有逻辑）。
	file, err := os.Open(fullPath)
	if err != nil {
		return "", fmt.Errorf("打开文件失败：%w", err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("获取文件信息失败：%w", err)
	}
	totalSize := info.Size()

	offset := input.Offset
	if offset > 0 {
		if offset >= totalSize {
			return fmt.Sprintf("[offset=%d 超出文件大小（%d 字节），无内容可读。]", offset, totalSize), nil
		}
		if _, err := file.Seek(offset, io.SeekStart); err != nil {
			return "", fmt.Errorf("定位文件偏移失败：%w", err)
		}
	}

	limit := input.Limit
	if limit <= 0 {
		limit = maxReadLen
	}

	// 多读 1 字节用于检测是否真的超出上限
	content, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil {
		return "", fmt.Errorf("读取文件内容失败：%w", err)
	}

	if len(content) > limit {
		nextOffset := offset + int64(limit)
		return string(content[:limit]) + fmt.Sprintf(
			"\n\n...[内容已截断。offset 和 limit 单位均为字节（非行号）。"+
				"已读取 offset=%d 起的 %d 字节，文件总大小 %d 字节。"+
				"如需继续读取，请使用 offset=%d（不要把行号当作 offset）。"+
				"提示：按行读取更直观，可改用 start_line/end_line 参数。]...",
			offset, limit, totalSize, nextOffset,
		), nil
	}

	return string(content), nil
}

// readFileByLines 按行号读取文件内容（1-based，含两端）。
// endLine 为 0 表示读到文件末尾，但最多读 maxLineRead 行。
func readFileByLines(fullPath string, startLine, endLine int) (string, error) {
	if startLine < 1 {
		startLine = 1
	}
	// endLine 0 表示"读到末尾"，但上限 maxLineRead 行
	if endLine <= 0 || endLine-startLine+1 > maxLineRead {
		endLine = startLine + maxLineRead - 1
	}

	f, err := os.Open(fullPath)
	if err != nil {
		return "", fmt.Errorf("打开文件失败：%w", err)
	}
	defer f.Close()

	var sb strings.Builder
	scanner := bufio.NewScanner(f)
	// 扩大 scanner buffer 以支持较长行
	scanner.Buffer(make([]byte, 512*1024), 512*1024)

	lineNum := 0
	// truncated 标记循环因"超过 endLine"退出（而非文件结束），用于决定是否输出截断提示。
	// 不在循环结束后再调用 scanner.Scan()——那会消费额外一行，产生副作用。
	truncated := false
	for scanner.Scan() {
		lineNum++
		if lineNum < startLine {
			continue
		}
		if lineNum > endLine {
			truncated = true
			break
		}
		// 每行带 1-based 行号前缀（`行号→Tab→内容`），便于 LLM 精确定位、
		// 减少 edit_file 的多匹配失败。行号仅供显示，调用 edit_file 时
		// source_text 不应包含该前缀（见 Definition 说明）。
		fmt.Fprintf(&sb, "%6d\t%s\n", lineNum, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		// 单行超过 scanner 缓冲（>512KB，如压缩/生成代码、长 JSON）时优雅降级，
		// 提示改用字节模式，而非抛出难以理解的通用错误。
		if errors.Is(err, bufio.ErrTooLong) {
			return "", fmt.Errorf("文件含超长行（>512KB），行号模式不支持；请改用 offset/limit 字节模式读取该文件")
		}
		return "", fmt.Errorf("读取文件失败：%w", err)
	}

	// sb.Len() == 0 只有一种真实情况：startLine 超出文件总行数。
	// "区间内全是空行"不可能发生——空行也会写入 '\n'，sb.Len() > 0。
	if sb.Len() == 0 {
		return fmt.Sprintf("[start_line=%d 超出文件总行数（%d 行）]", startLine, lineNum), nil
	}

	suffix := ""
	if truncated {
		suffix = fmt.Sprintf("\n...[已读取第 %d-%d 行，如需继续请使用 start_line=%d]...", startLine, endLine, endLine+1)
	}
	return sb.String() + suffix, nil
}
