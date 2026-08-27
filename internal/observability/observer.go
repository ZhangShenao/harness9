// observer.go — OTELEngineObserver 实现 engine.EngineObserver，
// 为每次 interaction 和每个 Turn 创建 OTEL Span，形成三层嵌套的 Span 树：
//
//	interaction (顶层) → turn (每轮) → llm_request / tool (每次 LLM 调用或工具执行)
//
// 关键设计：所有 Span 通过 trace.ContextWithSpan 双写（OTEL 标准 slot + 自定义 key），
// 确保在中间层代码（compaction、session 加载等）可能替换 ctx 的情况下，
// 父子关系链路不会断开，Langfuse 能正确分组显示。
package observability

import (
	"context"
	"fmt"
	"os"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/harness9/internal/engine"
)

// 确保编译期接口检查。
var _ engine.EngineObserver = (*OTELEngineObserver)(nil)

// interactionSpanKey 存储 interaction-level Span 的 ctx key。
type interactionSpanKey struct{}

// turnSpanKey 存储 turn-level Span 的 ctx key。
type turnSpanKey struct{}

// OTELEngineObserver 实现 engine.EngineObserver，
// 用 OTEL Span 覆盖每次 interaction（顶层父节点）和每个 Turn。
type OTELEngineObserver struct {
	tracer     trace.Tracer
	turnsTotal metric.Int64Counter
	forceFlush func(context.Context) error // 直接绑定 SDK TracerProvider.ForceFlush
}

// NewOTELEngineObserver 构造 OTELEngineObserver，初始化 turns 计数器。
// ForceFlush 直接从 Providers 获取，不经过全局 provider wrapper，确保 100% 可用。
func NewOTELEngineObserver(p *Providers) (*OTELEngineObserver, error) {
	turns, err := p.Meter.Int64Counter(MetricTurnsTotal,
		metric.WithDescription("Total agent turns executed"))
	if err != nil {
		return nil, err
	}
	return &OTELEngineObserver{
		tracer:     p.Tracer,
		turnsTotal: turns,
		forceFlush: p.ForceFlush,
	}, nil
}

// OnInteractionStart 启动顶层 interaction Span，注入 session.id 和初始 prompt 属性。
// 同时通过 trace.ContextWithSpan 将 Span 显式写入 OTEL span slot，
// 确保后续 tracer.Start 调用能正确获取父 Span。
func (o *OTELEngineObserver) OnInteractionStart(ctx context.Context, sessionID, prompt string) context.Context {
	ctx, span := o.tracer.Start(ctx, SpanInteraction,
		trace.WithAttributes(attribute.String(AttrSessionID, sessionID)),
	)
	// langfuse.trace.input：用户的初始 prompt，Langfuse v4 trace 级别 Input 字段。
	span.SetAttributes(attribute.String(AttrLangfuseTraceInput, truncateAttr(prompt)))
	// 将 span 同时存入自定义 key（供 OnInteractionEnd 取用）
	// 和 OTEL 标准 slot（供下级 tracer.Start 自动寻找父节点）。
	ctx = trace.ContextWithSpan(ctx, span)
	return context.WithValue(ctx, interactionSpanKey{}, span)
}

// OnInteractionEnd 结束 interaction Span，记录总 turns 数。
// 随后立即调用 ForceFlush，确保本次 interaction 的所有 span 在等待 batcher 定时前就被推送。
func (o *OTELEngineObserver) OnInteractionEnd(ctx context.Context, turns int, err error) {
	span, _ := ctx.Value(interactionSpanKey{}).(trace.Span)
	if span == nil {
		return
	}
	if err != nil {
		span.RecordError(err)
	}
	span.SetAttributes(attribute.Int(AttrTurnNumber, turns))
	span.End()

	// ForceFlush：立即将当前 batcher 中的所有已结束 span 推送到后端。
	// 避免 span 因等待 batcher 定时触发（2s）而延迟上报，尤其对短 session 尤为重要。
	if o.forceFlush != nil {
		flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if ferr := o.forceFlush(flushCtx); ferr != nil {
			fmt.Fprintf(os.Stderr, "[OTEL] ForceFlush 失败: %v\n", ferr)
		}
	}
}

// OnTurnStart 启动 turn-level Span（interaction Span 的子节点），将其存入 ctx。
// 在调用 tracer.Start 之前，先将 interaction Span 显式恢复到 OTEL slot，
// 确保 turn Span 的 parent_span_id 正确指向 interaction Span。
func (o *OTELEngineObserver) OnTurnStart(ctx context.Context, turn int) context.Context {
	// 从自定义 key 取出 interaction span，显式设为 OTEL 当前 span。
	// 这样即使中间层（compaction、session 加载等）替换了 OTEL slot，父节点也不会丢失。
	if iSpan, ok := ctx.Value(interactionSpanKey{}).(trace.Span); ok && iSpan.SpanContext().IsValid() {
		ctx = trace.ContextWithSpan(ctx, iSpan)
	}
	ctx, span := o.tracer.Start(ctx, SpanTurn,
		trace.WithAttributes(attribute.Int(AttrTurnNumber, turn)),
	)
	ctx = trace.ContextWithSpan(ctx, span)
	return context.WithValue(ctx, turnSpanKey{}, span)
}

// OnTurnEnd 结束 turn Span，增加 turns 计数。
func (o *OTELEngineObserver) OnTurnEnd(ctx context.Context, turn int, hasToolCalls bool) {
	span, _ := ctx.Value(turnSpanKey{}).(trace.Span)
	if span != nil {
		span.SetAttributes(attribute.Bool("turn.has_tool_calls", hasToolCalls))
		span.End()
	}
	o.turnsTotal.Add(ctx, 1)
}

// OnStateChange 将状态流转写入当前 turn Span 属性。
// Span 名沿用 turnSpanKey 存放的 turn-level Span（interaction 下级节点）。
func (o *OTELEngineObserver) OnStateChange(ctx context.Context, from, to engine.LoopState, turn int) {
	if span, _ := ctx.Value(turnSpanKey{}).(trace.Span); span != nil {
		span.SetAttributes(attribute.Int(AttrTurnNumber, turn))
		span.SetAttributes(attribute.String("harness9.loop.state_transition",
			string(from)+"->"+string(to)))
	}
}
