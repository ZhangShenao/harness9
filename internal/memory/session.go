// Package memory — Session 接口与元数据类型。
// 本文件定义了 harness9 会话管理的核心接口契约和 SessionInfo 元数据类型。
package memory

import (
	"context"
	"time"

	"github.com/harness9/internal/planning"
	"github.com/harness9/internal/schema"
)

// Session 管理单个会话的消息历史与规划状态。
type Session interface {
	SessionID() string
	GetMessages(ctx context.Context, limit int) ([]schema.Message, error)
	AddMessages(ctx context.Context, msgs []schema.Message) error
	PopMessage(ctx context.Context) (*schema.Message, error)
	Clear(ctx context.Context) error

	// GetPlan 返回该会话已持久化的计划条目。无计划时返回 nil, nil。
	GetPlan(ctx context.Context) ([]planning.PlanItem, error)

	// SavePlan 原子性保存计划条目（write-replace 语义）。items 为空时清空计划。
	SavePlan(ctx context.Context, items []planning.PlanItem) error
}

// SessionInfo 是 Manager.ListSessions 返回的会话元数据。
type SessionInfo struct {
	ID        string
	CreatedAt time.Time
	UpdatedAt time.Time
	MsgCount  int
}
