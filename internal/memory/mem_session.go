// Package memory — MemorySession：Session 的纯内存实现。
// 主要用于测试（隔离 SQLite 依赖）以及子代理的临时会话
// （子代理上下文在任务结束后即丢弃，无需 SQLite 持久化）。
package memory

import (
	"context"
	"sync"

	"github.com/harness9/internal/planning"
	"github.com/harness9/internal/schema"
)

// MemorySession 是 Session 的纯内存实现，使用 sync.Mutex 保证线程安全。
// 主要用于测试，同时也被 subagent.Runner 用于子代理的隔离会话（子代理会话不需要持久化，
// 独立 context 结束后即丢弃，无需写入 SQLite）。
type MemorySession struct {
	mu   sync.Mutex
	id   string
	msgs []schema.Message
	plan []planning.PlanItem
}

// NewMemorySession 创建指定 ID 的内存会话。
func NewMemorySession(id string) *MemorySession {
	return &MemorySession{id: id}
}

func (s *MemorySession) SessionID() string { return s.id }

func (s *MemorySession) GetMessages(_ context.Context, limit int) ([]schema.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 || limit >= len(s.msgs) {
		result := make([]schema.Message, len(s.msgs))
		copy(result, s.msgs)
		return result, nil
	}
	start := len(s.msgs) - limit
	result := make([]schema.Message, limit)
	copy(result, s.msgs[start:])
	return result, nil
}

func (s *MemorySession) AddMessages(_ context.Context, msgs []schema.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.msgs = append(s.msgs, msgs...)
	return nil
}

func (s *MemorySession) PopMessage(_ context.Context) (*schema.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.msgs) == 0 {
		return nil, nil
	}
	msg := s.msgs[len(s.msgs)-1]
	s.msgs = s.msgs[:len(s.msgs)-1]
	return &msg, nil
}

func (s *MemorySession) Clear(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.msgs = nil
	return nil
}

// GetPlan 内存实现：返回计划条目副本；无计划时返回 nil, nil。
func (s *MemorySession) GetPlan(_ context.Context) ([]planning.PlanItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.plan) == 0 {
		return nil, nil
	}
	out := make([]planning.PlanItem, len(s.plan))
	copy(out, s.plan)
	return out, nil
}

// SavePlan 内存实现：write-replace。
func (s *MemorySession) SavePlan(_ context.Context, items []planning.PlanItem) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.plan = make([]planning.PlanItem, len(items))
	copy(s.plan, items)
	return nil
}
