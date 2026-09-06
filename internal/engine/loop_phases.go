// Package engine — runLoop 的阶段化实现。
//
// runLoop 编排器（agent_loop.go）按阶段调用本文件定义的私有方法，每个方法
// 对应 ReAct 循环中的一个清晰阶段：
//
//	beginInteraction   → 初始化：observer 接入、状态快照、Todo 恢复、Plan 前缀、历史加载
//	beginTurn          → Turn 前置：计数、observer 回调、双重终止判定（MaxTurns / ctx 取消）
//	prepareTurnInput   → 上下文预处理：工具过滤、压缩检查、token 估算上报、nudge 注入
//	generateTurn       → LLM 调用：带重试生成、实际用量上报、响应追加到完整历史
//	trackStall         → 停滞检测：进展工具计数维护
//	injectObservations → 结果注入：工具执行结果作为 Observation（user 角色）追加
//	saveHistory        → 收尾：本次 Run 新增消息持久化（仅自然终止路径）
//	savePlan          → 收尾：PlanStore 持久化（所有退出路径，defer 保证）
//
// 并发模型：loopContext 的字段仅在 runLoop 所在 goroutine 中读写（emitter 内部
// 回调除外，其自身保证并发安全），因此无需加锁；与 TUI goroutine 的隔离通过
// 入口处对 AgentEngine 字段的一次性快照实现。
package engine

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/harness9/internal/logfmt"
	"github.com/harness9/internal/memory"
	"github.com/harness9/internal/planning"
	"github.com/harness9/internal/schema"
)

// loopContext 聚合单次交互（一次 Run / RunStream 调用）的全部可变状态。
// 引入它是为了消除阶段方法之间 6+ 参数的层层透传；engine 反向持有使阶段方法
// 可以同时访问引擎配置（如 contextWindow）与交互状态（如 turns）。
type loopContext struct {
	engine    *AgentEngine
	logPrefix string
	em        emitter

	// 以下字段在入口一次性快照自 AgentEngine，运行期不受 SetSession/SetPlanMode
	// 等跨 goroutine 修改的影响（修改仅对下一次交互生效）。
	sess      memory.Session
	comp      memory.Compactor
	planStore *planning.PlanStore
	planMode  planning.PlanMode

	obs                EngineObserver   // 非 nil：入口已兜底为 noopObserver
	obsCtx             context.Context  // interaction span 注入后的 ctx，所有 Turn ctx 的祖先
	history            []schema.Message // 引擎本地的完整历史（含 system prompt 与本次全部新增消息）
	startLen           int              // 本次 Run 新增消息在 history 中的起始下标（持久化边界）
	turns              int              // 已进入的 Turn 计数（含被 MaxTurns 拒绝的那一轮）
	turnsSinceProgress int              // 自上次进展工具调用以来的轮数（驱动停滞 nudge）
	interactionErr     error            // 记录导致交互非正常结束的错误，供 OnInteractionEnd 上报
	overallStart       time.Time
}

// turnInput 是单次 LLM 调用的完整输入。
type turnInput struct {
	// history 是发送给 LLM 的历史视图：在完整历史之上应用压缩与 nudge 注入后的副本，
	// 仅作用于当次调用——lc.history 保持完整增长，不因压缩或 nudge 而改变。
	history []schema.Message
	// toolDefs 是本 Turn 对 LLM 可见的工具定义（Plan Mode 下已过滤为只读白名单）。
	toolDefs []schema.ToolDefinition
}

// beginInteraction 完成 runLoop 入口的全部一次性准备，返回携带快照状态的 loopContext。
// 此方法不返回错误：所有可失败步骤（Todo 恢复、历史加载）均降级处理并记录告警，
// 保证 Run/RunStream 的失败语义完全由循环阶段决定（与重构前行为一致）。
func (e *AgentEngine) beginInteraction(ctx context.Context, userPrompt, logPrefix string, em emitter) *loopContext {
	lc := &loopContext{
		engine:       e,
		logPrefix:    logPrefix,
		em:           em,
		overallStart: time.Now(),
	}

	// 可观测层接入：若未注入 observer 则退化为 noop。
	lc.obs = e.observer
	if lc.obs == nil {
		lc.obs = noopObserver{}
	}

	// 单独加读锁读取 sessionID 用于 span 属性（与下方状态快照的锁分离，避免持锁时间过长）。
	e.mu.RLock()
	var sessIDForObs string
	if e.session != nil {
		sessIDForObs = e.session.SessionID()
	}
	e.mu.RUnlock()
	lc.obsCtx = lc.obs.OnInteractionStart(ctx, sessIDForObs, userPrompt)
	// 后续所有阶段（Todo 恢复 / 历史加载 / Turn / 收尾）统一使用 obsCtx：
	// observer 可能在 OnInteractionStart 中注入 Span 或其他值，若继续使用原始 ctx，
	// 这些注入会丢失（OTELEngineObserver 的 interaction→turn Span 父子关系即依赖此传播）。
	ctx = lc.obsCtx

	// 在循环开始时快照 session/compactor/planMode/planStore，避免与 TUI goroutine 的
	// SetSession/SetPlanMode 产生数据竞争。
	e.mu.RLock()
	lc.sess = e.session
	lc.comp = e.compactor
	lc.planMode = e.planMode
	lc.planStore = e.planStore
	e.mu.RUnlock()

	// 启动时从 Session 恢复 PlanStore 状态（跨会话续接未完成任务）。
	// 失败不终止 Run：plan 是辅助状态，丢失可接受，仅记录告警。
	if lc.sess != nil && lc.planStore != nil {
		if plan, err := lc.sess.GetPlan(ctx); err != nil {
			log.Print(logfmt.FormatMsg(logPrefix, fmt.Sprintf("加载 plan 失败: %v", err)))
		} else {
			lc.planStore.Write(plan)
		}
	}

	// Plan Mode：注入规划行为约束（write_file/edit_file 已由 filterReadOnlyTools 在工具层
	// 硬性过滤，此处只补充 bash 只读限制和 plan_write 输出要求等无法在工具层表达的行为规则）。
	userPrompt = applyPlanModePrefix(lc.planMode, userPrompt)

	lc.history, lc.startLen = e.loadHistoryWith(ctx, userPrompt, lc.sess, logPrefix)
	return lc
}

// beginTurn 执行每个 Turn 的前置动作：计数递增、observer 回调、双重终止判定。
// 返回本 Turn 的增强 ctx（LLM 调用与工具执行均继承此 ctx）。
// 命中 MaxTurns 或 ctx 取消时返回错误——这是三重终止保障中的两重
// （第三重"自然终止"在 generateTurn 之后的调用方处判定）。
func (lc *loopContext) beginTurn(ctx context.Context) (context.Context, error) {
	lc.turns++
	// OnTurnStart 先于终止判定调用：即使本轮被拒绝，观察者的 Span 计数仍保持配平。
	turnCtx := lc.obs.OnTurnStart(ctx, lc.turns)

	if lc.engine.maxTurns > 0 && lc.turns > lc.engine.maxTurns {
		lc.interactionErr = fmt.Errorf("已达最大 Turn 数 (%d)，循环终止", lc.engine.maxTurns)
		return nil, lc.interactionErr
	}
	select {
	case <-ctx.Done():
		lc.interactionErr = fmt.Errorf("context 已取消：%w", ctx.Err())
		return nil, lc.interactionErr
	default:
	}
	return turnCtx, nil
}

// prepareTurnInput 完成每轮 LLM 调用前的上下文预处理（保持以下执行顺序，
// 与压缩通知 → token 上报 → nudge 注入的对外事件顺序一致）：
//
//  1. 工具过滤：读取本 Turn 可用工具列表，Plan Mode 下过滤为只读白名单
//  2. 压缩检查：超过阈值时生成本轮的压缩视图，并上报压缩详情
//  3. token 估算上报：压缩后消息 + 工具定义的估算值（LLM 调用后由实际用量覆盖）
//  4. nudge 注入：记忆 nudge（周期性）与停滞 nudge（无进展检测），
//     均只注入发送副本，绝不写入 lc.history（因此不会被持久化、不会累积）
//
// 压缩审计记录在方法内部经 em.compaction 上报后即完成使命，不再外泄给调用方。
func (lc *loopContext) prepareTurnInput() turnInput {
	e := lc.engine

	// 1. 工具列表每轮重新读取：注册表内容可能在运行期变化（如 MCP 异步注入）。
	availableTools := e.registry.GetAvailableTools()
	if lc.planMode == planning.PlanModePlan {
		availableTools = filterReadOnlyTools(availableTools)
	}

	// 2. 压缩检查：comp 为 nil 时原样返回。压缩是逐轮视图——结果仅用于本次调用，
	//    lc.history 保留完整历史，保证下一轮压缩仍能看到全部上下文重新计算。
	compactedHistory, record := e.applyCompactionWith(lc.comp, lc.history)
	if record != nil && record.Tier != memory.TierNone {
		lc.em.compaction(*record)
	}

	// 3. token 用量上报（估算值）：字符数÷4 估算，供 TUI 在 LLM 响应前先行展示。
	totalTokens := memory.EstimateTokens(compactedHistory) + memory.EstimateToolTokens(availableTools)
	lc.em.tokenUpdate(totalTokens, e.contextWindow)

	// 4a. 记忆 nudge：每隔 nudgeInterval 轮注入一次长期记忆提示。
	if e.nudgeInterval > 0 && e.nudgeText != "" && lc.turns%e.nudgeInterval == 0 {
		compactedHistory = appendUserNudge(compactedHistory, e.nudgeText)
	}

	// 4b. 停滞 nudge：连续 stallWindow 轮未调用进展工具（只在静态重读/grep 空转）时，
	// 注入一次提示打断空转，并重置计数，避免每轮重复刷屏。
	if e.stallWindow > 0 && e.stallText != "" && lc.turnsSinceProgress >= e.stallWindow {
		compactedHistory = appendUserNudge(compactedHistory, e.stallText)
		lc.turnsSinceProgress = 0
	}

	return turnInput{history: compactedHistory, toolDefs: availableTools}
}

// generateTurn 执行一次带重试的 LLM 调用（计时仅覆盖生成本身）。
// 成功后依次：以上报实际 token 用量（覆盖估算值）、把响应追加到 lc.history
// （完整历史，而非压缩/nudge 副本）。失败时记录 interactionErr 并返回包装后的错误。
func (lc *loopContext) generateTurn(turnCtx context.Context, in turnInput) (*schema.Message, time.Duration, error) {
	llmStart := time.Now()
	responseMsg, usage, err := lc.engine.generateWithRetry(turnCtx, lc.em, lc.turns, lc.logPrefix, in.history, in.toolDefs)
	if err != nil {
		lc.interactionErr = err
		return nil, 0, fmt.Errorf("模型生成失败 (turn %d): %w", lc.turns, err)
	}
	llmDuration := time.Since(llmStart)

	// 用实际 API 返回的 token 用量更新显示，替代 prepareTurnInput 阶段的估算值。
	if usage != nil && usage.InputTokens > 0 {
		lc.em.tokenUpdate(usage.InputTokens, lc.engine.contextWindow)
	}

	// 响应追加到完整历史：压缩副本只影响当次 LLM 输入，本地历史必须完整增长，
	// 否则后续轮次将丢失本条 assistant 消息（Anthropic 交替约束也会被破坏）。
	lc.history = append(lc.history, *responseMsg)
	return responseMsg, llmDuration, nil
}

// trackStall 更新停滞计数：本轮调用了进展工具（write_file/edit_file）则归零，否则累加。
// 计数驱动 prepareTurnInput 中的停滞 nudge 注入。
func (lc *loopContext) trackStall(calls []schema.ToolCall) {
	if hasProgressTool(calls) {
		lc.turnsSinceProgress = 0
	} else {
		lc.turnsSinceProgress++
	}
}

// injectObservations 将工具执行结果作为 Observation（user 角色）逐条注入历史并返回新历史。
// 顺序与 toolCalls 一致（executeTools 已按索引写入保证）。
// 空输出兜底为占位文案：部分 backend 拒绝空 content 的 tool_result（返回 400，
// 在无重试时直接杀实例），且空 Observation 无信息可供模型推理、浪费一轮。
func injectObservations(history []schema.Message, toolCalls []schema.ToolCall, results []schema.ToolResult) []schema.Message {
	for i, toolCall := range toolCalls {
		content := results[i].Output
		if content == "" {
			content = "[工具执行完成，无输出]"
		}
		history = append(history, schema.Message{
			Role:       schema.RoleUser,
			Content:    content,
			ToolCallID: toolCall.ID,
			// 透传结构化错误信号，供 Provider 设置 tool_result.is_error，强化自愈。
			IsError: results[i].IsError,
		})
	}
	return history
}

// saveHistory 将本次 Run 新增的消息（history[startLen:]）写回 Session。
// 仅在自然终止路径调用（MaxTurns / ctx 取消 / LLM 失败时丢弃整条轨迹，与既有语义一致）。
func (lc *loopContext) saveHistory(ctx context.Context) {
	lc.engine.saveHistoryWith(ctx, lc.sess, lc.history, lc.startLen, lc.logPrefix)
}

// savePlan 将 PlanStore 持久化到 Session（write-replace）。
// 以 defer 注册、在所有退出路径执行；失败仅记录告警，不影响 Run 结果。
func (lc *loopContext) savePlan(ctx context.Context) {
	if lc.sess == nil || lc.planStore == nil {
		return
	}
	if err := lc.sess.SavePlan(ctx, lc.planStore.Read()); err != nil {
		log.Print(logfmt.FormatMsg(lc.logPrefix, fmt.Sprintf("保存 plan 失败: %v", err)))
	}
}
