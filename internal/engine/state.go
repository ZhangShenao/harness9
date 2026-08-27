// LoopState 显式状态机 —— runLoop 执行阶段的单一事实源。
//
// LoopState 将 runLoop 的执行阶段显式化：熔断检查点可按状态精准生效，
// 可观测层（OTEL Span 属性）与客户端事件流共享同一份阶段事实源。
// 状态变量是 runLoop 的局部值而非引擎字段，多实例并发天然无竞态，
// 延续 session/planMode 快照式读取的隔离哲学。
package engine

import (
	"fmt"
	"log"

	"github.com/harness9/internal/logfmt"
)

// LoopState 表示 runLoop 的执行阶段。所有流转经 setState 单一入口汇聚到
// observer 回调与流式事件通道，不允许绕过。
type LoopState string

const (
	// StateIdle runLoop 尚未启动。
	StateIdle LoopState = "idle"
	// StateTurnStart Turn 边界：计数递增、硬熔断检查点所在阶段。
	StateTurnStart LoopState = "turn_start"
	// StateCompacting 压缩与 token 估算阶段。
	StateCompacting LoopState = "compacting"
	// StateGenerating LLM 调用阶段。
	StateGenerating LoopState = "generating"
	// StateToolExecuting 工具并发执行阶段。
	StateToolExecuting LoopState = "tool_executing"
	// StateDone 自然完成退出。
	StateDone LoopState = "done"
	// StateTerminated 受控熔断退出。
	StateTerminated LoopState = "terminated"
)

// legalTransitions 是单向合法流转表。目标态缺失表示终态。
var legalTransitions = map[LoopState][]LoopState{
	StateIdle:          {StateTurnStart},
	StateTurnStart:     {StateCompacting, StateTerminated},
	StateCompacting:    {StateGenerating, StateTerminated},
	StateGenerating:    {StateToolExecuting, StateDone, StateTerminated},
	StateToolExecuting: {StateTurnStart},
	StateDone:          {},
	StateTerminated:    {},
}

// isLegalTransition 校验一次状态流转是否在合法流转表中。
func isLegalTransition(from, to LoopState) bool {
	for _, s := range legalTransitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

// transition 尝试流转并返回新状态；非法流转被拒绝并记录告警日志（防御性，
// 保证编码失误不会击穿主循环），返回原状态。
func transition(from, to LoopState, turn int) LoopState {
	if !isLegalTransition(from, to) {
		log.Print(logfmt.FormatMsg("engine", fmt.Sprintf(
			"警告：非法状态流转 %s -> %s (turn %d)，已拒绝", from, to, turn)))
		return from
	}
	return to
}
