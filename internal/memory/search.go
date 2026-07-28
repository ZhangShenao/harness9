// Package memory — SearchMessages：基于 FTS5 的会话消息全文检索。
// 本文件实现 MessageSearchResult 类型及 Manager.SearchMessages 方法，
// 通过 messages_fts standalone FTS5 虚表执行全文检索，与 messages 表写入保持同步。
package memory

import (
	"context"
	"fmt"
	"strings"
)

// MessageSearchResult 是 SearchMessages 返回的单条检索结果。
type MessageSearchResult struct {
	SessionID string
	Role      string
	Content   string
}

// ftsMessageQuery 将用户输入转换为安全的 FTS5 MATCH 表达式：
// 按空白分词，每个 token 使用双引号包裹（内部双引号翻倍转义），以 OR 连接。
// 无有效 token 时返回空串。
func ftsMessageQuery(q string) string {
	fields := strings.Fields(q)
	if len(fields) == 0 {
		return ""
	}
	quoted := make([]string, 0, len(fields))
	for _, f := range fields {
		quoted = append(quoted, `"`+strings.ReplaceAll(f, `"`, `""`)+`"`)
	}
	return strings.Join(quoted, " OR ")
}

// SearchMessages 在 messages_fts 表上执行 FTS5 全文检索。
//
//   - query 为空字符串时，直接返回空切片和 nil error。
//   - limit <= 0 时默认最多返回 20 条。
//   - 结果按 rowid 升序（确定性）排列。
func (m *Manager) SearchMessages(ctx context.Context, query string, limit int) ([]MessageSearchResult, error) {
	match := ftsMessageQuery(query)
	if match == "" {
		return []MessageSearchResult{}, nil
	}
	if limit <= 0 {
		limit = 20
	}

	rows, err := m.db.QueryContext(ctx,
		`SELECT session_id, role, content
		 FROM messages_fts
		 WHERE messages_fts MATCH ?
		 ORDER BY rowid ASC
		 LIMIT ?`,
		match, limit)
	if err != nil {
		return nil, fmt.Errorf("fts 消息检索: %w", err)
	}
	defer rows.Close()

	var results []MessageSearchResult
	for rows.Next() {
		var r MessageSearchResult
		if err := rows.Scan(&r.SessionID, &r.Role, &r.Content); err != nil {
			return nil, fmt.Errorf("扫描 fts 结果: %w", err)
		}
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历 fts 结果: %w", err)
	}

	if results == nil {
		results = []MessageSearchResult{}
	}
	return results, nil
}
