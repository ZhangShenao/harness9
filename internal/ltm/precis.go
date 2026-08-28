// Package ltm — Precis：MEMORY.md 物化视图管理器。
// 本文件实现 Precis，从 Store 拉取 top-N 高价值条目并渲染为有界 Markdown 文件，
// 供 DefaultPromptBuilder 在每次 Build() 时读取并注入 System Prompt 长期记忆段。
// 渲染结果在 maxBytes 处按 UTF-8 rune 边界截断，防止 token bomb。
package ltm

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// precisMaxEntries 是渲染进精华视图的最大条目数（按 importance 取 top-N）。
const precisMaxEntries = 30

// Precis 维护 MEMORY.md 物化视图：从 Store 的 top-N 高价值条目渲染出有界的
// markdown 文件，供 System Prompt 全量注入。每次写入记忆后调用 Regenerate 重建。
type Precis struct {
	store    *Store
	path     string // MEMORY.md 绝对路径
	maxBytes int    // 注入预算上限（字节）
}

// NewPrecis 创建绑定到指定 Store 与文件路径的 Precis。maxBytes<=0 时默认 5120。
func NewPrecis(store *Store, path string, maxBytes int) *Precis {
	if maxBytes <= 0 {
		maxBytes = 5120
	}
	return &Precis{store: store, path: path, maxBytes: maxBytes}
}

// Regenerate 从 Store 拉取 top-N 条目，渲染为 markdown 并写入 MEMORY.md（含父目录创建）。
func (p *Precis) Regenerate(ctx context.Context) error {
	entries, err := p.store.List(ctx, precisMaxEntries)
	if err != nil {
		return fmt.Errorf("拉取精华条目：%w", err)
	}
	content := renderPrecis(entries, p.maxBytes)
	if err := os.MkdirAll(filepath.Dir(p.path), 0700); err != nil {
		return fmt.Errorf("创建精华目录：%w", err)
	}
	if err := os.WriteFile(p.path, []byte(content), 0600); err != nil {
		return fmt.Errorf("写入精华文件：%w", err)
	}
	return nil
}

// Read 读取 MEMORY.md 内容；文件不存在时返回空串（不报错）。
func (p *Precis) Read() (string, error) {
	data, err := os.ReadFile(p.path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("读取精华文件：%w", err)
	}
	return string(data), nil
}

// renderPrecis 将条目渲染为 markdown，并在 maxBytes 处按 UTF-8 边界截断。
func renderPrecis(entries []*Entry, maxBytes int) string {
	if len(entries) == 0 {
		return ""
	}
	var b strings.Builder
	for _, e := range entries {
		b.WriteString("## ")
		b.WriteString(e.Title)
		if e.Category != "" {
			b.WriteString(fmt.Sprintf(" `%s`", e.Category))
		}
		b.WriteString("\n")
		b.WriteString(e.Content)
		b.WriteString("\n\n")
	}
	return truncateUTF8(strings.TrimRight(b.String(), "\n"), maxBytes)
}

// truncateUTF8 将 s 截断到不超过 maxBytes 字节，保证不切断多字节 rune，
// 截断时追加省略标记。
func truncateUTF8(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	const marker = "\n…（已截断）"
	budget := maxBytes - len(marker)
	if budget <= 0 {
		return ""
	}
	cut := budget
	for cut > 0 && !utf8RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + marker
}

// utf8RuneStart 报告字节 b 是否是一个 UTF-8 rune 的起始字节。
func utf8RuneStart(b byte) bool {
	return b&0xC0 != 0x80
}
