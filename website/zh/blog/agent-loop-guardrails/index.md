---
title: "Agent 失控时，谁来踩刹车？"
date: 2026-08-31
tags: [harness9, agent, golang, agent-loop, guardrails]
summary: "harness9 在 PR #106 中为 Agent Loop 落地三层行为护栏：loopGuard 四维熔断、七态显式状态机、Reminder 三源仲裁。这篇文章用真实代码讲清每一层为什么这样设计。"
---

# 给一路狂飙的模型踩刹车

## 关于 harness9

harness9 是一款 Local-First、轻量级、功能完备、生产可用的通用 Go Agent 框架。

- **官网**：[https://zhangshenao.github.io/harness9/zh/](https://zhangshenao.github.io/harness9/zh/)
- **GitHub**：[https://github.com/ZhangShenao/harness9](https://github.com/ZhangShenao/harness9)

⭐ Star 是对开源工作最直接的支持，欢迎提 Issue 和 PR。

---

## TL;DR

- 模型的"完成"只是概率输出，并不是保证。停止权必须握在 Harness 手里
- 所有熔断策略都收敛到 loopGuard 守护对象中：MaxTurns、SessionTimeout、Token Budget、重复死循环四条硬闸，只在 Turn 边界裁决，并不中断流式输出
- 七态显式状态机让循环每一步有明确的状态：可观测（OTEL + 事件流）、可裁决（熔断按状态精准生效），且状态是局部值，天然无锁
- Reminder 先劝后斩：同一工具调用反复出现，先注入一条带事实定位的定向提醒，再犯就硬终止——黄牌，然后红牌
- 所有提醒只写进"递给模型的复印件"，正式对话历史一个字不动
- 所有受控熔断走同一个 terminate 出口：顺手修掉了"熔断后几十轮轨迹丢失"的遗留 Bug

## 本文你将学到

> - 你将看清为什么"不完全信任模型"是 Harness 的本职，以及四条熔断硬闸各自的裁决时机
> - 你将理解熔断为什么只在 Turn 边界动手，单个工具如何被墙钟 deadline 钳制
> - 你将掌握七态状态机的流转表，以及"局部值、无锁"背后的并发考量
> - 你将看清重复签名（sha256 + canonical JSON）如何识别死循环，进展工具为什么能打破计数
> - 你将理解提醒的防御性副本语义，和统一 terminate 出口修复的轨迹丢失缺陷

## Agent 会怎么失控？

Agent 干活的样子是一个循环：想一想 → 动手 → 看结果 → 再想。

这就是推理行动循环（ReAct Loop），每转一圈叫一轮（Turn）。理论上，圈的终点是模型自己说"任务完成"。

实际上呢？harness9 项目跑 SWE-bench 时抓到过典型的 Trajectory bad-case：Agent 反复读同一批文件、跑同一条命令，代码一行没改，一路空转烧满 80 轮才被轮数上限拦下。xarray-3364 和 pylint-7080 两个实例，全栽在这个形态上。

空转烧的不是时间，是真金白银。每一轮都要把完整上下文喂给 LLM，80 轮就是 80 次全量调用。

为了解决这个问题，我们把真实轨迹里的失控形态拆成三类，痛点优先级排得干脆——卡死型 > 烧钱型 > 超时型：

| 失控形态 | 表现 | 代价 |
|---------|------|------|
| 卡死型 | 同一工具调用无限重复 | 时间全浪费，任务必败 |
| 烧钱型 | 每轮全量上下文进 LLM | Token 账单线性上涨 |
| 超时型 | 单次 Run 没有时间上限 | 墙钟（Wall-clock）失控 |

![图：Agent 死循环空转，烧满 80 轮](/website/public/blog/agent-loop-guardrails/images/runaway-loop-01.png)


## 谁来踩刹车？

拳击比赛里，选手可以认输，但裁判随时有权随时终止比赛——裁判的终止权，不依赖选手同意。

这就是 harness9 的核心立场：模型的自律是请求，不是保证，Harness 必须持有独立于 LLM 的裁判权。放弃裁判权的 Agent 框架，等于让选手自己决定比赛何时结束。

落到实现上，我们做了一个关键的架构决策：把全部护栏状态集中到一个守护对象 loopGuard 里。runLoop 主循环只在固定检查点问它一句话"要停吗"，不再散落一堆 if 判断。

四条硬闸在 GuardConfig 里一目了然：

```go
// internal/engine/loop_guard.go
type GuardConfig struct {
    MaxTurns            int           // <=0 关闭
    RunTimeout          time.Duration // <=0 关闭
    TokenBudget         int           // <=0 关闭
    RepetitionWindow    int           // <=0 关闭
    RepetitionThreshold int
    // ... StallWindow / StallText / MemoryInterval / MemoryText
}
```

注释里那句"零值字段一律表示该护栏关闭"是硬约束：不开启任何新选项，引擎行为与引入护栏前完全一致。向后兼容不是口号，是类型设计。

| 熔断轴 | 配置项 | 默认值 | 裁决点 |
|-------|--------|-------|--------|
| 自然终止 | — | — | `ToolCalls == 0` |
| MaxTurns | `WithMaxTurns` | 500 | Turn 开始 |
| 墙钟超时 | `WithRunTimeout` | 不限 | Turn 开始 |
| Token 预算 | `WithTokenBudget` | 不限 | Turn 开始 |
| 重复死循环 | `WithRepetitionReminder(window, threshold)` | 关闭 | Turn 开始 |

![图：loopGuard 四维熔断与 Turn 边界检查点](/website/public/blog/agent-loop-guardrails/images/loop-guard-four-fuses-02.png)


### 为什么在 Turn 边界动手？

熔断有个天然矛盾：掐得越早越好，但此时 Stream 正在输出 token，中途掐断等于把完整的结果截断，可能会产生无法预期的行为。

harness9 的取舍直接写进了 loopGuard 的包注释：**硬熔断只在 Turn 边界裁决，预留最多一轮的 Buffer，绝不在流式中途撕断。**

代价是明确的：最多过载一轮。收益也是明确的：消费者看到的每一条回复都是完整的。

一个 Turn 内可能跑几十秒，墙钟到了那轮还没结束怎么办？答案是把工具的超时预算钳制到剩余时间：

```go
// internal/engine/agent_loop.go（runLoop 内）
toolBudget := e.toolTimeout
if rem, ok := guard.Remaining(); ok && (toolBudget <= 0 || rem < toolBudget) {
    toolBudget = rem // 单个工具不得冲破墙钟 deadline
}
```

每个工具的子 context 拿到的是 `min(toolTimeout, remaining)`。deadline 是真的 deadline，不因工具执行而失守。

Token 预算同样讲究：优先累计 API 实际返回的 `usage.InputTokens`；Provider 不吐 usage 时，用本轮发给模型的上下文估算值兜底。预算约束不因 Provider 差异而失效。

![图：Turn 边界裁决与工具超时熔断](/website/public/blog/agent-loop-guardrails/images/turn-boundary-checkpoint-03.png)


## 循环现在在哪一步？

在外卖系统下单时，你能随时查看订单的状态：已下单、制作中、配送中、已送达。

但是，harness9 优化前的 runLoop 做不到这一点。它是几百行顺序代码，用户并不知道当前 Agent 执行到了哪个阶段。熔断检查点埋在哪、为什么埋在那，没有可引用的答案。

因此，我们把 harness9 的执行阶段显式化：七态状态机（Explicit State Machine），单向流转：

```
idle ──► turn_start ⇄ (compacting ──► generating ──► tool_executing) ──► done | terminated
```

流转表就是代码，没有第二种解读：

```go
// internal/engine/state.go
var legalTransitions = map[LoopState][]LoopState{
    StateIdle:          {StateTurnStart},
    StateTurnStart:     {StateCompacting, StateTerminated},
    StateCompacting:    {StateGenerating, StateTerminated},
    StateGenerating:    {StateToolExecuting, StateDone, StateTerminated},
    StateToolExecuting: {StateTurnStart},
    StateDone:          {},
    StateTerminated:    {},
}
```

两个设计决策值得单独说。

**局部值，无锁。** 状态变量是 runLoop 的局部值，不是引擎字段。多个引擎实例并发跑，天然无竞态条件——这延续了 session、planMode 快照式读取的隔离哲学。

**单一入口，双重扇出。** 所有流转都要经 `setState` 收口，同时通知两个观察者：

```go
// internal/engine/agent_loop.go（runLoop 内，有删节）
setState := func(to LoopState) {
    next := transition(state, to, turnCount)
    // ... 非法流转被 transition 拒绝并打告警，返回原状态
    if em.stateChanged != nil {
        em.stateChanged(StateChangeData{From: from, To: next, Turn: turnCount})
    }
    obs.OnStateChange(ctx, from, next, turnCount) // 接 OTEL Span 属性
}
```

一条路给可观测层（OTEL Span 属性），一条路给流式事件 `EventStateChange`（推给 TUI）。非法流转直接拒绝并打告警日志，编码失误击不穿主循环。

TUI 拿到 `EventStateChange` 后做了件小但聪明的事：spinner 动词前进一步。原来靠定时器轮换的加载动画，现在由真实状态驱动。状态机成了动画的事件源——可观测性不只给监控看，也给屏幕前的用户看。

![图：LoopState 七态状态机](/website/public/blog/agent-loop-guardrails/images/loop-state-machine-04.png)


## 先提醒，再熔断

足球裁判不直接掏红牌：先黄牌警告，再犯才罚下。

harness9 对死循环也是这个节奏。这套干预机制叫提醒（Reminder），第一步是认出"同一个调用"。签名（Signature）算法很直接：

```go
// internal/engine/loop_guard.go
func computeSignature(tc schema.ToolCall) turnSignature {
    h := sha256.New()
    h.Write([]byte(tc.Name))
    args := tc.Arguments
    var v any
    if err := json.Unmarshal(args, &v); err == nil {
        if b, err := json.Marshal(v); err == nil {
            args = b // canonical 化：消除键序与空白差异
        }
    }
    h.Write(args)
    // ...
}
```

签名 = `sha256(工具名 + canonical JSON 参数)`。参数经 map 往返序列化，键序、空白差异全部抹平——`{"city":"北京"}` 和 `{ "city": "北京" }` 是同一个签名。参数不是合法 JSON 时退化为原始字节哈希，fail-open，畸形参数不阻塞循环。

### 三源仲裁

每轮至多注入一条干预消息，优先级固定：**重复 > 停滞 > 记忆**。

```go
// internal/engine/loop_guard.go（EvaluateReminders，有删节）
if sig, total, hit := g.detectTopRepeat(); hit {
    if !g.reminded {
        g.reminded = true // 黄牌
        // ... 取签名的人类可读标签
        return fmt.Sprintf(repetitionReminderFmt, total, label), nil
    }
    g.terminated = &GuardTermination{Reason: ReasonRepetitionLoop}
    return "", fmt.Errorf("重复调用提醒无效（同一调用已出现 %d 次），循环终止", total) // 红牌
}
// ② 停滞：连续 N 轮无进展工具 → 注入停滞提醒，重置计数
// ③ 记忆：每 N 轮 → 注入长期记忆提示
```

黄牌不是一句泛泛的"请不要重复"。检测器知道具体签名，提醒文案带事实定位：

```go
// internal/engine/loop_guard.go
const repetitionReminderFmt = "系统检测：你在当前工作周期内已累计第 %d 次发起相同的工具调用（%s），" +
    "且每次都得到相同结果。继续同一调用不会产生新信息。请择一执行：" +
    "① 改用其他手段获取所需信息；② 基于已有结果推进任务；" +
    "③ 若任务已完成，直接输出最终回复停止。"
```

提醒无效、阈值再次命中，才升级为硬终止 `ReasonRepetitionLoop`。模型始终先拿到一次自救机会。

### 进展打破规则

这里有个容易误伤的坑：SWE-bench 式修复的合法节奏就是"改代码 → 重跑同一条测试命令"。`bash(go test ./...)` 连续出现三四次，可能完全正常。

解法是进展打破：一旦某轮包含 `edit_file` / `write_file`，全部签名计数和停滞计数清零，开启新工作周期。"改一下、跑一次"不会触发；"原地踏步跑十次"才会被拦下。

![图：Reminder 三源仲裁漏斗](/website/public/blog/agent-loop-guardrails/images/reminder-three-source-arbitration-05.png)


![图：签名计算与进展打破规则](/website/public/blog/agent-loop-guardrails/images/signature-progress-break-06.png)



### 提醒会污染历史吗？

不会。把发给模型的历史想成一份复印件：提醒写在复印件页边，原件一个字不动。

```go
// internal/engine/agent_loop.go
func appendUserNudge(history []schema.Message, text string) []schema.Message {
    withNudge := make([]schema.Message, len(history), len(history)+1)
    copy(withNudge, history)
    return append(withNudge, schema.Message{Role: schema.RoleUser, Content: text})
}
```

提醒只追加到当轮发送给 LLM 的防御性副本（Defensive Copy）上。三条语义锁死：不写入 `contextHistory`、不持久化到 Session、不跨轮累积——每轮从零重新裁决。

连续 user 消息会不会捅娄子？Anthropic 对消息序列有严格要求，兼容性由 Provider 的 `convertMessages` 统一处理，引擎层不用管。

## 熔断后轨迹去哪了？

护栏体系顺手修了一个旧缺陷。

优化前的代码里，熔断路径直接 return——绕过了 `saveHistoryWith`。后果：跑了几十轮、烧掉大量 token 的会话，一条消息都没存下来，轨迹无法复盘。你只知道它停了，不知道它干了什么。

本次我们把所有受控终止收敛到一个 terminate 闭包：

```go
// internal/engine/agent_loop.go（runLoop 内）
terminate := func(reason TerminationReason, msg string) error {
    interactionErr = fmt.Errorf("%s", msg)
    setState(StateTerminated)
    if em.terminated != nil {
        em.terminated(TerminationData{Reason: reason, Message: msg})
    }
    log.Print(logfmt.FormatMsg(logPrefix, fmt.Sprintf("受控终止 [%s]: %s", reason, msg)))
    e.saveHistoryWith(ctx, sess, contextHistory, startLen) // 轨迹落库
    return interactionErr
}
```

重置状态 → 发终止事件 → 记日志 → 轨迹落库 → 返回 error。五步，一条路，没有旁支。

`Run` 的返回语义没变（还是 error），但流式消费者多了一个新事件 `EventTerminated`，与 `EventError` 分工明确：前者是设计内的受控终止（受控终止，Controlled Termination），后者是意外故障。TUI 用不同样式渲染——⛔ 和 ❌，用户一眼分清"被护栏拦下"和"真出事了"。

RunStream 里还有个细节：terminate 回调会置位 `terminatedData` 标记，runLoop 返回 error 后据此抑制 `EventError`。同一次终止，不会在终端上渲染两条互斥消息。

终止原因码一共五个，直接展示给用户：

```go
// internal/engine/termination.go
ReasonNatural        = "natural"         // 模型自然停止
ReasonMaxTurns       = "max_turns"       // 达到最大轮数
ReasonRunTimeout     = "run_timeout"     // 墙钟超时
ReasonTokenBudget    = "token_budget"    // Token 预算耗尽
ReasonRepetitionLoop = "repetition_loop" // 重复死循环且提醒无效
```

![图：统一受控终止出口](/website/public/blog/agent-loop-guardrails/images/terminate-unified-exit-07.png)


## 三层防线的分工

回头看，三层护栏不是三个孤立功能，是同一个裁判权的三个动作：

| 层 | 回答的问题 | 动作 |
|----|----------|------|
| 熔断边界 | 极限在哪？ | 越线即停，硬边界 |
| 显式状态机 | 现在哪一步？ | 每步有名字，可观测、可裁决 |
| Reminder | 走偏了吗？ | 先提醒自救，无效再终止 |

三层的时序也咬合得很紧：Reminder 在熔断之前给模型一次自救机会；状态机让每次熔断留下精确现场（哪个状态、哪一轮）；统一出口保证终止本身是干净的——轨迹还在，复盘可做。


![图：三层防线协同全景](/website/public/blog/agent-loop-guardrails/images/three-layer-defense-08.png)


## 结语

护栏的目的不是证明模型不可信，而是给信任划边界：模型负责把任务做好，Harness 负责保证这件事有底。

留一个问题：你用过的 Agent 框架里，停止权在谁手上？

---

