// Package tools — plan_write 工具（执行计划读写 + 防作弊校验）。
//
// PlanWriteTool 是 Planning 模块对 LLM 暴露的唯一计划管理接口。
// 工具有两种调用模式：
//   - 写模式（提供 steps 数组）：全量替换当前计划，内置防作弊校验。
//   - 读模式（省略 steps 字段）：返回当前计划 JSON，不修改状态。
//
// 防作弊校验设计：
// 一次调用中最多允许 1 个 pending/新条目直接跳转到 completed，
// 超过 1 个视为"幻觉执行"（LLM 未做实际工作但伪造进度），拒绝写入并回传错误。
// cancelled → completed 的转换始终被拒绝（需先恢复为 pending/in_progress）。
// 阈值设为 1 而非 0：保留 LLM 完成实际工作后直接标记完成的正常用法，
// 同时阻止批量伪造（原始 bug 场景：11 个条目中 9 个被一次性批量完成）。
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/harness9/internal/logfmt"
	"github.com/harness9/internal/planning"
	"github.com/harness9/internal/schema"
)

// PlanWriteOption 配置 PlanWriteTool 的可选行为。
type PlanWriteOption func(*PlanWriteTool)

// WithPlanWriter 注入 PlanWriter，每次写入 PlanStore 成功后调用 Write。
// pw 为 nil 时等同于不注入（无操作）。
func WithPlanWriter(pw planning.PlanWriter) PlanWriteOption {
	return func(t *PlanWriteTool) { t.planWriter = pw }
}

// PlanWriteTool 实现 BaseTool 接口，允许 LLM 维护当前任务的执行计划。
// 内部通过 *planning.PlanStore 存取计划状态，PlanStore 本身是线程安全的。
// 传入 steps 数组时全量替换并执行防作弊校验；省略 steps 时仅读取当前计划。
type PlanWriteTool struct {
	// store 是会话内共享的计划存储，由 main.go 创建后注入引擎和此工具。
	store      *planning.PlanStore
	planWriter planning.PlanWriter // 可选，nil 时跳过
}

// NewPlanWriteTool 创建绑定到指定 PlanStore 的工具实例。
// store 不得为 nil，否则 Execute 调用时会发生 panic。
// opts 为可选配置，当前支持 WithPlanWriter 注入计划文件写入器。
func NewPlanWriteTool(store *planning.PlanStore, opts ...PlanWriteOption) *PlanWriteTool {
	t := &PlanWriteTool{store: store}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

// Name 返回工具标识符 "plan_write"。
func (t *PlanWriteTool) Name() string { return "plan_write" }

// Definition 返回工具元信息，包含描述和 JSON Schema 参数定义。
func (t *PlanWriteTool) Definition() schema.ToolDefinition {
	return schema.ToolDefinition{
		Name: "plan_write",
		Description: "创建或更新当前任务的执行计划（权威状态）。" +
			"提供 steps 数组时全量替换（atomic replace）；省略 steps 时读取当前计划。\n" +
			"面对复杂多步任务时，先制定计划再逐步执行：开始某条目前将其标记为 in_progress，" +
			"完成后立即标记为 completed。计划在上下文压缩后依然可见，以计划为准继续执行。",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"steps": map[string]interface{}{
					"type":        "array",
					"description": "完整的计划条目列表（全量替换）。省略此字段则仅读取当前计划。",
					"items": map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"id":      map[string]interface{}{"type": "string"},
							"content": map[string]interface{}{"type": "string"},
							"status": map[string]interface{}{
								"type": "string",
								"enum": []string{"pending", "in_progress", "completed", "cancelled"},
							},
						},
						"required": []string{"id", "content", "status"},
					},
				},
			},
		},
	}
}

// planWriteArgs 定义 plan_write 工具的 JSON 参数结构。
// Steps 字段省略或显式传 null 时，json.Unmarshal 将其设为 nil；
// Execute 通过 len(input.Steps) > 0 区分读操作（nil/空切片）和写操作（非空列表）。
type planWriteArgs struct {
	Steps []planning.PlanItem `json:"steps"`
}

// Execute 处理 plan_write 工具调用：
//   - 写操作（steps 非空）：执行防作弊校验后全量替换 PlanStore，返回写入后的计划 JSON。
//   - 读操作（steps 为空/省略）：直接返回当前 PlanStore 的计划 JSON，不修改状态。
//
// 防作弊校验逻辑：
//  1. 遍历新列表中所有 status == completed 的条目；
//  2. cancelled → completed：始终拒绝（不受"单个允许"规则豁免）；
//  3. pending/新建 → completed：计为 directCompletions，超过 1 个则拒绝整批写入；
//  4. in_progress → completed / completed → completed：合法路径，不计入 directCompletions。
//
// 返回的 JSON 始终是数组格式（空列表时为 "[]" 而非 "null"）。
func (t *PlanWriteTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	var input planWriteArgs
	if err := json.Unmarshal(args, &input); err != nil {
		return "", fmt.Errorf("参数解析失败：%w", err)
	}

	var current []planning.PlanItem
	if len(input.Steps) > 0 {
		// ---- 防作弊校验（Anti-Cheat Validation） ----
		// 读取写入前的当前状态快照，用于判断每个条目的历史状态。
		// 快照在校验期间不会改变（PlanStore.Read 返回副本），确保校验一致性。
		prev := t.store.Read()
		prevStatus := make(map[string]planning.PlanStatus, len(prev))
		for _, item := range prev {
			prevStatus[item.ID] = item.Status
		}

		var directCompletions int
		for _, item := range input.Steps {
			if item.Status != planning.PlanCompleted {
				// 非 completed 状态的条目无需校验（任意状态转换均允许）。
				continue
			}
			prior, exists := prevStatus[item.ID]
			if !exists || prior == planning.PlanPending {
				// 情况 A：新建条目或 pending → completed。
				// 单个允许（LLM 可能真实完成了工作后直接记录结果），但批量超过 1 个视为作弊。
				directCompletions++
				continue
			}
			if prior == planning.PlanCancelled {
				// 情况 B：cancelled → completed 始终拒绝。
				// 取消的条目表明已放弃，需经用户重新评估（恢复为 pending/in_progress）才能完成。
				return "", fmt.Errorf(
					"计划条目 %q 已取消，不能直接标记为 completed；如需重新执行，请先将其恢复为 pending 或 in_progress。",
					item.ID)
			}
			// 情况 C：in_progress → completed 或 completed → completed，合法，不计入计数。
		}
		if directCompletions > 1 {
			// 超过 1 个条目在未经 in_progress 阶段的情况下直接完成，判定为批量幻觉执行。
			// 返回错误让 LLM 感知并重新组织写入（自愈机制：错误回传给 LLM，不终止 agent loop）。
			return "", fmt.Errorf(
				"不允许在一次调用中将 %d 个计划条目直接标记为 completed（未经 in_progress）。"+
					"请逐一处理：每次仅完成一项实际工作后更新该条目状态。",
				directCompletions)
		}
		current = t.store.Write(input.Steps)
		if t.planWriter != nil {
			if err := t.planWriter.Write(current); err != nil {
				log.Print(logfmt.FormatMsg("plan_write", fmt.Sprintf("写入计划文件失败: %v", err)))
			}
		}
	} else {
		// 读操作：不修改 PlanStore，直接返回当前快照。
		current = t.store.Read()
	}

	// 将 nil 切片规范化为空切片，确保序列化结果为 "[]" 而非 "null"，
	// 符合 LLM 工具调用规范（空列表应明确表达，而非空指针）。
	if current == nil {
		current = []planning.PlanItem{}
	}

	b, err := json.Marshal(current)
	if err != nil {
		return "", fmt.Errorf("序列化计划列表失败：%w", err)
	}
	return string(b), nil
}
