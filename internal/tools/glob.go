// Package tools — glob 工具：跨目录文件名模式匹配（对标 Codex/Claude Code 的 Glob）。
//
// 纯 Go 实现：filepath.WalkDir + 分段 ** 匹配（** 匹配零或多层目录，段内
// path.Match 语义）。默认 mtime 降序（最新改动优先），输出 ≤200 条 + 总数。
// 默认排除 .git 目录；其余不排除——pattern 即契约。
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/harness9/internal/schema"
)

// globMaxResults 是输出条数上限（超出截断并标注总数）。
const globMaxResults = 200

// GlobTool 实现 BaseTool。
type GlobTool struct {
	workDir string
}

// NewGlobTool 创建 glob 工具。
func NewGlobTool(workDir string) *GlobTool {
	return &GlobTool{workDir: workDir}
}

// Name 返回工具名 "glob"。
func (t *GlobTool) Name() string { return "glob" }

// Definition 返回工具定义。
func (t *GlobTool) Definition() schema.ToolDefinition {
	return schema.ToolDefinition{
		Name:        "glob",
		Description: "按 glob 模式快速查找文件（比 bash find 更快且结构化输出）。模式用 / 分隔；** 匹配任意层级目录，段内支持 *、?、[...]（不跨目录）。默认按修改时间降序（最新优先），可选 name 字典序。返回相对路径列表（≤200 条）。",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern": map[string]any{"type": "string", "description": "glob 模式，如 **/*_test.go 或 internal/**/*.go"},
				"path":    map[string]any{"type": "string", "description": "起始目录（默认工作区根）"},
				"sort":    map[string]any{"type": "string", "enum": []string{"mtime", "name"}, "description": "排序方式，默认 mtime（新→旧）"},
			},
			"required": []string{"pattern"},
		},
	}
}

type globArgs struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path"`
	Sort    string `json:"sort"`
}

// Execute 执行 glob 查找。
func (t *GlobTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a globArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("参数解析失败：%w", err)
	}
	if a.Pattern == "" {
		return "", fmt.Errorf("pattern 不能为空")
	}
	root := t.workDir
	if a.Path != "" {
		p, err := safePath(t.workDir, a.Path)
		if err != nil {
			return "", fmt.Errorf("path 校验失败: %w", err)
		}
		root = p
	}
	// I1：遍历前校验 root——不存在或非目录时 WalkDir 的 fail-open 会吞掉根级错误，
	// 导致误报"没有匹配的文件"；此处显式返回 Go error。
	if info, serr := os.Stat(root); serr != nil || !info.IsDir() {
		shown := a.Path
		if shown == "" {
			shown = t.workDir
		}
		return "", fmt.Errorf("搜索路径 %q 不存在或不是目录", shown)
	}

	type entry struct {
		rel   string
		mtime int64
	}
	var matches []entry
	pattern := filepath.ToSlash(a.Pattern)
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // 不可读条目跳过（fail-open，保持搜索完整性）
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if !MatchGlobPath(rel, pattern) {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		matches = append(matches, entry{rel: rel, mtime: info.ModTime().UnixNano()})
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("遍历失败: %w", err)
	}

	if len(matches) == 0 {
		return "没有匹配的文件。", nil
	}
	switch a.Sort {
	case "name":
		sort.Slice(matches, func(i, j int) bool { return matches[i].rel < matches[j].rel })
	default:
		sort.Slice(matches, func(i, j int) bool { return matches[i].mtime > matches[j].mtime })
	}

	var sb strings.Builder
	shown := matches
	truncated := false
	if len(shown) > globMaxResults {
		shown = shown[:globMaxResults]
		truncated = true
	}
	for _, e := range shown {
		sb.WriteString(e.rel)
		sb.WriteString("\n")
	}
	fmt.Fprintf(&sb, "共 %d 个文件", len(matches))
	if truncated {
		fmt.Fprintf(&sb, "（显示前 %d 个）", globMaxResults)
	}
	return sb.String(), nil
}

// MatchGlobPath 判断相对路径 rel（/ 分隔）是否匹配 pattern（/ 分隔）。
// 语义：** 段匹配零或多层目录；其余段按 path.Match（* 不跨 /）。
func MatchGlobPath(rel, pattern string) bool {
	return matchGlobSegments(strings.Split(strings.Trim(rel, "/"), "/"),
		strings.Split(strings.Trim(pattern, "/"), "/"))
}

func matchGlobSegments(segs, pat []string) bool {
	if len(pat) == 0 {
		return len(segs) == 0
	}
	if pat[0] == "**" {
		for i := 0; i <= len(segs); i++ {
			if matchGlobSegments(segs[i:], pat[1:]) {
				return true
			}
		}
		return false
	}
	if len(segs) == 0 {
		return false
	}
	ok, err := path.Match(pat[0], segs[0])
	if err != nil || !ok {
		return false
	}
	return matchGlobSegments(segs[1:], pat[1:])
}
