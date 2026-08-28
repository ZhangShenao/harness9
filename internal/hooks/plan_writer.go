// Package hooks — FilePlanWriter：todo 计划持久化到 Markdown 文件。
// 本文件实现 FilePlanWriter，在每次 todo_write 工具成功写入后，
// 将当前 TodoItem 列表序列化为 Markdown 格式并覆写固定路径的计划文件。
// 路径策略：git 项目写入 workDir/.harness9/plans/；否则写入 homeDir/.harness9/plans/。
// FilePlanWriter 实现了 planning.PlanWriter 接口，通过 tools.WithPlanWriter 注入 TodoWriteTool。
package hooks

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/harness9/internal/planning"
)

// FilePlanWriter 将 todo 列表写入固定路径的 markdown 文件，每次调用覆写。
// 路径在构造时确定：git 项目写入 workDir/.harness9/plans/，否则写入 homeDir/.harness9/plans/。
type FilePlanWriter struct {
	path      string
	sessionID string
}

// NewFilePlanWriter 计算 plan 文件路径，创建目录，并返回 FilePlanWriter。
// 通过检测 workDir/.git 判断是否为 git 项目。
// 若目录无法创建则立即返回错误，实现启动时快速失败。
func NewFilePlanWriter(workDir, homeDir, sessionID string) (*FilePlanWriter, error) {
	timestamp := time.Now().Unix()
	slug := sessionID
	if len(slug) > 8 {
		slug = slug[:8]
	}
	filename := fmt.Sprintf("%d-%s.md", timestamp, slug)

	var base string
	if isGitRepo(workDir) {
		base = filepath.Join(workDir, ".harness9", "plans")
	} else {
		base = filepath.Join(homeDir, ".harness9", "plans")
	}

	if err := os.MkdirAll(base, 0700); err != nil {
		return nil, fmt.Errorf("创建计划目录失败：%w", err)
	}

	return &FilePlanWriter{
		path:      filepath.Join(base, filename),
		sessionID: sessionID,
	}, nil
}

// isGitRepo 检测 workDir 是否包含 .git 目录/文件。
func isGitRepo(workDir string) bool {
	_, err := os.Stat(filepath.Join(workDir, ".git"))
	return err == nil
}

// Write 将 todos 序列化为 markdown 并覆写计划文件。
// 写入失败时返回 error（调用方决定是否记录日志，不中断主流程）。
// 目录已在构造时由 NewFilePlanWriter 创建，此处无需再次检查。
func (w *FilePlanWriter) Write(todos []planning.TodoItem) error {
	return os.WriteFile(w.path, []byte(formatPlanMarkdown(w.sessionID, todos)), 0600)
}

// Path 返回计划文件的绝对路径（供测试和日志使用）。
func (w *FilePlanWriter) Path() string { return w.path }

func formatPlanMarkdown(sessionID string, todos []planning.TodoItem) string {
	var sb strings.Builder
	sb.WriteString("# 执行计划\n\n")
	fmt.Fprintf(&sb, "session: %s\n", sessionID)
	fmt.Fprintf(&sb, "updated: %s\n\n", time.Now().Format(time.RFC3339))
	sb.WriteString("## 任务列表\n\n")
	for _, item := range todos {
		marker := todoMarker(item.Status)
		fmt.Fprintf(&sb, "- %s %s\n", marker, item.Content)
	}
	return sb.String()
}

func todoMarker(status planning.TodoStatus) string {
	switch status {
	case planning.TodoPending:
		return "[ ]"
	case planning.TodoInProgress:
		return "[>]"
	case planning.TodoCompleted:
		return "[x]"
	case planning.TodoCancelled:
		return "[-]"
	default:
		return "[ ]"
	}
}
