// Package memory - CompactionOffloader：压缩期工具结果外存（Offload）辅助器。
// ProgressiveCompactor 在压缩对话历史时，调用 CompactionOffloader 将超大的
// tool_result 消息写入文件系统，返回带预览的占位符字符串与元数据条目，
// 供压缩后的上下文保留可检索引用，而非完整原文。
package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/harness9/internal/schema"
)

// OffloadEntry 记录单条被外存到文件系统的工具结果元数据。
// ProgressiveCompactor 收集 OffloadEntry 列表，在压缩消息末尾追加引用清单，
// 供后续轮次通过 read_file 检索完整结果。
type OffloadEntry struct {
	ToolCallID string `json:"tool_call_id"`
	ToolName   string `json:"tool_name"`
	FilePath   string `json:"file_path"`
	Bytes      int    `json:"bytes"`
	Lines      int    `json:"lines"`
}

// CompactionOffloader 在压缩期将超大 tool_result 写入文件系统并返回占位符。
// threshold 以下或已被 OffloadHook 处理过的内容会被跳过（返回 error）；
// 同一 ToolCallID 首次写入后进入 cache，后续命中直接返回占位符而不重写文件，
// 避免覆盖外部对文件所做的修改。
type CompactionOffloader struct {
	workDir      string
	sessionID    string
	threshold    int
	previewLines int
	cache        map[string]bool
}

// NewCompactionOffloader 构造一个使用默认 threshold（4000 字节）与 previewLines（10）
// 的 CompactionOffloader，文件写入 workDir 下 .harness9/tool_results/<sessionID>/ 目录。
func NewCompactionOffloader(workDir, sessionID string) *CompactionOffloader {
	return &CompactionOffloader{
		workDir:      workDir,
		sessionID:    sessionID,
		threshold:    4000,
		previewLines: 10,
		cache:        make(map[string]bool),
	}
}

// OffloadToolResult 将 msg.Content 写入文件系统并返回占位符与元数据。
// 跳过条件（返回 error）：空 ToolCallID、内容低于 threshold、已被 OffloadHook
// 处理（内容以 "[输出已保存至" 开头）。命中 cache 时直接返回占位符，不重写文件。
func (o *CompactionOffloader) OffloadToolResult(msg schema.Message) (OffloadEntry, string, error) {
	if msg.ToolCallID == "" {
		return OffloadEntry{}, "", fmt.Errorf("empty ToolCallID")
	}
	if len(msg.Content) <= o.threshold {
		return OffloadEntry{}, "", fmt.Errorf("content below threshold")
	}
	if strings.HasPrefix(msg.Content, "[输出已保存至") {
		return OffloadEntry{}, "", fmt.Errorf("already offloaded by OffloadHook")
	}

	relPath := filepath.Join(".harness9", "tool_results", o.sessionID, msg.ToolCallID+".txt")

	if o.cache[msg.ToolCallID] {
		entry := o.buildEntry(msg, relPath)
		placeholder := o.buildPlaceholder(msg, relPath)
		return entry, placeholder, nil
	}

	absDir := filepath.Join(o.workDir, ".harness9", "tool_results", o.sessionID)
	if err := os.MkdirAll(absDir, 0700); err != nil {
		return OffloadEntry{}, "", fmt.Errorf("mkdir: %w", err)
	}
	absPath := filepath.Join(absDir, msg.ToolCallID+".txt")
	if err := os.WriteFile(absPath, []byte(msg.Content), 0600); err != nil {
		return OffloadEntry{}, "", fmt.Errorf("write file: %w", err)
	}

	o.cache[msg.ToolCallID] = true
	entry := o.buildEntry(msg, relPath)
	placeholder := o.buildPlaceholder(msg, relPath)
	return entry, placeholder, nil
}

func (o *CompactionOffloader) buildEntry(msg schema.Message, relPath string) OffloadEntry {
	lines := strings.Count(msg.Content, "\n") + 1
	return OffloadEntry{
		ToolCallID: msg.ToolCallID,
		FilePath:   relPath,
		Bytes:      len(msg.Content),
		Lines:      lines,
	}
}

func (o *CompactionOffloader) buildPlaceholder(msg schema.Message, relPath string) string {
	lines := strings.Split(msg.Content, "\n")
	totalLines := len(lines)
	previewEnd := o.previewLines
	if previewEnd > totalLines {
		previewEnd = totalLines
	}
	preview := strings.Join(lines[:previewEnd], "\n")
	return fmt.Sprintf(
		"[offloaded: %s | %d 行 / %d bytes]\n预览（前 %d 行）：\n%s\n...（完整结果已保存至文件，可通过 read_file 检索）",
		relPath, totalLines, len(msg.Content), previewEnd, preview,
	)
}
