// Package engine — TaskGate：轮边界控制门。
//
// 后台子代理的暂停/转向挂接点。接口定义在使用者侧（engine 包），
// 由 subagent.TaskController 等实现者经 WithTaskGate 注入。
// 语义：runLoop 在每个 Turn 开始前（beginTurn 之前）调用 AwaitTurn——
// 实现方在暂停状态下阻塞，恢复后放行并返回排队中的转向消息；
// 引擎把转向消息以 user 角色持久化到历史后继续循环。
package engine

import "context"

// TaskGate 是引擎轮边界控制门。
type TaskGate interface {
	// AwaitTurn 在暂停时阻塞（直到 Resume 或 ctx 结束），返回排队中的转向消息。
	// ctx 结束时返回 ctx 的错误；正常放行返回已排队的 steer 消息（可为空切片）。
	AwaitTurn(ctx context.Context) ([]string, error)
}

// WithTaskGate 为引擎注入轮边界控制门。nil 或不设置 = 零开销（主引擎路径）。
func WithTaskGate(g TaskGate) Option {
	return func(e *AgentEngine) { e.taskGate = g }
}
