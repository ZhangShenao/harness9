---
title: "上下文压缩不是删历史 — harness9 怎样让 Agent 一直记得要做什么"
date: 2026-08-11
tags: [harness9, agent, golang, context, compaction, memory]
summary: "harness9 用 ProgressiveCompactor 把上下文压缩拆成四档：先外存，再局部摘要，再全量摘要，最后才截断；结构化 Anchor、工具结果外存、工具对修复和 JSONL 审计一起守住长程 Agent 的连续性。"
---

# 上下文压缩不是删历史 — harness9 怎样让 Agent 一直记得要做什么

## 关于 harness9

harness9 是一款 Local-First、轻量级、功能完备、生产可用的通用 Go Agent 框架。

- **官网**：[https://zhangshenao.github.io/harness9/zh/](https://zhangshenao.github.io/harness9/zh/)
- **GitHub**：[https://github.com/ZhangShenao/harness9](https://github.com/ZhangShenao/harness9)

⭐ Star 是对开源工作最直接的支持，欢迎提 Issue 和 PR。

---

## TL;DR

- 上下文压缩是一个渐进式的行为，压缩比例取决于上下文窗口预算，`ProgressiveCompactor` 就是要实现这个核心机制。
- 60% 的预警档只把旧 `tool_result` 外存，不花一次 LLM 调用，也不改写对话结构。
- 软压缩只摘要最旧的一半；全量压缩才把整个旧 head 收进一条带 Anchor 的压缩消息。
- Anchor 结构把“用户到底要什么、做到哪了、为什么这么做、踩过什么坑、下一步干什么”固定成五个可检查的锚点。
- `OffloadHook` 和 `CompactionOffloader` 分别守在工具执行后与压缩前，拦住两种体量的大输出。
- 真到危险水位或摘要失败时，`TokenBudgetCompactor` 负责兜底；压缩记录仍会写入 JSONL，方便回头查。

## 本文你将学到

- 你将看清一次压缩为什么发生在每次 LLM 调用之前，而不是历史写入之后。
- 你将理解如何通过四档压缩级别，把“要么不压、要么猛删”的突变改成渐进调节。
- 你将掌握两段式工具结果外存怎样保留可检索的原始证据。
- 你将理解 Anchor 的合并规则为何能减少多次摘要后的任务漂移。
- 你将看清工具调用配对、回退和审计如何共同保证长程任务不断线。

## Context Compaction，到底压的是什么？

`contextHistory` 是 Agent 的完整工作记录。`compactedHistory` 只是这一轮送给模型看的临时视图。我们实际压缩的，是传入模型 Context Window 的消息窗口，而不是原始的对话历史！
这个分离很重要：压缩不把 SQLite 会话历史原地改成摘要，下一轮仍从完整历史派生新视图。

在 `runLoop` 里，harness9 engine 先拿到可用工具及其 schema，再调用 `applyCompactionWith`。随后才把压缩后的消息和工具定义送进 LLM。压缩不是事后清理；它是推理前的准入检查。

看这段真正决定调用顺序的代码：

```go
// internal/engine/agent_loop.go
compactedHistory, compactionRecord := e.applyCompactionWith(comp, contextHistory)
msgTokensAfter := memory.EstimateTokens(compactedHistory)
totalTokens := msgTokensAfter + toolTokens

if compactionRecord != nil && compactionRecord.Tier != memory.TierNone {
    em.compaction(*compactionRecord)
}
em.tokenUpdate(totalTokens, e.contextWindow)
msg, usage, err := e.generateWithRetry(ctx, em, turnCount,
    compactedHistory, availableTools)
```

`EstimateTokens` 用字符数除以 4 做快速估算。它不追求精确，而是足够高效，能在每个 Turn 前运行。工具 schema 的 token 也要单独算进去；这就是为什么不能把历史刚好压到窗口边缘。

![图：压缩在 LLM 调用前发生](/blog/progressive-context-compaction/images/preflight-flow-02.png)


## 何时触发压缩？

`ProgressiveCompactor` 把窗口占用率切成五种结果。`TierNone` 不是“忘了处理”，而是明确选择不动。每一档都比前一档多做一点事。

| 档位 | 占用率 | 实际动作 | LLM 摘要 |
|---|---:|---|---|
| `TierNone` | < 60% | 原样通过 | 否 |
| `TierWarn` | 60%–70% | 仅外存旧的大工具结果 | 否 |
| `TierSoft` | 70%–80% | 摘要最旧半段，保留较新的 head 与 tail | 是 |
| `TierFull` | 80%–95% | 摘要整个 head，只保留最近 tail | 是 |
| `TierEmergency` | ≥ 95% | 直接强制截断 | 否 |

判定函数没有隐藏魔法。它只拿当前估算 token 除以模型 `ContextWindow`，从最危险的档位向下匹配：

```go
// internal/memory/progressive_compactor.go
func (c *ProgressiveCompactor) determineTier(msgs []schema.Message) CompactionTier {
    if c.ContextWindow <= 0 {
        return TierNone
    }
    ratio := float64(EstimateTokens(msgs)) / float64(c.ContextWindow)
    switch {
    case ratio >= c.EmergencyThreshold:
        return TierEmergency
    case ratio >= c.FullThreshold:
        return TierFull
    case ratio >= c.SoftThreshold:
        return TierSoft
    case ratio >= c.WarnThreshold:
        return TierWarn
    default:
        return TierNone
    }
}
```

默认阈值是 `0.60 / 0.70 / 0.80 / 0.95`，最近消息尾部默认保留 6 条。这里的取舍很朴素：越早的档位越不碰语义，越靠后的档位越优先让任务活下来。这样 Agent 不会在 79% 时毫无变化，80% 时突然失忆。

![图：ProgressiveCompactor 的五档水位](/blog/progressive-context-compaction/images/tier-ladder-01.png)


## 如何保证长结果不丢失？

长任务真正危险的，常常不是对话，而是一条 `bash` 输出几万字。harness9 不把它简单截成省略号，而是它把原文存回本地文件，再让上下文留下带预览的可检索引用。

这件事有两道门。

- 工具执行后的 `OffloadHook` 处理超过 10000 字符的输出，避免它刚出生就塞满历史。
- 压缩期的 `CompactionOffloader` 再处理已进入旧 head、超过 4000 字节的中等输出。这正好补住了第一道门没有拦下的 4000–10000 字符区间。

外存路径以会话和工具调用 ID 隔离：`.harness9/tool_results/<sessionID>/<toolCallID>.txt`。文件权限是 `0600`。同一个 `ToolCallID` 还会命中内存缓存，后续压缩只重建占位符，不会反复覆盖文件。

看压缩器怎样只替换可识别、足够大的 `tool_result`：

```go
// internal/memory/progressive_compactor.go
for i, msg := range head {
    if msg.ToolCallID == "" || len(msg.Content) <= c.OffloadThreshold {
        continue
    }
    entry, placeholder, err := c.offloader.OffloadToolResult(msg)
    if err != nil {
        continue
    }
    head[i].Content = placeholder
    entries = append(entries, entry)
}
```

失败就 `continue`，原始消息保留。这是 fail-open：文件写不进去时，Agent 也不该因为“想省 token”而中断。之后模型需要细节，仍可以用 `read_file` 分页取回。

![图：两段式工具结果外存](/blog/progressive-context-compaction/images/two-stage-offload-03.png)


## 为什么 Anchor 非常重要？

自由文本摘要很容易“像是记住了”，却没法验证它真的保住了任务的骨架。`ProgressiveCompactor` 因此要求摘要 LLM 同时输出五类结构化锚点（Anchor）：用户意图（User Intent）、执行进度（Execution Progress）、关键决策（Key Decisions）、尝试方案（Tried Solutions）、下一步（Next Steps）。

`ParseAnchorsAndSummary` 会保证返回的锚点永远是这五项；没提到的项填 `N/A`。下一次压缩时，`MergeAnchors` 用新值覆盖旧值，但 `N/A` 不覆盖旧内容：

```go
// internal/memory/anchor.go
for _, a := range new {
    if a.Content != "N/A" {
        m[a.Type] = a.Content
    }
}
for _, at := range allAnchorTypes {
    content := m[at]
    if content == "" {
        content = "N/A"
    }
    result = append(result, Anchor{Type: at, Content: content})
}
```

这不是为了把摘要写得更像报告。它是在给长任务留五个钉子：项目做到一半时，模型可以忘记措辞，却不能轻易把“用户要解决什么”或“这个坑已经试过”一起冲掉。正文摘要仍然保留细节，Anchor 则负责稳定的骨架。

![图：Anchor 的增量合并](/blog/progressive-context-compaction/images/anchor-merge-04.png)


## 保留完整 Tool-Call 协议

压缩消息还有一个很容易被忽略的 API 约束。Anthropic Messages API 要求 `tool_call` 和 `tool_result` 成对出现。切掉旧消息时，可能只剩结果，也可能只剩调用。两种情况都不是“少一点上下文”这么简单，而是请求直接 400。

`repairOrphanedToolPairs` 在每次压缩结果上做双向修复：无对应调用的结果直接删掉；没有结果的调用补一个“上下文已压缩，结果不可用”的占位结果。`ProgressiveCompactor` 的每个实际 tier 在组装结果后都会过这道修复。

![图：工具调用对的双向修复](/blog/progressive-context-compaction/images/tool-pair-repair-05.png)


## 降级策略

harness9 设计了降级策略，确保即使 ProgressiveCompactor 执行失败，也不影响 agent 的正常执行。`TierSoft` 和 `TierFull` 给摘要调用单独设了 60 秒超时；摘要服务失败就回退给 `TokenBudgetCompactor`。`TierEmergency` 更直接：它根本不调用 LLM，而是优先使用 `CompactForce` 截断。

这个设计放弃了一部分历史语义，换来 Agent 不会因为“用于压缩的 LLM 不可用”而卡死。`CompactionRecord.Error` 会把降级原因留下。只要不是 `TierNone`，记录就会尝试追加到按会话分文件的 JSONL；TUI 也会收到 `EventCompaction`，显示压缩前后 token、档位、外存数量和尾部保留数量。


![图：压缩记录的审计链](/blog/progressive-context-compaction/images/compaction-audit-06.png)


## 结语

Context Compaction 并不是一次简单的 Summarize，而是一个渐进式的分级压缩策略，它是上下文预算与信息精度的权衡。
当上下文窗口余量足够时，我们可以进行宽松的压缩，仅将历史较长的 tool-call 结构写入文件。而当上下文预算紧张时，则要触发更加保守的压缩，仅保留关键信息，而关键信息需要 Anchor 结构来做约束。这就是 harness9 的分层渐进式压缩设计思想。