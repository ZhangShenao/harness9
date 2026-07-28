// Package tools — session_search 工具（会话消息全文检索）。
package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/harness9/internal/memory"
	"github.com/harness9/internal/schema"
)

// SessionSearchTool 实现 BaseTool，用 FTS5 检索当前及历史会话的原始消息内容。
type SessionSearchTool struct {
	manager *memory.Manager
}

// NewSessionSearchTool 创建会话消息检索工具。
func NewSessionSearchTool(manager *memory.Manager) *SessionSearchTool {
	return &SessionSearchTool{manager: manager}
}

// Name 返回工具标识符 "session_search"。
func (t *SessionSearchTool) Name() string { return "session_search" }

// Definition 返回工具元信息。
func (t *SessionSearchTool) Definition() schema.ToolDefinition {
	return schema.ToolDefinition{
		Name: "session_search",
		Description: "在当前及历史会话的原始消息内容中按关键词全文检索。" +
			"检索的是会话消息本身（user/assistant 对话原文），" +
			"区别于 memory_search 检索的是跨会话长期记忆条目。" +
			"当需要回溯过去对话中的具体内容、引用或上下文时调用。",
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

type sessionSearchArgs struct {
	Query string `json:"query"`
	Limit int    `json:"limit"`
}

// Execute 处理 session_search 调用，返回命中消息的 JSON 数组（无命中返回 "[]"）。
// query 为空时返回参数错误，不发起检索。
func (t *SessionSearchTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var in sessionSearchArgs
	if err := json.Unmarshal(args, &in); err != nil {
		return "", fmt.Errorf("参数解析失败: %w", err)
	}
	if in.Query == "" {
		return "", fmt.Errorf("参数错误: query 不能为空")
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 5
	}
	results, err := t.manager.SearchMessages(ctx, in.Query, limit)
	if err != nil {
		return "", fmt.Errorf("检索会话消息失败: %w", err)
	}
	if len(results) == 0 {
		return "[]", nil
	}
	b, err := json.Marshal(results)
	if err != nil {
		return "", fmt.Errorf("序列化结果失败: %w", err)
	}
	return string(b), nil
}
