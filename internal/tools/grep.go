// Package tools — grep 工具：跨文件正则内容搜索（对标 Codex/Claude Code 的 Grep）。
//
// 纯 Go 流式逐行扫描：跳过二进制（首 8KB 含 NUL）与 >10MB 文件、默认排除 .git；
// glob 参数过滤文件名（复用 glob 工具的 MatchGlobPath，支持 **）。
// 每行命中截断 500 runes（UTF-8 安全）；max_results clamp [1,500]。
package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/harness9/internal/schema"
)

const (
	// grepDefaultMaxResults 是默认命中行上限。
	grepDefaultMaxResults = 50
	// grepHardMaxResults 是命中行硬上限。
	grepHardMaxResults = 500
	// grepLineMaxRunes 是单行输出的 rune 截断上限。
	grepLineMaxRunes = 500
	// grepMaxFileSize 是跳过的单文件大小上限。
	grepMaxFileSize = 10 << 20
	// grepBinarySniff 是二进制嗅探的读取长度。
	grepBinarySniff = 8 << 10
	// grepMaxLineBytes 是 bufio.Scanner 的单行缓冲上限。
	grepMaxLineBytes = 1 << 20
)

// GrepTool 实现 BaseTool。
type GrepTool struct {
	workDir string
}

// NewGrepTool 创建 grep 工具。
func NewGrepTool(workDir string) *GrepTool {
	return &GrepTool{workDir: workDir}
}

// Name 返回工具名 "grep"。
func (t *GrepTool) Name() string { return "grep" }

// Definition 返回工具定义。
func (t *GrepTool) Definition() schema.ToolDefinition {
	return schema.ToolDefinition{
		Name:        "grep",
		Description: "跨文件正则内容搜索（比 bash grep 更快的结构化输出）。输出 path:line: 行内容（每行截断 500 字符），默认最多 50 行命中；glob 参数可过滤文件名（如 *.go）。适合快速定位符号定义、调用点与配置项。",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern":     map[string]any{"type": "string", "description": "Go 正则语法（https://pkg.go.dev/regexp/syntax）"},
				"path":        map[string]any{"type": "string", "description": "起始目录（默认工作区根）"},
				"glob":        map[string]any{"type": "string", "description": "文件名过滤模式，如 *.go（支持 **）"},
				"ignore_case": map[string]any{"type": "boolean", "description": "忽略大小写（默认 false）"},
				"max_results": map[string]any{"type": "integer", "description": "命中行上限（默认 50，上限 500）"},
			},
			"required": []string{"pattern"},
		},
	}
}

type grepArgs struct {
	Pattern    string `json:"pattern"`
	Path       string `json:"path"`
	Glob       string `json:"glob"`
	IgnoreCase bool   `json:"ignore_case"`
	MaxResults int    `json:"max_results"`
}

// Execute 执行内容搜索。
func (t *GrepTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a grepArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("参数解析失败：%w", err)
	}
	if a.Pattern == "" {
		return "", fmt.Errorf("pattern 不能为空")
	}
	expr := a.Pattern
	if a.IgnoreCase {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return "", fmt.Errorf("正则编译失败: %w", err)
	}
	maxResults := a.MaxResults
	if maxResults <= 0 {
		maxResults = grepDefaultMaxResults
	}
	if maxResults > grepHardMaxResults {
		maxResults = grepHardMaxResults
	}
	root := t.workDir
	if a.Path != "" {
		p, serr := safePath(t.workDir, a.Path)
		if serr != nil {
			return "", fmt.Errorf("path 校验失败: %w", serr)
		}
		root = p
	}
	// I1：遍历前校验 root——不存在或非目录时 WalkDir 的 fail-open 会吞掉根级错误，
	// 导致误报"没有命中的内容"；此处显式返回 Go error。
	if info, serr := os.Stat(root); serr != nil || !info.IsDir() {
		shown := a.Path
		if shown == "" {
			shown = t.workDir
		}
		return "", fmt.Errorf("搜索路径 %q 不存在或不是目录", shown)
	}

	var lines []string
	hitFiles := 0
	truncated := false
	werr := filepath.WalkDir(root, func(p string, d os.DirEntry, werr error) error {
		if werr != nil {
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if truncated {
			return filepath.SkipAll
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if a.Glob != "" && !MatchGlobPath(rel, a.Glob) && !MatchGlobPath(filepath.Base(rel), a.Glob) {
			return nil
		}
		fileHit, ferr := grepFile(p, rel, re, maxResults-len(lines), &lines)
		if ferr != nil {
			return nil // 单文件失败跳过（fail-open）
		}
		if fileHit {
			hitFiles++
		}
		if len(lines) >= maxResults {
			truncated = true
			return filepath.SkipAll
		}
		return nil
	})
	if werr != nil {
		return "", fmt.Errorf("遍历失败: %w", werr)
	}
	if len(lines) == 0 {
		return "没有命中的内容。", nil
	}
	var sb strings.Builder
	for _, ln := range lines {
		sb.WriteString(ln)
		sb.WriteString("\n")
	}
	fmt.Fprintf(&sb, "共 %d 个文件命中 / %d 行", hitFiles, len(lines))
	if truncated {
		fmt.Fprintf(&sb, "（已达上限 %d，已截断）", maxResults)
	}
	return sb.String(), nil
}

// grepFile 逐行扫描单文件，命中追加 "rel:line: text" 到 out。
// 返回该文件是否有命中；错误表示文件不可读/二进制/过大（调用方跳过）。
func grepFile(p, rel string, re *regexp.Regexp, budget int, out *[]string) (bool, error) {
	f, err := os.Open(p)
	if err != nil {
		return false, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > grepMaxFileSize {
		return false, err
	}
	// 二进制嗅探：首 8KB 含 NUL 即跳过
	head := make([]byte, grepBinarySniff)
	n, _ := io.ReadFull(f, head)
	if bytes.IndexByte(head[:n], 0) >= 0 {
		return false, nil
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return false, err
	}

	hit := false
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64<<10), grepMaxLineBytes)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		if re.MatchString(scanner.Text()) {
			hit = true
			*out = append(*out, fmt.Sprintf("%s:%d: %s", rel, lineNo, truncateRunesStr(scanner.Text(), grepLineMaxRunes)))
			if len(*out) >= budget {
				break
			}
		}
	}
	return hit, nil
}

// truncateRunesStr 按 runes 截断（UTF-8 安全），超出部分以 … 结尾。
func truncateRunesStr(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
