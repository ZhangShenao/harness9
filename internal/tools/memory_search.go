// Package tools — memory_search 工具（长期记忆全文检索）。
package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/harness9/internal/ltm"
	"github.com/harness9/internal/schema"
)

// MemorySearchTool 实现 BaseTool，用 FTS5 检索长期记忆。
type MemorySearchTool struct {
	store *ltm.Store
}

// NewMemorySearchTool 创建检索工具。
func NewMemorySearchTool(store *ltm.Store) *MemorySearchTool {
	return &MemorySearchTool{store: store}
}

// Name 返回工具标识符 "memory_search"。
func (t *MemorySearchTool) Name() string { return "memory_search" }

// Definition 返回工具元信息。
func (t *MemorySearchTool) Definition() schema.ToolDefinition {
	return schema.ToolDefinition{
		Name: "memory_search",
		Description: "在跨会话长期记忆中按关键词全文检索。" +
			"当前任务可能涉及用户既有偏好、过去的决策或项目背景时调用，召回相关记忆。",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"query": map[string]interface{}{"type": "string", "description": "检索关键词"},
				"limit": map[string]interface{}{"type": "integer", "description": "返回上限，默认 5"},
			},
			"required": []string{"query"},
		},
	}
}

type memorySearchArgs struct {
	Query string `json:"query"`
	Limit int    `json:"limit"`
}

// Execute 处理 memory_search 调用，返回命中条目的 JSON 数组（无命中返回 "[]"）。
func (t *MemorySearchTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var in memorySearchArgs
	if err := json.Unmarshal(args, &in); err != nil {
		return "", fmt.Errorf("参数解析失败：%w", err)
	}
	entries, err := t.store.Search(ctx, in.Query, in.Limit)
	if err != nil {
		return "", fmt.Errorf("检索记忆失败：%w", err)
	}
	if entries == nil {
		entries = []*ltm.Entry{}
	}
	b, err := json.Marshal(entries)
	if err != nil {
		return "", fmt.Errorf("序列化结果失败：%w", err)
	}
	return string(b), nil
}
