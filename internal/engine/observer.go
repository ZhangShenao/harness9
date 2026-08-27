// Package engine — EngineObserver 接口，供可观测层无侵入接入引擎生命周期。
package engine

import "context"

// EngineObserver 观察 runLoop 的生命周期事件，可在各阶段注入追踪 Span 等上下文信息。
// 所有方法均接收并返回 context.Context，使实现者可将 Span 注入 ctx 供下游（LLM 调用、工具执行）继承。
type EngineObserver interface {
	// OnInteractionStart 在 runLoop 入口调用，返回可携带 Span 的增强 ctx。
	OnInteractionStart(ctx context.Context, sessionID, prompt string) context.Context
	// OnInteractionEnd 在 runLoop 退出时调用（defer 保证执行）。turns 是实际执行的轮数。
	OnInteractionEnd(ctx context.Context, turns int, err error)
	// OnTurnStart 在每个 Turn 开始时调用，返回本 Turn 的增强 ctx（LLM 调用和工具调用均继承此 ctx）。
	OnTurnStart(ctx context.Context, turn int) context.Context
	// OnTurnEnd 在每个 Turn 结束时调用（工具执行完毕后）。
	OnTurnEnd(ctx context.Context, turn int, hasToolCalls bool)
	// OnStateChange 在 runLoop 状态机发生流转时调用。
	OnStateChange(ctx context.Context, from, to LoopState, turn int)
}

// noopObserver 是 EngineObserver 的空实现，所有方法为零开销。
type noopObserver struct{}

func (noopObserver) OnInteractionStart(ctx context.Context, _, _ string) context.Context {
	return ctx
}
func (noopObserver) OnInteractionEnd(_ context.Context, _ int, _ error)     {}
func (noopObserver) OnTurnStart(ctx context.Context, _ int) context.Context { return ctx }
func (noopObserver) OnTurnEnd(_ context.Context, _ int, _ bool)             {}
func (noopObserver) OnStateChange(_ context.Context, _, _ LoopState, _ int) {}
