// Package tools — memory_write 工具（长期记忆写入）。
//
// 三种动作：
//   - add：新增一条记忆（内容签名去重）
//   - update：按 id 部分更新既有记忆（仅覆盖调用方显式提供的字段）
//   - remove：按 id 软删除记忆
//
// 每次成功写入后重建 MEMORY.md 物化视图（若注入了 Precis）。
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/harness9/internal/logfmt"
	"github.com/harness9/internal/ltm"
	"github.com/harness9/internal/schema"
)

// MemoryWriteTool 实现 BaseTool，向长期记忆 Store 写入条目。
type MemoryWriteTool struct {
	store  *ltm.Store
	precis *ltm.Precis // 可选，nil 时跳过精华重建
}

// NewMemoryWriteTool 创建写入工具。precis 可为 nil。
func NewMemoryWriteTool(store *ltm.Store, precis *ltm.Precis) *MemoryWriteTool {
	return &MemoryWriteTool{store: store, precis: precis}
}

// Name 返回工具标识符 "memory_write"。
func (t *MemoryWriteTool) Name() string { return "memory_write" }

// Definition 返回工具元信息。
func (t *MemoryWriteTool) Definition() schema.ToolDefinition {
	return schema.ToolDefinition{
		Name: "memory_write",
		Description: "写入跨会话长期记忆。action=add 新增（相同内容自动去重）；" +
			"action=update 按 id 部分更新（仅覆盖提供的字段）；action=remove 按 id 删除。" +
			"用于记住用户偏好、稳定的项目知识、关键决策或可复用技能。",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"action":  map[string]interface{}{"type": "string", "enum": []string{"add", "update", "remove"}},
				"id":      map[string]interface{}{"type": "string", "description": "update/remove 时必填"},
				"title":   map[string]interface{}{"type": "string"},
				"content": map[string]interface{}{"type": "string"},
				"category": map[string]interface{}{
					"type": "string",
					"enum": []string{"knowledge", "preference", "task", "skill"},
				},
				"importance": map[string]interface{}{"type": "integer", "description": "0-10"},
				"ttl_days":   map[string]interface{}{"type": "integer", "description": "可选，过期天数；省略=永不过期"},
				"tags":       map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}},
			},
			"required": []string{"action"},
		},
	}
}

// memoryWriteArgs 定义 memory_write 工具的 JSON 参数结构。
type memoryWriteArgs struct {
	Action     string   `json:"action"`
	ID         string   `json:"id"`
	Title      string   `json:"title"`
	Content    string   `json:"content"`
	Category   string   `json:"category"`
	Importance int      `json:"importance"`
	TTLDays    int      `json:"ttl_days"`
	Tags       []string `json:"tags"`
}

// Execute 处理 memory_write 调用，返回写入后条目的 JSON（remove 返回状态消息）。
func (t *MemoryWriteTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var in memoryWriteArgs
	if err := json.Unmarshal(args, &in); err != nil {
		return "", fmt.Errorf("参数解析失败：%w", err)
	}

	var result string
	switch in.Action {
	case "add":
		if in.Content == "" {
			return "", fmt.Errorf("add 需要非空 content")
		}
		e, err := t.store.Add(ctx, &ltm.Entry{
			Title: in.Title, Content: in.Content, Category: ltm.Category(in.Category),
			Importance: in.Importance, TTLDays: in.TTLDays, Tags: in.Tags,
		})
		if err != nil {
			return "", fmt.Errorf("写入记忆失败：%w", err)
		}
		result = mustJSON(e)
	case "update":
		if in.ID == "" {
			return "", fmt.Errorf("update 需要 id")
		}
		// 部分更新（merge）：先取出现有条目，仅覆盖调用方显式提供的字段，
		// 未提供的字段保留原值，避免 LLM 仅想改某一字段却误清空 content。
		// 已知限制：importance 与 ttl_days 为整数类型，JSON 零值（0）与「未提供」无法区分。
		// 因此无法通过 update 将 importance 或 ttl_days 显式设回 0；
		// 如需设为 0，需先 remove 再 add。这是简化设计的权衡，LLM 极少需要将重要度降为 0。
		existing, err := t.store.Get(ctx, in.ID)
		if err != nil {
			return "", fmt.Errorf("查询待更新记忆失败：%w", err)
		}
		if in.Title != "" {
			existing.Title = in.Title
		}
		if in.Content != "" {
			existing.Content = in.Content
		}
		if in.Category != "" {
			existing.Category = ltm.Category(in.Category)
		}
		if in.Importance != 0 {
			existing.Importance = in.Importance
		}
		if in.TTLDays != 0 {
			existing.TTLDays = in.TTLDays
		}
		if in.Tags != nil {
			existing.Tags = in.Tags
		}
		if err := t.store.Update(ctx, existing); err != nil {
			return "", fmt.Errorf("更新记忆失败：%w", err)
		}
		result = mustJSON(existing)
	case "remove":
		if in.ID == "" {
			return "", fmt.Errorf("remove 需要 id")
		}
		if err := t.store.SoftDelete(ctx, in.ID); err != nil {
			return "", fmt.Errorf("删除记忆失败：%w", err)
		}
		result = mustJSON(map[string]string{"status": "removed", "id": in.ID})
	default:
		return "", fmt.Errorf("未知 action: %q（应为 add/update/remove）", in.Action)
	}

	// 重建 MEMORY.md 物化视图（fail-soft：失败仅记日志）。
	if t.precis != nil {
		if err := t.precis.Regenerate(ctx); err != nil {
			log.Print(logfmt.FormatMsg("memory_write", fmt.Sprintf("重建精华失败: %v", err)))
		}
	}
	return result, nil
}

// mustJSON 将值序列化为 JSON 字符串，失败时返回空对象。
func mustJSON(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}
