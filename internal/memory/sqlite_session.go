// Package memory — SQLiteSession：会话消息和执行计划的 SQLite 持久化实现。
// 本文件实现 SQLiteSession，通过共享的 *sql.DB 连接（Manager 持有）执行 SQLite 操作。
// 写入操作（AddMessages、SavePlan）使用显式事务或 UPSERT 保证原子性。
// defer tx.Rollback() 配合 //nolint:errcheck 确保任何错误路径都能回滚事务，
// 避免 SQLite 连接进入错误状态。
package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/harness9/internal/planning"
	"github.com/harness9/internal/schema"
)

// SQLiteSession 是 Session 的 SQLite 持久化实现。
// 通过 Manager 创建，共享同一个 *sql.DB 连接。
type SQLiteSession struct {
	db        *sql.DB
	sessionID string
}

func (s *SQLiteSession) SessionID() string { return s.sessionID }

// GetMessages 返回历史消息，按插入顺序升序排列。
// limit=0 返回全部；limit>0 返回最近 limit 条（升序）。
func (s *SQLiteSession) GetMessages(ctx context.Context, limit int) ([]schema.Message, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if limit > 0 {
		rows, err = s.db.QueryContext(ctx, `
			SELECT role, content, tool_calls, tool_call_id
			FROM messages WHERE session_id = ?
			ORDER BY id DESC LIMIT ?`, s.sessionID, limit)
	} else {
		rows, err = s.db.QueryContext(ctx, `
			SELECT role, content, tool_calls, tool_call_id
			FROM messages WHERE session_id = ?
			ORDER BY id ASC`, s.sessionID)
	}
	if err != nil {
		return nil, fmt.Errorf("查询消息：%w", err)
	}
	defer rows.Close()

	var msgs []schema.Message
	for rows.Next() {
		var (
			roleStr     string
			content     string
			toolCallsJS sql.NullString
			toolCallID  sql.NullString
		)
		if err := rows.Scan(&roleStr, &content, &toolCallsJS, &toolCallID); err != nil {
			return nil, fmt.Errorf("扫描消息：%w", err)
		}
		msg := schema.Message{
			Role:    schema.Role(roleStr),
			Content: content,
		}
		if toolCallsJS.Valid && toolCallsJS.String != "" {
			if err := json.Unmarshal([]byte(toolCallsJS.String), &msg.ToolCalls); err != nil {
				return nil, fmt.Errorf("反序列化 tool_calls: %w", err)
			}
		}
		if toolCallID.Valid {
			msg.ToolCallID = toolCallID.String
		}
		msgs = append(msgs, msg)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("迭代消息：%w", err)
	}

	if limit > 0 {
		// 反转 DESC 结果为升序
		for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
			msgs[i], msgs[j] = msgs[j], msgs[i]
		}
	}
	return msgs, nil
}

// AddMessages 在事务中批量插入消息，并更新会话的 updated_at。
func (s *SQLiteSession) AddMessages(ctx context.Context, msgs []schema.Message) error {
	if len(msgs) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开始事务：%w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	now := time.Now().Unix()
	for _, msg := range msgs {
		var toolCallsJSON sql.NullString
		if len(msg.ToolCalls) > 0 {
			b, err := json.Marshal(msg.ToolCalls)
			if err != nil {
				return fmt.Errorf("序列化 tool_calls: %w", err)
			}
			toolCallsJSON = sql.NullString{String: string(b), Valid: true}
		}
		var toolCallID sql.NullString
		if msg.ToolCallID != "" {
			toolCallID = sql.NullString{String: msg.ToolCallID, Valid: true}
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO messages (session_id, role, content, tool_calls, tool_call_id, created_at)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			s.sessionID, string(msg.Role), msg.Content, toolCallsJSON, toolCallID, now)
		if err != nil {
			return fmt.Errorf("插入消息：%w", err)
		}
	}
	_, err = tx.ExecContext(ctx,
		`UPDATE sessions SET updated_at = ? WHERE id = ?`, now, s.sessionID)
	if err != nil {
		return fmt.Errorf("更新会话时间：%w", err)
	}
	return tx.Commit()
}

// PopMessage 删除并返回最新一条消息；无消息时返回 nil, nil。
func (s *SQLiteSession) PopMessage(ctx context.Context) (*schema.Message, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("开始事务：%w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	var (
		id          int64
		roleStr     string
		content     string
		toolCallsJS sql.NullString
		toolCallID  sql.NullString
	)
	err = tx.QueryRowContext(ctx,
		`SELECT id, role, content, tool_calls, tool_call_id
		 FROM messages WHERE session_id = ? ORDER BY id DESC LIMIT 1`,
		s.sessionID).Scan(&id, &roleStr, &content, &toolCallsJS, &toolCallID)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("查询最新消息：%w", err)
	}

	msg := &schema.Message{Role: schema.Role(roleStr), Content: content}
	if toolCallsJS.Valid && toolCallsJS.String != "" {
		if err := json.Unmarshal([]byte(toolCallsJS.String), &msg.ToolCalls); err != nil {
			return nil, fmt.Errorf("反序列化 tool_calls: %w", err)
		}
	}
	if toolCallID.Valid {
		msg.ToolCallID = toolCallID.String
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE id = ?`, id); err != nil {
		return nil, fmt.Errorf("删除消息：%w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("提交事务：%w", err)
	}
	return msg, nil
}

// Clear 删除该会话的全部消息。
func (s *SQLiteSession) Clear(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM messages WHERE session_id = ?`, s.sessionID)
	if err != nil {
		return fmt.Errorf("清空消息：%w", err)
	}
	return nil
}

// GetPlan 返回该会话已持久化的计划条目（单行 JSON 反序列化）。无记录时返回 nil, nil。
func (s *SQLiteSession) GetPlan(ctx context.Context) ([]planning.PlanItem, error) {
	var js sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT items FROM session_plans WHERE session_id = ?`, s.sessionID).Scan(&js)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("查询 plan: %w", err)
	}
	if !js.Valid || js.String == "" {
		return nil, nil
	}
	var items []planning.PlanItem
	if err := json.Unmarshal([]byte(js.String), &items); err != nil {
		return nil, fmt.Errorf("反序列化 plan: %w", err)
	}
	return items, nil
}

// SavePlan 原子性保存计划条目（write-replace，UPSERT 单行 JSON）。
func (s *SQLiteSession) SavePlan(ctx context.Context, items []planning.PlanItem) error {
	js := "[]"
	if len(items) > 0 {
		b, err := json.Marshal(items)
		if err != nil {
			return fmt.Errorf("序列化 plan: %w", err)
		}
		js = string(b)
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO session_plans (session_id, items, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(session_id) DO UPDATE
		    SET items = excluded.items, updated_at = excluded.updated_at`,
		s.sessionID, js, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("保存 plan: %w", err)
	}
	return nil
}
