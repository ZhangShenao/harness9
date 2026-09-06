// Package planning 实现 harness9 的规划模块：PlanStore（Session 级执行计划）。
//
// Plan 是 Agent 的原生能力（非用户切换的模式）：System Prompt 准则驱动 LLM
// 在复杂任务上先规划后执行；引擎负责写时检查点与压缩免疫注入（见 engine 包）。
package planning

import (
	"fmt"
	"strings"
	"sync"
)

// PlanStatus 表示单个计划条目的生命周期状态。
// 状态转换约束由 plan_write 工具（tools 包）负责执行，PlanStore 本身不做校验。
//
// 合法的状态转换路径：
//
//	pending ──► in_progress ──► completed
//	   │              │
//	   └──────────────┴──► cancelled
type PlanStatus string

const (
	// PlanPending 表示条目尚未开始。初始创建时的默认状态。
	PlanPending PlanStatus = "pending"
	// PlanInProgress 表示条目正在执行中。
	// LLM 在开始实际工具调用前应先将条目标记为此状态。
	PlanInProgress PlanStatus = "in_progress"
	// PlanCompleted 表示条目已完成，对应有实际产出（文件创建、命令执行等）。
	// 防作弊校验（plan_write）确保此状态不能被批量伪造。
	PlanCompleted PlanStatus = "completed"
	// PlanCancelled 表示条目已取消，不再执行。
	// 取消的条目不能直接标记为 completed，必须先恢复为 pending 或 in_progress。
	PlanCancelled PlanStatus = "cancelled"
)

// PlanItem 是单个计划条目，包含唯一标识、内容描述和当前状态。
// ID 由 LLM 自行分配，用于在全量替换时识别条目历史状态（防作弊校验的依据）。
type PlanItem struct {
	ID      string     `json:"id"`      // 条目的唯一标识符，LLM 自行分配
	Content string     `json:"content"` // 条目内容描述，应对应一个具体可执行动作
	Status  PlanStatus `json:"status"`  // 当前状态（pending/in_progress/completed/cancelled）
}

// PlanStore 是线程安全的会话级执行计划，采用全量替换（atomic replace）语义。
//
// 设计决策——全量替换 vs 增量更新：
// LLM 每次调用 plan_write 时输出完整的当前计划（而非增量指令），
// 全量替换与这种输出形式完全匹配，同时避免了增量 API 的状态一致性问题。
//
// 并发安全：内部使用 sync.RWMutex 保护 items 切片，
// Read 允许多读并发，Write 排他。所有方法均可从任意 goroutine 安全调用。
type PlanStore struct {
	mu    sync.RWMutex
	items []PlanItem
}

// NewPlanStore 创建空的 PlanStore，无任何初始条目。
func NewPlanStore() *PlanStore {
	return &PlanStore{}
}

// Write 原子性全量替换计划条目，返回替换后的列表副本。
// 先将入参复制到内部 slice，再返回内部 slice 的第二份副本：
// 双重复制确保调用方、内部存储与入参三者各自独立，互不影响。
func (s *PlanStore) Write(items []PlanItem) []PlanItem {
	s.mu.Lock()
	defer s.mu.Unlock()
	// 第一次 copy：内部存储与入参 items 解耦，防止调用方后续修改 items 影响内部状态。
	s.items = make([]PlanItem, len(items))
	copy(s.items, items)
	// 第二次 copy（通过 s.copy()）：返回值与内部存储解耦，防止调用方修改返回值影响 PlanStore。
	return s.copy()
}

// Read 返回当前计划条目的副本（线程安全）。
func (s *PlanStore) Read() []PlanItem {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.copy()
}

// copy 返回 s.items 的副本。调用方必须持有读锁或写锁后才能调用此方法。
// 空列表时返回 nil（而非空切片），与 json.Marshal 的行为兼容（nil → "null"，[]{} → "[]"）。
// 注意：PlanItem 是值类型（无指针字段），浅拷贝即为完整独立副本。
func (s *PlanStore) copy() []PlanItem {
	if len(s.items) == 0 {
		return nil
	}
	result := make([]PlanItem, len(s.items))
	copy(result, s.items)
	return result
}

// planInjectionHeader 是 FormatPlan 输出的标题行。
// 措辞强调"权威状态、持续有效"：注入每轮发生在发送视图上（压缩后、恢复后均生效）。
const planInjectionHeader = "## 当前执行计划（权威状态，压缩或恢复后仍以此为准继续执行）"

// FormatPlan 将 pending 和 in_progress 状态的条目格式化为纯文本注入块，
// 供引擎在每轮 LLM 调用前追加到发送视图末尾（原样注入，不经摘要器转述），
// 防止 LLM 在上下文压缩或会话恢复后遗忘尚未完成的计划。
//
// 无活跃条目（全部已完成或已取消）时返回空字符串，调用方应跳过注入。
//
// 输出格式示例：
//
//	## 当前执行计划（权威状态，压缩或恢复后仍以此为准继续执行）
//	[ ] 实现 handler/user.go
//	[>] 配置数据库连接
func (s *PlanStore) FormatPlan() string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var lines []string
	for _, item := range s.items {
		if item.Status == PlanPending || item.Status == PlanInProgress {
			// [ ] 表示 pending（待开始），[>] 表示 in_progress（进行中）
			prefix := "[ ]"
			if item.Status == PlanInProgress {
				prefix = "[>]"
			}
			lines = append(lines, fmt.Sprintf("%s %s", prefix, item.Content))
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return planInjectionHeader + "\n" + strings.Join(lines, "\n")
}

// ActiveCount 返回 (active, total) 两个计数：
//   - active：pending 和 in_progress 状态的条目数（即尚未完成的条目）
//   - total：PlanStore 中的全部条目数
//
// TUI 续跑逻辑（autoExecuting）使用此方法判断是否仍有待执行的条目。
func (s *PlanStore) ActiveCount() (active, total int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	total = len(s.items)
	for _, item := range s.items {
		if item.Status == PlanPending || item.Status == PlanInProgress {
			active++
		}
	}
	return
}
